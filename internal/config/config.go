// Package config loads collector configuration from a TOML file, with
// sane defaults for everything except the handful of values that have no
// safe default (where to send traffic, where to persist it, and - in full
// mode - the TLS certificate/key to terminate with).
package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
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
	Mode      Mode            `toml:"mode"`
	Network   NetworkConfig   `toml:"network"`
	TLS       TLSConfig       `toml:"tls"`
	Cache     CacheConfig     `toml:"cache"`
	Storage   StorageConfig   `toml:"storage"`
	Limits    LimitsConfig    `toml:"limits"`
	ASNLookup ASNLookupConfig `toml:"asn_lookup"`
}

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
	// ApplyToScoring is accepted and validated but not yet consulted by
	// internal/scoring - country and ASN are both resolved for real, and
	// BlockedCountries/BlockedASNs below already consume them for
	// blocking, but wiring either into a scoring *decision* is later
	// work. It's part of the config shape now so wiring it up later is a
	// behavior change, not a schema change.
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
	}

	return nil
}
