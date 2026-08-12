// Package asnlookup resolves an IP to the country it's registered to and
// the ASN that routes it, using two sapics/ip-location-db datasets - free,
// Public-Domain-licensed (PDDL, no attribution required), daily-updated
// CSVs compiled from RIR delegated-stats, BGP routing archives (RouteViews
// / RIPE RIS), and RFC 8805/9632 geofeeds:
//
//   - user-country: IP range -> country code.
//   - origin-asn: IP range -> ASN number + organization name.
//
// See the README for the full courtesy attribution.
//
// The two datasets are independently sourced, with their own unrelated
// range boundaries, so they're kept as four separate range tables
// (country x {v4,v6}, ASN x {v4,v6}) rather than merged into one -
// merging would need the same overlap-resolution complexity this package
// has deliberately stayed away from. Each dataset is fetched from GitHub
// Releases (or read from a local directory - see Config.LocalCSVPath) on
// a periodic schedule, and lookups are answered locally against an
// in-memory copy (mirrored to TimescaleDB for durability across restarts)
// with no per-lookup network or, in the common case, database access at
// all.
package asnlookup

import (
	"context"
	"fmt"
	"io"
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
	Country string // ISO 3166-1 alpha-2, e.g. "US" - "" if not found in the country dataset.
	ASN     int    // e.g. 15169 - 0 if not found in the ASN dataset.
	ASNName string // e.g. "GOOGLE" - "" if not found in the ASN dataset.
	Found   bool   // True if IP was found in the country dataset, the ASN dataset, or both.
}

const (
	countryIPv4URL      = "https://github.com/sapics/ip-location-db/releases/download/latest/user-country-ipv4.csv"
	countryIPv6URL      = "https://github.com/sapics/ip-location-db/releases/download/latest/user-country-ipv6.csv"
	countryIPv4Filename = "user-country-ipv4.csv"
	countryIPv6Filename = "user-country-ipv6.csv"

	asnIPv4URL      = "https://github.com/sapics/ip-location-db/releases/download/latest/origin-asn-ipv4.csv"
	asnIPv6URL      = "https://github.com/sapics/ip-location-db/releases/download/latest/origin-asn-ipv6.csv"
	asnIPv4Filename = "origin-asn-ipv4.csv"
	asnIPv6Filename = "origin-asn-ipv6.csv"
)

// CacheConfig sizes the in-memory result cache sitting in front of the
// range tables. Both fields must be positive - see config validation.
type CacheConfig struct {
	MaxEntries int
	TTL        time.Duration
}

// Resolver resolves IPs to countries and ASNs. Safe for concurrent use.
type Resolver struct {
	pool       *pgxpool.Pool
	httpClient *http.Client
	cache      *ttlCache

	countryTable4 atomic.Pointer[rangeTable[string]]
	countryTable6 atomic.Pointer[rangeTable[string]]
	asnTable4     atomic.Pointer[rangeTable[asnInfo]]
	asnTable6     atomic.Pointer[rangeTable[asnInfo]]

	// localCSVPath, if non-empty, skips downloading entirely: refresh
	// reads user-country-ipv4/6.csv and origin-asn-ipv4/6.csv from this
	// local directory instead, with no network access of any kind. Empty
	// means download from GitHub Releases, as normal.
	localCSVPath string
	// SkipRangePersistence stops refresh from writing the datasets back
	// to ip_country_ranges/ip_asn_ranges. In-memory lookups are
	// unaffected; only the persisted copy is skipped.
	//
	// This exists because writeCountryRanges/writeASNRanges TRUNCATE
	// those tables before repopulating them, which is correct for one
	// writer and actively destructive for two: a second process
	// refreshing on its own schedule would repeatedly blow away the
	// first one's rows, and both would see the table empty for the
	// duration of the other's load. Any process that resolves IPs
	// against a database some *other* process already refreshes - the
	// beacon alongside a collector, for instance - must set this.
	SkipRangePersistence bool
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
// migrate the ip_country_ranges/ip_asn_ranges tables. Apply
// asnlookup/schema.sql once, manually, before setting
// asn_lookup.enabled = true (see the README); like the rest of this
// collector, it never runs DDL itself. If those tables don't exist yet,
// Resolver still starts up fine - Run's refreshes will simply fail
// (logged, not fatal) and every Resolve stays Found: false until the
// schema is applied and a refresh succeeds.
//
// localCSVPath, if non-empty, makes every refresh read the dataset files
// from local disk instead of downloading from GitHub Releases - no
// network call is made for either dataset in that mode. The database
// connection above still applies either way; it's for durability, not
// for fetching the datasets.
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
// tables for ip's address family - country and ASN independently, since
// they're independent datasets (an IP can be found in one and not the
// other). It never blocks on network or database I/O - the only things
// that touch either are Run's periodic refreshes, entirely off this path.
func (r *Resolver) Resolve(ip netip.Addr) Result {
	ip = ip.Unmap()
	if cached, ok := r.cache.get(ip); ok {
		return cached
	}

	result := Result{IP: ip}
	var countryTable *rangeTable[string]
	var asnTable *rangeTable[asnInfo]
	switch {
	case ip.Is4():
		countryTable, asnTable = r.countryTable4.Load(), r.asnTable4.Load()
	case ip.Is6():
		countryTable, asnTable = r.countryTable6.Load(), r.asnTable6.Load()
	}

	if country, found := countryTable.lookup(ip); found {
		result.Country = country
		result.Found = true
	}
	if info, found := asnTable.lookup(ip); found {
		result.ASN = info.asn
		result.ASNName = info.org
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

// refresh loads both datasets, both address families each (from GitHub
// Releases or a local directory - see fetchOrReadLocal), persisting
// whatever succeeded to TimescaleDB and atomically swapping in each
// family's new table independently. A family that fails to load or parse
// is logged and its previous table (if any) is left in place rather than
// replaced with something empty; everything else still updates normally.
// The country and ASN datasets are entirely independent of each other -
// one failing has no effect on the other.
func (r *Resolver) refresh(ctx context.Context) {
	r.refreshCountry(ctx)
	r.refreshASN(ctx)
}

func (r *Resolver) refreshCountry(ctx context.Context) {
	entries4, err4 := r.loadCountryCSV(ctx, countryIPv4URL, countryIPv4Filename)
	if err4 != nil {
		r.logger().Warn("asnlookup: country ipv4 refresh failed, keeping previous table", "err", err4)
	} else {
		r.countryTable4.Store(newRangeTable(entries4))
	}

	entries6, err6 := r.loadCountryCSV(ctx, countryIPv6URL, countryIPv6Filename)
	if err6 != nil {
		r.logger().Warn("asnlookup: country ipv6 refresh failed, keeping previous table", "err", err6)
	} else {
		r.countryTable6.Store(newRangeTable(entries6))
	}

	if err4 != nil && err6 != nil {
		return // nothing new to persist
	}
	if r.SkipRangePersistence {
		r.logger().Info("asnlookup: country refresh complete (in-memory only)", "ipv4_ranges", len(entries4), "ipv6_ranges", len(entries6))
		return
	}
	if err := r.writeCountryRanges(ctx, append(entries4, entries6...)); err != nil {
		r.logger().Error("asnlookup: failed to persist country ranges, in-memory tables still updated", "err", err)
		return
	}
	r.logger().Info("asnlookup: country refresh complete", "ipv4_ranges", len(entries4), "ipv6_ranges", len(entries6))
}

func (r *Resolver) refreshASN(ctx context.Context) {
	entries4, err4 := r.loadASNCSV(ctx, asnIPv4URL, asnIPv4Filename)
	if err4 != nil {
		r.logger().Warn("asnlookup: asn ipv4 refresh failed, keeping previous table", "err", err4)
	} else {
		r.asnTable4.Store(newRangeTable(entries4))
	}

	entries6, err6 := r.loadASNCSV(ctx, asnIPv6URL, asnIPv6Filename)
	if err6 != nil {
		r.logger().Warn("asnlookup: asn ipv6 refresh failed, keeping previous table", "err", err6)
	} else {
		r.asnTable6.Store(newRangeTable(entries6))
	}

	if err4 != nil && err6 != nil {
		return
	}
	if r.SkipRangePersistence {
		r.logger().Info("asnlookup: asn refresh complete (in-memory only)", "ipv4_ranges", len(entries4), "ipv6_ranges", len(entries6))
		return
	}
	if err := r.writeASNRanges(ctx, append(entries4, entries6...)); err != nil {
		r.logger().Error("asnlookup: failed to persist asn ranges, in-memory tables still updated", "err", err)
		return
	}
	r.logger().Info("asnlookup: asn refresh complete", "ipv4_ranges", len(entries4), "ipv6_ranges", len(entries6))
}

// fetchOrReadLocal returns the raw dataset file contents, either by
// downloading url or, if r.localCSVPath is set, by reading localFilename
// from that directory instead - no network access happens in the latter
// case. Shared by loadCountryCSV and loadASNCSV below; the caller is
// responsible for closing the returned reader.
func (r *Resolver) fetchOrReadLocal(ctx context.Context, url, localFilename string) (io.ReadCloser, error) {
	if r.localCSVPath != "" {
		path := filepath.Join(r.localCSVPath, localFilename)
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open local file %s: %w", path, err)
		}
		return f, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

func (r *Resolver) loadCountryCSV(ctx context.Context, url, localFilename string) ([]rangeEntry[string], error) {
	body, err := r.fetchOrReadLocal(ctx, url, localFilename)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return parseCountryCSV(body)
}

func (r *Resolver) loadASNCSV(ctx context.Context, url, localFilename string) ([]rangeEntry[asnInfo], error) {
	body, err := r.fetchOrReadLocal(ctx, url, localFilename)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return parseASNCSV(body)
}

// writeCountryRanges replaces the entire ip_country_ranges table in one
// transaction. A full replace rather than an incremental diff: refreshes
// are weekly by default and the whole dataset is at most a few million
// rows, so the "recompute everything, every time" simplicity this
// project has favored elsewhere is a good trade here too.
func (r *Resolver) writeCountryRanges(ctx context.Context, entries []rangeEntry[string]) error {
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
			return []any{e.start, e.end, e.value}, nil
		}),
	)
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return tx.Commit(ctx)
}

// writeASNRanges replaces the entire ip_asn_ranges table in one
// transaction, the same full-replace approach as writeCountryRanges.
func (r *Resolver) writeASNRanges(ctx context.Context, entries []rangeEntry[asnInfo]) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "TRUNCATE ip_asn_ranges"); err != nil {
		return fmt.Errorf("truncate: %w", err)
	}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"ip_asn_ranges"}, []string{"start_addr", "end_addr", "asn", "asn_org"},
		pgx.CopyFromSlice(len(entries), func(i int) ([]any, error) {
			e := entries[i]
			return []any{e.start, e.end, e.value.asn, e.value.org}, nil
		}),
	)
	if err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return tx.Commit(ctx)
}
