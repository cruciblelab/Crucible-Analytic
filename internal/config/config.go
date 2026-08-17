// Package config loads collector configuration from a TOML file, with
// sane defaults for everything except the handful of values that have no
// safe default (where to send traffic, where to persist it, and - in full
// mode - the TLS certificate/key to terminate with).
package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/privacy"
	"github.com/cruciblelab/crucible-analytic/internal/retention"
)

// Mode selects which proxy implementation main.go wires up.
type Mode string

const (
	// ModePassthrough is the default: a content-blind TCP/TLS proxy that
	// never terminates TLS (internal/proxy). Unaffected by this package's
	// full-mode-related fields.
	ModePassthrough Mode = "passthrough"
	// ModeFull terminates TLS and reverse-proxies HTTP (internal/fullproxy),
	// trading a larger trust boundary (it needs the backend's real
	// certificate/key) for real per-request visibility.
	ModeFull Mode = "full"
)

// OverloadPolicy selects what the collector does when the limits in
// LimitsConfig are exceeded. It mirrors limiter.Policy's values as plain
// strings rather than importing that package directly, the same way Mode
// stays self-contained instead of importing proxy/fullproxy - main.go
// does the translation when it constructs a limiter.Limiter, keeping
// internal/limiter usable and testable without any dependency on how
// configuration happens to be loaded.
type OverloadPolicy string

const (
	// PolicyFailOpen is the default: skip fingerprinting/recording for
	// traffic over the limit, but keep forwarding it to the backend
	// normally. The collector should never, by default, be the reason a
	// site goes down - the other two policies are opt-in.
	PolicyFailOpen OverloadPolicy = "fail_open"
	// PolicyFailClosed rejects connections/requests over the limit
	// outright.
	PolicyFailClosed OverloadPolicy = "fail_closed"
	// PolicyThrottle queues excess connections/requests (bounded by
	// ThrottleQueueSize) until capacity frees up, falling back to
	// fail-closed behavior if the queue itself is full.
	PolicyThrottle OverloadPolicy = "throttle"
)

// Config holds everything main.go needs to wire up the collector, decoded
// from a TOML file. Durations are stored as plain seconds in the file
// (simplest to write by hand) and exposed as time.Duration via the
// accessor methods below.
type Config struct {
	// SiteID identifies which site this collector instance is fronting.
	// Required, and stamped onto every traffic_snapshots row, so one
	// TimescaleDB can hold several sites' data - the case when one VDS
	// hosts more than one customer site, each with its own collector
	// process but sharing a database. It's also the path segment the
	// read API exposes a site under (see cmd/analytics-api), which is
	// why the character set is restricted below rather than free-form.
	SiteID    string          `toml:"site_id"`
	Mode      Mode            `toml:"mode"`
	Network   NetworkConfig   `toml:"network"`
	TLS       TLSConfig       `toml:"tls"`
	Cache     CacheConfig     `toml:"cache"`
	Storage   StorageConfig   `toml:"storage"`
	Limits    LimitsConfig    `toml:"limits"`
	ASNLookup ASNLookupConfig `toml:"asn_lookup"`
	BotData   BotDataConfig   `toml:"bot_data"`
	Privacy   PrivacyConfig   `toml:"privacy"`
	Retention RetentionConfig `toml:"retention"`
	Logging   logging.Config  `toml:"logging"`
}

// BotDataConfig points at the known-bot fingerprint file.
//
// This project ships no copy of that dataset. It belongs to somebody
// else, this repository is MIT, and carrying third-party data under
// unstated terms inside a permissively licensed repository hands that
// uncertainty to everyone who clones it. The deployment fetches it
// instead, onto its own machine, under the source's own terms:
//
//	collector -config collector.toml -update-bot-data
//
// Run that from cron, by hand, or from wherever you like - the schedule
// is not this software's business.
//
// An empty path, or a path with no file at it yet, is a supported
// state: the known-bot signal is simply absent and every other signal
// still works. The collector says so at startup rather than leaving it
// to be discovered.
type BotDataConfig struct {
	// Path is where the fetched file lives, read at startup and written
	// by -update-bot-data.
	Path string `toml:"path"`
	// SourceURL overrides where it is fetched from. Empty takes
	// botdata.DefaultSourceURL - configurable because pinning a third
	// party's URL into a binary is how a deployment becomes unable to
	// update itself the day that host moves.
	SourceURL string `toml:"source_url"`
}

// PrivacyConfig decides what personal data reaches the disk.
//
// Its own section rather than a key under [storage], because these are
// not storage decisions - they are answers to legal questions, and
// somebody reading the file to check what this deployment keeps should
// find them together and not have to know which subsystem implements
// them.
type PrivacyConfig struct {
	// IPStorage is "masked" (the default) or "full".
	//
	// An empty value means masked, so a config file written before this
	// setting existed - and every config file that simply does not
	// mention it - stores less rather than more. That direction is the
	// whole point: the value nobody sets is the one that ends up in
	// production.
	IPStorage string `toml:"ip_storage"`
	// IPHashKey keys the pseudonym in hashed mode. Must match the
	// beacon's, or the crossover join silently finds nothing.
	IPHashKey string `toml:"ip_hash_key"`
}

// HashKey returns the configured key as bytes.
func (p PrivacyConfig) HashKey() []byte { return []byte(p.IPHashKey) }

// IPMode resolves the configured value, defaulting to masked.
func (p PrivacyConfig) IPMode() privacy.IPMode { return privacy.ParseIPMode(p.IPStorage) }

// RetentionConfig bounds how long traffic_snapshots is kept.
//
// The collector reads this from its file rather than from the panel,
// because the collector has no live-settings reader yet (A6-devam). The
// beacon does, so beacon_events already follows the panel. The two
// tables are therefore configured in different places for now, which is
// a gap worth naming rather than papering over.
type RetentionConfig struct {
	// Days is the retention. Zero, or anything out of range, takes the
	// default of 90 - out-of-range is treated as unset rather than
	// clamped, so a config saying 20000 is visible as a mistake.
	Days int `toml:"days"`
	// IntervalHours is how often the policy is re-applied. Zero takes an
	// hour.
	IntervalHours int `toml:"interval_hours"`
}

// DefaultRetentionDays matches the panel's default and the read API's
// maximum range.
const DefaultRetentionDays = 90

// Resolved is the configured retention, or the default.
func (r RetentionConfig) Resolved() int {
	if r.Days >= retention.MinDays && r.Days <= retention.MaxDays {
		return r.Days
	}
	return DefaultRetentionDays
}

// Interval is how often to re-apply, defaulting to an hour.
func (r RetentionConfig) Interval() time.Duration {
	if r.IntervalHours > 0 {
		return time.Duration(r.IntervalHours) * time.Hour
	}
	return time.Hour
}

// siteIDPattern restricts SiteID to characters that are safe unescaped in
// a URL path segment and in a filename, so the same identifier can be used
// verbatim in the read API's routes without any encoding step that could
// disagree between the collector writing rows and the API reading them.
var siteIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// NetworkConfig covers where the collector listens and what it proxies to.
// BackendAddr is a plain host:port in both modes: passthrough dials it
// directly over TCP, full mode treats it as a plaintext HTTP backend (the
// standard "TLS terminates at the edge, internal traffic is HTTP" setup -
// an HTTPS backend isn't supported yet, since that's a meaningfully
// different, not-yet-requested case, not an oversight).
type NetworkConfig struct {
	ListenAddr              string `toml:"listen_addr"`
	BackendAddr             string `toml:"backend_addr"`
	DialTimeoutSeconds      int    `toml:"dial_timeout_seconds"`
	HandshakeTimeoutSeconds int    `toml:"handshake_timeout_seconds"`
}

// DialTimeout bounds connecting to BackendAddr, in both modes.
func (n NetworkConfig) DialTimeout() time.Duration {
	return time.Duration(n.DialTimeoutSeconds) * time.Second
}

// HandshakeTimeout bounds how long passthrough mode waits to see a
// complete ClientHello before giving up on fingerprinting. Unused in full
// mode, where crypto/tls owns handshake timing.
func (n NetworkConfig) HandshakeTimeout() time.Duration {
	return time.Duration(n.HandshakeTimeoutSeconds) * time.Second
}

// TLSConfig is only consulted in full mode, to terminate TLS with the
// backend's real certificate/key.
type TLSConfig struct {
	CertFile string `toml:"cert_file"`
	KeyFile  string `toml:"key_file"`
}

// CacheConfig tunes the in-memory RateStore's sliding window and eviction.
type CacheConfig struct {
	WindowSizeSeconds      int `toml:"window_size_seconds"`
	TTLSeconds             int `toml:"ttl_seconds"`
	CleanupIntervalSeconds int `toml:"cleanup_interval_seconds"`
}

// WindowSize is the sliding-window width used for rate estimation.
func (c CacheConfig) WindowSize() time.Duration {
	return time.Duration(c.WindowSizeSeconds) * time.Second
}

// TTL is how long an IP can go without a request before its state is
// dropped from memory.
func (c CacheConfig) TTL() time.Duration {
	return time.Duration(c.TTLSeconds) * time.Second
}

// CleanupInterval is how often the idle-TTL sweep runs.
func (c CacheConfig) CleanupInterval() time.Duration {
	return time.Duration(c.CleanupIntervalSeconds) * time.Second
}

// StorageConfig configures the periodic flush to TimescaleDB.
type StorageConfig struct {
	TimescaleDSN         string `toml:"timescale_dsn"`
	FlushIntervalSeconds int    `toml:"flush_interval_seconds"`
}

// FlushInterval is how often RateStore state is summarized and written to
// TimescaleDB.
func (s StorageConfig) FlushInterval() time.Duration {
	return time.Duration(s.FlushIntervalSeconds) * time.Second
}

// LimitsConfig bounds the collector's own total resource usage -
// concurrent connections/requests and requests/second, summed across all
// IPs - independent of anything in CacheConfig/RateStore, which is about
// per-IP behavior for scoring, not the collector's own load. Without
// this, the collector has no upper bound on concurrency and becomes a
// resource-exhaustion target itself. Zero (including an absent
// MaxConcurrentConnections/MaxRequestsPerSecond field) means "no limit"
// for that one dimension - but see defaults() for why both actually
// default to a real, protective number rather than to zero.
type LimitsConfig struct {
	MaxConcurrentConnections int            `toml:"max_concurrent_connections"`
	MaxRequestsPerSecond     int            `toml:"max_requests_per_second"`
	OverloadPolicy           OverloadPolicy `toml:"overload_policy"`
	ThrottleQueueSize        int            `toml:"throttle_queue_size"`
}

// ASNLookupConfig configures the optional internal/asnlookup module,
// which resolves an IP to both the country it's registered to and the ASN
// that routes it (see that package's doc comment for why the two datasets
// are kept independent rather than merged). Disabled by default: when
// Enabled is false, nothing else in this section is consulted, neither
// dataset is ever downloaded or read, and asnlookup's TimescaleDB tables
// are never touched.
type ASNLookupConfig struct {
	Enabled bool `toml:"enabled"`
	// ApplyToScoring gates KnownBotASNs below: when false (the default),
	// internal/scoring never sees an ASN or a known-bot-ASN set at all,
	// so its ASN component always contributes 0, identical to today's
	// behavior. When true, storage.Flusher is wired with a
	// map[int]struct{} built from KnownBotASNs, and each flush's
	// scoring.Score call gets the snapshot's already-resolved ASN (the
	// same Resolve() call already made for storage enrichment - no extra
	// lookup) alongside it.
	ApplyToScoring         bool `toml:"apply_to_scoring"`
	CacheMaxEntries        int  `toml:"cache_max_entries"`
	CacheTTLSeconds        int  `toml:"cache_ttl_seconds"`
	RefreshIntervalSeconds int  `toml:"refresh_interval_seconds"`
	// LocalCSVPath, if set, skips downloading either dataset from GitHub
	// Releases entirely: every refresh instead reads
	// <LocalCSVPath>/user-country-ipv4.csv, -ipv6.csv, origin-asn-ipv4.csv
	// and -ipv6.csv from local disk, with no network access of any kind.
	// Useful for an offline VDS, or for operators who'd rather manage the
	// download themselves (e.g. via their own cron job writing into that
	// directory) than let the collector reach out to GitHub on its own
	// schedule. Empty (the default) means download normally.
	LocalCSVPath string `toml:"local_csv_path"`
	// BlockedCountries and BlockedASNs configure limiter.GeoBlocklist - a
	// request whose resolved country or ASN matches either list is
	// rejected outright, regardless of limits.overload_policy (blocking
	// by geography/ASN is a deliberate security decision, not
	// collector-load-shedding). Both empty (the default) means no
	// blocking - and, importantly, means main.go never wires a resolver
	// into the proxy's admission path at all, so enabling asn_lookup for
	// storage enrichment alone (see Aşama 2) costs nothing extra on the
	// request path. Country codes are case-insensitive (normalized like
	// asnlookup's own parser); see NOTES.md for the richer per-rule-policy
	// version of this that was deliberately deferred rather than built
	// now.
	BlockedCountries []string `toml:"blocked_countries"`
	BlockedASNs      []int    `toml:"blocked_asns"`
	// KnownBotASNs is a separate list from BlockedASNs, only consulted
	// when ApplyToScoring = true: matching ASNs add a flat bonus to the
	// score (see scoring.maxASNScore) instead of being blocked outright.
	// A blocked ASN is rejected before it ever reaches scoring, so
	// reusing BlockedASNs here wouldn't do anything - these need to be
	// separately configured lists for separate purposes (deny vs. flag
	// as more suspicious but still let through).
	KnownBotASNs []int `toml:"known_bot_asns"`
}

// CacheTTL is how long one resolved IP is cached before the next lookup
// re-checks the in-memory range tables.
func (a ASNLookupConfig) CacheTTL() time.Duration {
	return time.Duration(a.CacheTTLSeconds) * time.Second
}

// RefreshInterval is how often both datasets are re-fetched (downloaded,
// or re-read from LocalCSVPath) and re-parsed.
func (a ASNLookupConfig) RefreshInterval() time.Duration {
	return time.Duration(a.RefreshIntervalSeconds) * time.Second
}

func defaults() Config {
	return Config{
		Mode: ModePassthrough,
		Network: NetworkConfig{
			ListenAddr:              ":8443",
			DialTimeoutSeconds:      10,
			HandshakeTimeoutSeconds: 5,
		},
		Cache: CacheConfig{
			WindowSizeSeconds:      60,
			TTLSeconds:             300,
			CleanupIntervalSeconds: 60,
		},
		Storage: StorageConfig{
			FlushIntervalSeconds: 10,
		},
		Limits: LimitsConfig{
			// Real, protective numbers by default - not zero/unlimited -
			// so the collector is self-protecting out of the box, even
			// for a config file with no [limits] section at all. A user
			// who genuinely wants a dimension unlimited sets it to 0
			// explicitly, which is then a deliberate, visible choice in
			// their own file rather than an accidental gap.
			MaxConcurrentConnections: 1000,
			MaxRequestsPerSecond:     500,
			OverloadPolicy:           PolicyFailOpen,
			ThrottleQueueSize:        200,
		},
		ASNLookup: ASNLookupConfig{
			Enabled:                false,
			ApplyToScoring:         false,
			CacheMaxEntries:        50_000,
			CacheTTLSeconds:        6 * 60 * 60,      // 6 hours
			RefreshIntervalSeconds: 7 * 24 * 60 * 60, // 1 week
			LocalCSVPath:           "",               // download from GitHub Releases by default
			BlockedCountries:       nil,              // no blocking by default
			BlockedASNs:            nil,
			KnownBotASNs:           nil, // no ASN scoring signal by default
		},
	}
}

// Load reads and validates configuration from the TOML file at path.
// Fields absent from the file keep their defaults - see defaults() - so a
// minimal file only needs to set what actually differs.
func Load(path string) (*Config, error) {
	cfg := defaults()
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	switch c.Mode {
	case "":
		c.Mode = ModePassthrough
	case ModePassthrough, ModeFull:
	default:
		return fmt.Errorf("config: invalid mode %q (want %q or %q)", c.Mode, ModePassthrough, ModeFull)
	}

	// Required rather than defaulted to something like "default": an
	// unset site_id would silently commingle two sites' rows the moment a
	// second collector is pointed at the same database, and that's a
	// data-integrity problem you'd only notice long after the fact.
	if c.SiteID == "" {
		return fmt.Errorf("config: site_id is required (identifies which site this collector's rows belong to)")
	}
	if !siteIDPattern.MatchString(c.SiteID) {
		return fmt.Errorf("config: invalid site_id %q (want 1-64 characters, letters/digits/underscore/dash only)", c.SiteID)
	}

	if c.Network.BackendAddr == "" {
		return fmt.Errorf("config: network.backend_addr is required (host:port of the site to proxy to)")
	}
	if c.Storage.TimescaleDSN == "" {
		return fmt.Errorf("config: storage.timescale_dsn is required (postgres connection string for TimescaleDB)")
	}
	if c.Mode == ModeFull && (c.TLS.CertFile == "" || c.TLS.KeyFile == "") {
		return fmt.Errorf("config: tls.cert_file and tls.key_file are required when mode = %q", ModeFull)
	}

	switch c.Limits.OverloadPolicy {
	case "":
		c.Limits.OverloadPolicy = PolicyFailOpen
	case PolicyFailOpen, PolicyFailClosed, PolicyThrottle:
	default:
		return fmt.Errorf("config: invalid limits.overload_policy %q (want %q, %q, or %q)",
			c.Limits.OverloadPolicy, PolicyFailOpen, PolicyFailClosed, PolicyThrottle)
	}
	if c.Limits.OverloadPolicy == PolicyThrottle && c.Limits.ThrottleQueueSize <= 0 {
		return fmt.Errorf("config: limits.throttle_queue_size must be positive when limits.overload_policy = %q", PolicyThrottle)
	}

	if c.ASNLookup.Enabled {
		if c.ASNLookup.CacheMaxEntries <= 0 {
			return fmt.Errorf("config: asn_lookup.cache_max_entries must be positive when asn_lookup.enabled = true")
		}
		if c.ASNLookup.CacheTTLSeconds <= 0 {
			return fmt.Errorf("config: asn_lookup.cache_ttl_seconds must be positive when asn_lookup.enabled = true")
		}
		if c.ASNLookup.RefreshIntervalSeconds <= 0 {
			return fmt.Errorf("config: asn_lookup.refresh_interval_seconds must be positive when asn_lookup.enabled = true")
		}
		for _, country := range c.ASNLookup.BlockedCountries {
			if len(strings.TrimSpace(country)) != 2 {
				return fmt.Errorf("config: asn_lookup.blocked_countries entry %q is not a 2-letter ISO 3166-1 alpha-2 code", country)
			}
		}
		for _, asn := range c.ASNLookup.BlockedASNs {
			if asn <= 0 {
				return fmt.Errorf("config: asn_lookup.blocked_asns entry %d must be positive", asn)
			}
		}
		for _, asn := range c.ASNLookup.KnownBotASNs {
			if asn <= 0 {
				return fmt.Errorf("config: asn_lookup.known_bot_asns entry %d must be positive", asn)
			}
		}
	}

	// Rejected here even though privacy.ParseIPMode would quietly fall
	// back to masked. The two are answering different questions: at
	// runtime a bad value must not stop a running service, but at
	// startup somebody is standing at the file and can fix it, and a
	// deployment that wrote "tam" expecting full addresses should be
	// told it did not get them rather than discovering it in a year.
	if v := strings.TrimSpace(c.Privacy.IPStorage); v != "" &&
		v != string(privacy.IPFull) && v != string(privacy.IPMasked) {
		return fmt.Errorf("config: privacy.ip_storage must be %q or %q, got %q",
			privacy.IPMasked, privacy.IPFull, c.Privacy.IPStorage)
	}
	// Refused at startup rather than falling back, because the fallback
	// is the dangerous direction: full mode with no usable key would
	// write the masked address and no token, so the deployment would
	// silently be in masked mode while its config said otherwise.
	if c.Privacy.IPMode().Tokenises() && len(c.Privacy.HashKey()) < privacy.MinHashKeyLen {
		return fmt.Errorf("config: privacy.ip_hash_key must be at least %d bytes when ip_storage = %q "+
			"- generate one with: go run ./cmd/devpass -ipkey",
			privacy.MinHashKeyLen, privacy.IPFull)
	}

	return nil
}
