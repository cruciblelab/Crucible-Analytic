// Package asnlookup resolves an IP to the country it's registered to, by
// downloading and parsing the five Regional Internet Registries' public
// "delegated-extended" stats files - the same standardized, free,
// rate-limit-free bulk report every RIR publishes - on a periodic
// schedule, and answering lookups locally against an in-memory copy
// (mirrored to TimescaleDB for durability across restarts) with no
// per-lookup network or, in the common case, database access at all.
//
// Country only, IPv4 only, this phase:
//
//   - IPv4 only: RIR IPv6 allocation records are skipped by the parser.
//     IPv6 input to Resolve always returns Found: false. Adding IPv6
//     support later is additive (a second rangeTable keyed on the
//     128-bit address, or similar) - it isn't wired up now because it
//     wasn't needed to validate the approach, not because it's hard.
//
//   - Country only, not ASN: delegated-extended files record two
//     independent things - which country an IP range is registered to,
//     and which country an ASN *number* is registered to. Neither
//     record links a specific IP range to the ASN that actually routes
//     it - that mapping is a routing-table (BGP) fact, not a registry
//     fact, and coming from a fundamentally different data source (e.g.
//     a routing-table snapshot like RouteViews/CAIDA's prefix-to-AS
//     tables). Result.ASN and Result.ASNName exist so the shape of a
//     future real ASN lookup doesn't require a breaking change, but
//     they're always the zero value in this phase - never a guess, and
//     never silently borrowed from the ASN-number-to-country records
//     this package does parse (that would be actively misleading: it
//     would attribute an IP to some country-appropriate ASN that may
//     have nothing to do with who actually routes that address).
package asnlookup

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sort"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Result is the outcome of resolving one IP.
type Result struct {
	IP      netip.Addr
	Country string // ISO 3166-1 alpha-2, e.g. "US" - "" if Found is false.
	ASN     int    // Always 0 this phase - see the package doc comment.
	ASNName string // Always "" this phase - see the package doc comment.
	Found   bool   // True if IP fell inside a known RIR-registered range.
}

// rirSource is one RIR's delegated-extended stats endpoint. All five
// publish the same file format at a well-known, stable URL.
type rirSource struct {
	name string
	url  string
}

var rirSources = []rirSource{
	{"arin", "https://ftp.arin.net/pub/stats/arin/delegated-arin-extended-latest"},
	{"ripencc", "https://ftp.ripe.net/ripe/stats/delegated-ripencc-extended-latest"},
	{"apnic", "https://ftp.apnic.net/apnic/stats/apnic/delegated-apnic-extended-latest"},
	{"lacnic", "https://ftp.lacnic.net/pub/stats/lacnic/delegated-lacnic-extended-latest"},
	{"afrinic", "https://ftp.afrinic.net/pub/stats/afrinic/delegated-afrinic-extended-latest"},
}

// CacheConfig sizes the in-memory result cache sitting in front of the
// range table. Both fields must be positive - see config validation.
type CacheConfig struct {
	MaxEntries int
	TTL        time.Duration
}

// Resolver resolves IPs to countries. Safe for concurrent use.
type Resolver struct {
	pool       *pgxpool.Pool
	httpClient *http.Client
	cache      *ttlCache
	table      atomic.Pointer[rangeTable]
	logger     *slog.Logger
}

// NewResolver opens a connection pool to databaseURL and verifies it's
// reachable, exactly like storage.NewWriter - it does not create or
// migrate the ip_country_ranges table. Apply asnlookup/schema.sql once,
// manually, before setting asn_lookup.enabled = true (see the README);
// like the rest of this collector, it never runs DDL itself. If that
// table doesn't exist yet, Resolver still starts up fine - Run's
// refreshes will simply fail (logged, not fatal) and every Resolve stays
// Found: false until the schema is applied and a refresh succeeds.
func NewResolver(ctx context.Context, databaseURL string, cache CacheConfig, logger *slog.Logger) (*Resolver, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("asnlookup: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("asnlookup: ping database: %w", err)
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Resolver{
		pool:       pool,
		httpClient: &http.Client{Timeout: 2 * time.Minute},
		cache:      newTTLCache(cache.MaxEntries, cache.TTL),
		logger:     logger,
	}, nil
}

// Close releases the connection pool. Safe to call once.
func (r *Resolver) Close() {
	r.pool.Close()
}

// Resolve answers one lookup: cache first, then the in-memory range
// table. It never blocks on network or database I/O - the only things
// that touch either are Run's periodic refreshes, entirely off this path.
func (r *Resolver) Resolve(ip netip.Addr) Result {
	ip = ip.Unmap()
	if cached, ok := r.cache.get(ip); ok {
		return cached
	}

	result := Result{IP: ip}
	if ipInt, ok := ipv4ToUint32(ip); ok {
		if country, found := r.table.Load().lookup(ipInt); found {
			result.Country = country
			result.Found = true
		}
	}
	r.cache.set(ip, result)
	return result
}

// Run performs an immediate refresh, then repeats every refreshInterval
// until ctx is cancelled. The immediate first refresh is a deliberate
// difference from storage.Flusher's ticker (which is fine to let wait out
// its first full interval, since "nothing flushed yet" is a harmless
// startup state): here, an unrefreshed table isn't neutral - every
// Resolve would silently report Found: false for up to a full
// refreshInterval after every process start otherwise.
func (r *Resolver) Run(ctx context.Context, refreshInterval time.Duration) {
	r.refresh(ctx)

	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refresh(ctx)
		}
	}
}

// refresh downloads and parses all five RIR sources, merges what
// succeeded, persists it to TimescaleDB, and - only once that succeeds -
// atomically swaps in the new table. A source that fails to download or
// parse is logged and skipped rather than aborting the whole refresh
// (partial, mostly-fresh data beats none); if every source fails, or the
// database write fails, the previous table (if any) is left in place
// rather than replaced with something empty or partial.
func (r *Resolver) refresh(ctx context.Context) {
	var all []rangeEntry
	for _, src := range rirSources {
		entries, err := r.fetchAndParse(ctx, src.url)
		if err != nil {
			r.logger.Warn("asnlookup: source refresh failed, continuing with the rest", "source", src.name, "err", err)
			continue
		}
		all = append(all, entries...)
	}
	if len(all) == 0 {
		r.logger.Warn("asnlookup: refresh produced no usable data from any source, keeping previous table")
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].start < all[j].start })

	if err := r.writeRanges(ctx, all); err != nil {
		r.logger.Error("asnlookup: failed to persist ranges, keeping previous table", "err", err)
		return
	}
	r.table.Store(newRangeTable(all))
	r.logger.Info("asnlookup: refresh complete", "ranges", len(all))
}

func (r *Resolver) fetchAndParse(ctx context.Context, url string) ([]rangeEntry, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return parseDelegatedStats(resp.Body)
}

// writeRanges replaces the entire ip_country_ranges table in one
// transaction. A full replace rather than an incremental diff: refreshes
// are weekly by default and the whole dataset is a few hundred thousand
// rows at most, so the "recompute everything, every time" simplicity this
// project has favored elsewhere (e.g. skipping full-mode's overlap/CIDR
// bookkeeping the reference architecture used) is a good trade here too.
func (r *Resolver) writeRanges(ctx context.Context, entries []rangeEntry) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once Commit has succeeded

	if _, err := tx.Exec(ctx, "TRUNCATE ip_country_ranges"); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"ip_country_ranges"}, []string{"start_addr", "end_addr", "country"},
		pgx.CopyFromSlice(len(entries), func(i int) ([]any, error) {
			e := entries[i]
			return []any{int64(e.start), int64(e.end), e.country}, nil
		}),
	)
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return tx.Commit(ctx)
}
