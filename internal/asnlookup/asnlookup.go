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

	"github.com/cruciblelab/crucible-analytic/internal/ipsources"
)

// Result is the outcome of resolving one IP.
type Result struct {
	IP      netip.Addr
	Country string // ISO 3166-1 alpha-2, e.g. "US" - "" if not found in the country dataset.
	ASN     int    // e.g. 15169 - 0 if not found in the ASN dataset.
	ASNName string // e.g. "GOOGLE" - "" if not found in the ASN dataset.
	Found   bool   // True if IP was found in the country dataset, the ASN dataset, or both.
}

// The URLs and filenames moved to sources.go, which holds them once per
// dataset alongside the licence and the reason to choose it. They were
// four constants describing one provider; M1 made them a library because
// "which source" became a question a deployment can answer.

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
	// CountryOnly stops the ASN dataset being fetched, read, parsed or
	// stored at all. Country lookups are unaffected; Resolve simply
	// never finds an ASN, because there is nothing to find it in.
	//
	// # What it buys
	//
	// The two ASN files are the larger half of this module's memory. A
	// resolver holds four range tables, and in country-only mode two of
	// them stay nil - see TestCountryOnlyNeverReadsTheASNDataset for the
	// measurement.
	//
	// # Why "never read" and not "left empty"
	//
	// Emptying the tables afterwards would save the memory the tables
	// occupy and none of the memory the parse needs, which is the
	// larger figure and the one that decides whether a small VDS
	// survives a refresh: the whole file is read and turned into a slice
	// of entries before anything is swapped in. So this is checked at
	// the top of refresh, before the fetch, and the test proves the file
	// is never opened rather than that the table came out empty.
	//
	// Set from asn_lookup.country_only. That setting refuses to coexist
	// with apply_to_scoring, blocked_asns or known_bot_asns - each of
	// those is an explicit request for something this mode cannot
	// answer, and accepting it would leave a configured control
	// silently doing nothing.
	CountryOnly bool
	// chosen is which datasets to fetch, swapped whole by SetSources.
	//
	// An atomic pointer to an immutable value rather than three fields,
	// and the reason is that Run refreshes on its own goroutine while
	// the collector's settings loop writes on another. Three plain
	// fields would be three races, and the shape they would produce is
	// the worst kind: a refresh that read the new country id and the old
	// fallback list, which is a combination nobody configured.
	chosen atomic.Pointer[chosenSources]
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
	r.sweep(ctx)
	// And once before the first tick, for the same reason the refresh
	// above is immediate: a service that has just started and finds a
	// request waiting should answer it now rather than in thirty
	// seconds. A customer who pressed the button and then restarted the
	// collector - which is what somebody does when they think nothing is
	// happening - would otherwise wait out the poll for no reason.
	r.answerRequests(ctx)

	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	// A second, much shorter ticker for the button (M3).
	//
	// Separate rather than one fast ticker doing both: a refresh
	// downloads about 124 MB from a third party, and running it every
	// thirty seconds to notice a button press would be a denial of
	// service aimed at people who publish the data for free. Polling an
	// indexed table costs nothing worth measuring. internal/asnlookup's
	// onrequest.go says the rest.
	requests := time.NewTicker(requestPollInterval)
	defer requests.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refresh(ctx)
			r.sweep(ctx)
		case <-requests.C:
			r.answerRequests(ctx)
		}
	}
}

// sweep trims the fetch log.
//
// Here rather than in the panel's housekeeping, and PurgeOldFetches says
// why at length. After the refresh rather than before it, so a run that
// has just written rows is the one that also removes the old ones - a
// sweep that ran first would leave every refresh's own rows one tick
// longer than the retention says.
//
// Failure is logged and nothing else. A fetch log that cannot be trimmed
// is a table growing slowly; a refresh that stopped because of it would
// be geography data going stale, which is the thing this whole package
// is for.
func (r *Resolver) sweep(ctx context.Context) {
	removed, err := r.PurgeOldFetches(ctx)
	if err != nil {
		r.logger().Warn("asnlookup: could not trim the fetch log", "err", err)
		return
	}
	if removed > 0 {
		// Only when something went. A line saying "removed 0" on every
		// tick is how a log stops being read.
		r.logger().Info("asnlookup: trimmed the fetch log", "removed", removed)
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
	// Before the fetch, not after the parse. See CountryOnly: what this
	// mode is for is the memory the parse needs, not the memory the
	// table keeps.
	if r.CountryOnly {
		return
	}
	r.refreshASN(ctx)
}

// CountrySource and ASNSource name the datasets this resolver fetches,
// and Fallbacks are tried in order when the chosen one fails.
//
// Empty means the library's default, which is what every installation
// made before M1 is already downloading. That equivalence is the phase's
// first done criterion and TestAnUntouchedResolverFetchesTodaysDatasets
// is what holds it: a deployment that has chosen nothing must not start
// fetching something else because the library grew.
// chosenSources is one consistent answer to "what should this refresh
// fetch".
type chosenSources struct {
	country   string
	asn       string
	fallbacks []string
}

// SetSources tells a running resolver which datasets to use from the
// next refresh onwards.
//
// Takes effect at the next refresh rather than immediately, and that is
// the honest behaviour rather than a shortcut: a refresh in flight is
// downloading tens of megabytes, and abandoning it to start again would
// turn every settings save into a re-download. The panel's help says the
// change applies at the next refresh.
func (r *Resolver) SetSources(country, asn string, fallbacks []string) {
	next := &chosenSources{country: country, asn: asn}
	next.fallbacks = append(next.fallbacks, fallbacks...)
	r.chosen.Store(next)
}

// sources returns what this refresh should fetch.
func (r *Resolver) sources() chosenSources {
	if c := r.chosen.Load(); c != nil {
		return *c
	}
	return chosenSources{}
}

func (r *Resolver) countrySource() ipsources.Source {
	return r.sourceOr(r.sources().country, ipsources.DefaultCountry)
}

func (r *Resolver) asnSource() ipsources.Source {
	return r.sourceOr(r.sources().asn, ipsources.DefaultASN)
}

// sourceOr resolves an id, falling back to the default when it is empty
// or unknown.
//
// Unknown rather than refusing, and it is deliberate: the id comes from
// a settings row, and a row naming a source this build does not carry is
// exactly what an operator sees after rolling a binary back. Refusing
// would turn a stale setting into no country data at all; falling back
// keeps the deployment working and says so once.
func (r *Resolver) sourceOr(id, fallback string) ipsources.Source {
	if id != "" {
		if src, ok := ipsources.ByID(id); ok {
			return src
		}
		r.logger().Warn("asnlookup: this build does not carry the configured dataset, "+
			"using the default", "configured", id, "using", fallback)
	}
	src, _ := ipsources.ByID(fallback)
	return src
}

// chain is the chosen source followed by the configured fallbacks, with
// anything of the wrong kind or unknown dropped.
//
// Dropped rather than refused for the same reason sourceOr falls back:
// the list is a settings row and half of a usable list is better than
// none. Each drop is logged with its reason, so "why is my fallback not
// being used" has an answer in the log rather than in the source.
func (r *Resolver) chain(first ipsources.Source, kind ipsources.SourceKind) []ipsources.Source {
	out := []ipsources.Source{first}
	seen := map[string]bool{first.ID: true}
	for _, id := range r.sources().fallbacks {
		if seen[id] {
			continue
		}
		src, ok := ipsources.ByID(id)
		if !ok {
			r.logger().Warn("asnlookup: fallback names a dataset this build does not carry",
				"id", id)
			continue
		}
		if src.Kind != kind {
			// A country list naming an ASN dataset is a real mistake and
			// silently skipping it would leave the operator believing
			// they had a fallback.
			continue
		}
		seen[id] = true
		out = append(out, src)
	}
	return out
}

func (r *Resolver) refreshCountry(ctx context.Context) {
	chain := r.chain(r.countrySource(), ipsources.KindCountry)
	var (
		entries4 []rangeEntry[string]
		entries6 []rangeEntry[string]
		err4     error
		err6     error
	)
	for _, src := range chain {
		var bytes4, bytes6 int64
		began4 := time.Now()
		entries4, bytes4, err4 = r.loadCountryCSV(ctx, src.IPv4URL, src.IPv4File)
		began6 := time.Now()
		entries6, bytes6, err6 = r.loadCountryCSV(ctx, src.IPv6URL, src.IPv6File)
		done := time.Now()

		// One row per file, written whichever way it went. A failed
		// attempt followed by a successful one is how the fallback shows
		// up in the log: both rows are here, in order, saying which
		// dataset was asked first.
		r.recordFetch(ctx, r.newFetchRecord(src, familyIPv4, began4, began6, len(entries4), bytes4, err4))
		r.recordFetch(ctx, r.newFetchRecord(src, familyIPv6, began6, done, len(entries6), bytes6, err6))

		if err4 == nil || err6 == nil {
			if src.ID != chain[0].ID {
				r.logger().Warn("asnlookup: country dataset came from a fallback",
					"chosen", chain[0].ID, "used", src.ID)
			}
			break
		}
		if len(chain) > 1 {
			r.logger().Warn("asnlookup: country dataset failed, trying the next",
				"source", src.ID, "ipv4_err", err4, "ipv6_err", err6)
		}
	}
	r.storeCountry(ctx, entries4, entries6, err4, err6)
}

func (r *Resolver) storeCountry(ctx context.Context, entries4, entries6 []rangeEntry[string], err4, err6 error) {
	if err4 != nil {
		r.logger().Warn("asnlookup: country ipv4 refresh failed, keeping previous table", "err", err4)
	} else {
		r.countryTable4.Store(newRangeTable(entries4))
	}

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
	chain := r.chain(r.asnSource(), ipsources.KindASN)
	var (
		entries4 []rangeEntry[asnInfo]
		entries6 []rangeEntry[asnInfo]
		err4     error
		err6     error
	)
	for _, src := range chain {
		var bytes4, bytes6 int64
		began4 := time.Now()
		entries4, bytes4, err4 = r.loadASNCSV(ctx, src.IPv4URL, src.IPv4File)
		began6 := time.Now()
		entries6, bytes6, err6 = r.loadASNCSV(ctx, src.IPv6URL, src.IPv6File)
		done := time.Now()

		r.recordFetch(ctx, r.newFetchRecord(src, familyIPv4, began4, began6, len(entries4), bytes4, err4))
		r.recordFetch(ctx, r.newFetchRecord(src, familyIPv6, began6, done, len(entries6), bytes6, err6))

		if err4 == nil || err6 == nil {
			if src.ID != chain[0].ID {
				r.logger().Warn("asnlookup: asn dataset came from a fallback",
					"chosen", chain[0].ID, "used", src.ID)
			}
			break
		}
		if len(chain) > 1 {
			r.logger().Warn("asnlookup: asn dataset failed, trying the next",
				"source", src.ID, "ipv4_err", err4, "ipv6_err", err6)
		}
	}
	r.storeASN(ctx, entries4, entries6, err4, err6)
}

func (r *Resolver) storeASN(ctx context.Context, entries4, entries6 []rangeEntry[asnInfo], err4, err6 error) {
	if err4 != nil {
		r.logger().Warn("asnlookup: asn ipv4 refresh failed, keeping previous table", "err", err4)
	} else {
		r.asnTable4.Store(newRangeTable(entries4))
	}

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

// countingReader counts what passed through it.
//
// The byte count is one of the two numbers M2 promises, and it has to be
// measured here rather than taken from Content-Length: a header is what
// the server said it would send, and the interesting failure is the one
// where it sent less. A truncated file parses without error - both
// parsers stop at a malformed record and keep what they read - so the
// only evidence that half of it never arrived is the count.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// loadCountryCSV returns the parsed ranges and how many bytes were read
// to get them.
//
// Bytes are returned even on a parse failure, because zero bytes and
// nine thousand bytes are different problems: the first is a URL that
// moved, the second is a format that changed.
func (r *Resolver) loadCountryCSV(ctx context.Context, url, localFilename string) ([]rangeEntry[string], int64, error) {
	body, err := r.fetchOrReadLocal(ctx, url, localFilename)
	if err != nil {
		return nil, 0, err
	}
	defer body.Close()
	counted := &countingReader{r: body}
	entries, err := parseCountryCSV(counted)
	return entries, counted.n, err
}

func (r *Resolver) loadASNCSV(ctx context.Context, url, localFilename string) ([]rangeEntry[asnInfo], int64, error) {
	body, err := r.fetchOrReadLocal(ctx, url, localFilename)
	if err != nil {
		return nil, 0, err
	}
	defer body.Close()
	counted := &countingReader{r: body}
	entries, err := parseASNCSV(counted)
	return entries, counted.n, err
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
