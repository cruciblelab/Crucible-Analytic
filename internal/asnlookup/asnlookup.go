// Package asnlookup resolves an IP to the country it's registered to,
// using sapics/ip-location-db's "user-country" dataset - a free,
// Public-Domain-licensed (PDDL, no attribution required), daily-updated
// CSV compiled from RIR delegated-stats, BGP routing archives (RouteViews
// / RIPE RIS), and RFC 8805/9632 geofeeds. See the README for the full
// courtesy attribution.
//
// The dataset is fetched from GitHub Releases (or read from a local
// directory - see Config.LocalCSVPath) on a periodic schedule, and
// lookups are answered locally against an in-memory copy (mirrored to
// TimescaleDB for durability across restarts) with no per-lookup network
// or, in the common case, database access at all.
//
// Country only, no ASN, this phase: the "user-country" dataset carries
// only country codes. A real ASN number/organization-name lookup needs a
// different sapics/ip-location-db dataset (origin-asn) and is explicitly
// out of scope for this phase - Result.ASN and Result.ASNName exist so
// adding that later is a config change, not a breaking one, but they're
// always the zero value right now rather than a guess.
package asnlookup

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
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
	Found   bool   // True if IP fell inside a known registered range.
}

const (
	countryIPv4URL      = "https://github.com/sapics/ip-location-db/releases/download/latest/user-country-ipv4.csv"
	countryIPv6URL      = "https://github.com/sapics/ip-location-db/releases/download/latest/user-country-ipv6.csv"
	countryIPv4Filename = "user-country-ipv4.csv"
	countryIPv6Filename = "user-country-ipv6.csv"
)

// CacheConfig sizes the in-memory result cache sitting in front of the
// range tables. Both fields must be positive - see config validation.
type CacheConfig struct {
	MaxEntries int
	TTL        time.Duration
}

// Resolver resolves IPs to countries. Safe for concurrent use.
type Resolver struct {
	pool       *pgxpool.Pool
	httpClient *http.Client
	cache      *ttlCache
	table4     atomic.Pointer[rangeTable]
	table6     atomic.Pointer[rangeTable]
	// localCSVPath, if non-empty, skips downloading entirely: refresh
	// reads <localCSVPath>/user-country-ipv4.csv and
	// -ipv6.csv from local disk instead, with no network access of any
	// kind. Empty means download from GitHub Releases, as normal.
	localCSVPath string
	// Logger defaults to slog.Default() when nil - see the logger()
	// accessor - so a Resolver value built directly (bypassing
	// NewResolver, as some tests do) is never at risk of a nil-pointer
	// panic the moment something needs logging.
	Logger *slog.Logger
}

// logger returns r.Logger, falling back to slog.Default() if it was
// never set - the same defensive pattern proxy.Server and fullproxy.Server
// use.
func (r *Resolver) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

// NewResolver opens a connection pool to databaseURL and verifies it's
// reachable, exactly like storage.NewWriter - it does not create or
// migrate the ip_country_ranges table. Apply asnlookup/schema.sql once,
// manually, before setting asn_lookup.enabled = true (see the README);
// like the rest of this collector, it never runs DDL itself. If that
// table doesn't exist yet, Resolver still starts up fine - Run's
// refreshes will simply fail (logged, not fatal) and every Resolve stays
// Found: false until the schema is applied and a refresh succeeds.
//
// localCSVPath, if non-empty, makes every refresh read from
// <localCSVPath>/user-country-ipv4.csv and -ipv6.csv on local disk
// instead of downloading from GitHub Releases - no network call is made
// for the dataset in that mode, ever. The database connection above still
// applies either way; it's for durability, not for fetching the dataset.
func NewResolver(ctx context.Context, databaseURL string, cache CacheConfig, localCSVPath string, logger *slog.Logger) (*Resolver, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("asnlookup: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("asnlookup: ping database: %w", err)
	}
	return &Resolver{
		pool:         pool,
		httpClient:   &http.Client{Timeout: 2 * time.Minute},
		cache:        newTTLCache(cache.MaxEntries, cache.TTL),
		localCSVPath: localCSVPath,
		Logger:       logger,
	}, nil
}

// Close releases the connection pool. Safe to call once.
func (r *Resolver) Close() {
	r.pool.Close()
}

// Resolve answers one lookup: cache first, then the in-memory range
// table for ip's address family. It never blocks on network or database
// I/O - the only things that touch either are Run's periodic refreshes,
// entirely off this path.
func (r *Resolver) Resolve(ip netip.Addr) Result {
	ip = ip.Unmap()
	if cached, ok := r.cache.get(ip); ok {
		return cached
	}

	result := Result{IP: ip}
	var table *rangeTable
	switch {
	case ip.Is4():
		table = r.table4.Load()
	case ip.Is6():
		table = r.table6.Load()
	}
	if country, found := table.lookup(ip); found {
		result.Country = country
		result.Found = true
	}
	r.cache.set(ip, result)
	return result
}

// Run performs an immediate refresh, then repeats every refreshInterval
// until ctx is cancelled. The immediate first refresh is a deliberate
// difference from storage.Flusher's ticker (which is fine to let wait out
// its first full interval, since "nothing flushed yet" is a harmless
// startup state): here, unrefreshed tables aren't neutral - every
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

// refresh loads both address families (from GitHub Releases or a local
// directory - see loadCountryCSV), persists whatever succeeded to
// TimescaleDB, and atomically swaps in each family's new table
// independently. A family that fails to load or parse is logged and
// its previous table (if any) is left in place rather than replaced with
// something empty; the other family still updates normally.
func (r *Resolver) refresh(ctx context.Context) {
	entries4, err4 := r.loadCountryCSV(ctx, countryIPv4URL, countryIPv4Filename)
	if err4 != nil {
		r.logger().Warn("asnlookup: ipv4 refresh failed, keeping previous table", "err", err4)
	} else {
		r.table4.Store(newRangeTable(entries4))
	}

	entries6, err6 := r.loadCountryCSV(ctx, countryIPv6URL, countryIPv6Filename)
	if err6 != nil {
		r.logger().Warn("asnlookup: ipv6 refresh failed, keeping previous table", "err", err6)
	} else {
		r.table6.Store(newRangeTable(entries6))
	}

	if err4 != nil && err6 != nil {
		return // nothing new to persist
	}
	all := append(entries4, entries6...)
	if err := r.writeRanges(ctx, all); err != nil {
		r.logger().Error("asnlookup: failed to persist ranges, in-memory tables still updated", "err", err)
		return
	}
	r.logger().Info("asnlookup: refresh complete", "ipv4_ranges", len(entries4), "ipv6_ranges", len(entries6))
}

// loadCountryCSV returns the parsed ranges for one address family, either
// by downloading url or, if r.localCSVPath is set, by reading
// localFilename from that directory instead - no network access happens
// in the latter case.
func (r *Resolver) loadCountryCSV(ctx context.Context, url, localFilename string) ([]rangeEntry, error) {
	if r.localCSVPath != "" {
		path := filepath.Join(r.localCSVPath, localFilename)
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open local file %s: %w", path, err)
		}
		defer f.Close()
		return parseCountryCSV(f)
	}

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
	return parseCountryCSV(resp.Body)
}

// writeRanges replaces the entire ip_country_ranges table in one
// transaction. A full replace rather than an incremental diff: refreshes
// are weekly by default and the whole dataset is at most a few million
// rows, so the "recompute everything, every time" simplicity this
// project has favored elsewhere is a good trade here too.
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
			return []any{e.start, e.end, e.country}, nil
		}),
	)
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return tx.Commit(ctx)
}
