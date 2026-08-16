// Package settings lets a running service read the operational values
// the panel writes, without a restart and without a config file edit.
//
// It is the other half of internal/panel's settings table. The panel
// validates and writes; this reads, caches, and hands a service values
// it can swap in while serving. Between them they are what makes "fix it
// from the panel" mean something other than "edit a file over SSH".
//
// # Why a service may read this table at all
//
// Every service in this project runs with a database role scoped to what
// it does: the collector writes traffic_snapshots, the beacon inserts
// into beacon_events and nothing else, the read API cannot write at all.
// Reading settings needs one more grant:
//
//	GRANT SELECT ON panel_settings TO beacon_writer;
//
// That is a deliberate, minimal widening. It is one table, read-only,
// containing no personal data and no credentials - it does not let the
// beacon read analytics, users, sessions or tokens. The alternative
// designs are worse: fetching over HTTP would add an authentication
// surface to a process the whole internet can already reach, and having
// the panel write a snapshot file would add a synchronisation problem
// with no compensating benefit.
//
// # The failure mode, decided rather than discovered
//
// When the read fails, the last known values are kept. Not the defaults.
// A service that reset a customer's tuning to defaults during a database
// blip would be worse than one running on a stale value, and far harder
// to notice - the numbers would simply start being different, with
// nothing in the logs to say why.
package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultInterval is how often a service re-reads. A minute is short
// enough that a support call does not wait on it and long enough that
// four services polling cost nothing measurable.
const DefaultInterval = time.Minute

// Config configures a Source.
type Config struct {
	// Interval between reads. Zero means DefaultInterval.
	Interval time.Duration
	// Logger receives the one line a failed read produces. Nil uses the
	// default logger.
	Logger *slog.Logger
	// Now supplies the clock, for tests.
	Now func() time.Time
}

// Source is a service's cached view of the settings table.
//
// Safe for concurrent use: a request handler reads it while the
// refresher writes it, which is the whole point.
type Source struct {
	pool     *pgxpool.Pool
	interval time.Duration
	logger   *slog.Logger
	now      func() time.Time

	mu sync.RWMutex
	// values is keyed by "key\x00site". The separator is a NUL because
	// neither a key nor a site id can contain one - keys come from a
	// closed registry and site ids are validated against a character
	// set - so no two different pairs can collide into one entry.
	values map[string]json.RawMessage
	// loadedAt is when the cache last succeeded, so a service can say
	// how stale it is rather than pretending it is current.
	loadedAt time.Time
	// failures counts consecutive failed reads, so the log line can say
	// "still failing" instead of repeating itself identically.
	failures int
}

// New builds a Source and performs the first read.
//
// The first read is not fatal if it fails: a service that refuses to
// start because the settings table was briefly unreachable has turned a
// configuration convenience into an availability risk. It starts on the
// callers' defaults and picks the real values up on the next tick.
func New(ctx context.Context, pool *pgxpool.Pool, cfg Config) *Source {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultInterval
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	s := &Source{
		pool:     pool,
		interval: cfg.Interval,
		logger:   cfg.Logger,
		now:      cfg.Now,
		values:   map[string]json.RawMessage{},
	}
	if err := s.Refresh(ctx); err != nil {
		s.logger.Warn("settings: first read failed, starting on defaults", "err", err)
	}
	return s
}

// Run refreshes on the interval until ctx is cancelled.
func (s *Source) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.Refresh(ctx); err != nil {
				// One line per failure rather than one per setting, and
				// it says how long the cache has been stale so the
				// reader can judge whether to care.
				s.logger.Warn("settings: read failed, keeping the last known values",
					"err", err, "consecutive_failures", s.Failures(), "stale_for", s.Age().Truncate(time.Second))
			}
		}
	}
}

// Refresh reads the table once.
//
// The cache is replaced only on success. A partial read - a query that
// failed halfway - must not leave a service with some settings current
// and some stale, because the combination is a state nobody designed.
func (s *Source) Refresh(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT key, site_id, value FROM panel_settings`)
	if err != nil {
		s.recordFailure()
		return fmt.Errorf("settings: query: %w", err)
	}
	defer rows.Close()

	fresh := map[string]json.RawMessage{}
	for rows.Next() {
		var key, site string
		var raw []byte
		if err := rows.Scan(&key, &site, &raw); err != nil {
			s.recordFailure()
			return fmt.Errorf("settings: scan: %w", err)
		}
		fresh[cacheKey(key, site)] = json.RawMessage(raw)
	}
	if err := rows.Err(); err != nil {
		s.recordFailure()
		return fmt.Errorf("settings: read: %w", err)
	}

	s.mu.Lock()
	s.values = fresh
	s.loadedAt = s.now()
	s.failures = 0
	s.mu.Unlock()
	return nil
}

func (s *Source) recordFailure() {
	s.mu.Lock()
	s.failures++
	s.mu.Unlock()
}

// Failures is how many consecutive reads have failed.
func (s *Source) Failures() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.failures
}

// Age is how long since the cache last loaded successfully. For the
// health page: a service running on hour-old settings should say so.
func (s *Source) Age() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.loadedAt.IsZero() {
		return 0
	}
	return s.now().Sub(s.loadedAt)
}

// Loaded reports whether any read has ever succeeded. False means every
// accessor is returning the caller's default.
func (s *Source) Loaded() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return !s.loadedAt.IsZero()
}

func cacheKey(key, site string) string { return key + "\x00" + site }

// raw resolves a key for a site, falling back to the deployment-wide
// row, mirroring panel.Store.GetSetting exactly. The two must agree, or
// the panel would show one value and the service would use another.
func (s *Source) raw(key, site string) (json.RawMessage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if site != "" {
		if value, ok := s.values[cacheKey(key, site)]; ok {
			return value, true
		}
	}
	value, ok := s.values[cacheKey(key, "")]
	return value, ok
}

// Int returns a bounded integer setting, or fallback.
//
// The bounds are checked here as well as when the panel writes. A row
// written by an older build, or edited in the database by hand, must not
// be able to hand a running service a value outside what it was written
// against - the same reasoning that bounds argon2 cost parameters when
// reading back a stored hash.
func (s *Source) Int(key, site string, fallback, min, max int) int {
	raw, ok := s.raw(key, site)
	if !ok {
		return fallback
	}
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return fallback
	}
	if n < min || n > max {
		return fallback
	}
	return n
}

// String returns a string setting bounded to one of admissible, or
// fallback. Passing a nil admissible set accepts any string up to a
// sane length.
func (s *Source) String(key, site, fallback string, admissible []string) string {
	raw, ok := s.raw(key, site)
	if !ok {
		return fallback
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return fallback
	}
	if len(value) > 1024 {
		return fallback
	}
	if admissible == nil {
		return value
	}
	for _, allowed := range admissible {
		if value == allowed {
			return value
		}
	}
	return fallback
}

// Bool returns a boolean setting, or fallback.
func (s *Source) Bool(key, site string, fallback bool) bool {
	raw, ok := s.raw(key, site)
	if !ok {
		return fallback
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return fallback
	}
	return value
}

// Strings returns a list setting, or fallback.
//
// The returned slice is a copy: a caller that sorted or appended to the
// cached one in place would corrupt every later read, and the bug would
// look like the settings table changing on its own.
func (s *Source) Strings(key, site string, fallback []string) []string {
	raw, ok := s.raw(key, site)
	if !ok {
		return fallback
	}
	var value []string
	if err := json.Unmarshal(raw, &value); err != nil {
		return fallback
	}
	if len(value) > 1000 {
		return fallback
	}
	out := make([]string, len(value))
	copy(out, value)
	return out
}

// Setting keys a service reads. These mirror internal/panel's registry,
// which is the writer; duplicating the names here rather than importing
// that package keeps the dependency pointing one way - a beacon that
// imported the panel would drag the panel's whole data layer into the
// one process the entire internet can reach.
//
// The two lists must agree. A key here that the panel does not define is
// a setting nobody can change; a key the panel defines that no service
// reads is a setting that does nothing. Both are caught by the test in
// internal/panel that walks the registry.
const (
	KeyBeaconSites          = "beacon.sites"
	KeyCampaignDropParams   = "campaign.drop_params"
	KeyCampaignExtraParams  = "campaign.extra_params"
	KeyCampaignStoreClickID = "campaign.store_click_ids"
	KeyLogLevel             = "logs.level"
)
