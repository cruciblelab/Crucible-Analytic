package beacon

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/BurntSushi/toml"
)

// siteIDPattern must stay identical to config.siteIDPattern in the
// collector: a beacon accepting a site_id the collector could never
// have written would produce beacon_events rows that no
// traffic_snapshots row can ever join to.
var siteIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// Config is the beacon's own TOML config, separate from both the
// collector's and the read API's because all three are separate
// processes with different database privileges: the collector writes
// traffic_snapshots, the API reads, and this writes beacon_events. Its
// DSN should name a role that can INSERT into beacon_events and nothing
// else.
type Config struct {
	ListenAddr string `toml:"listen_addr"`
	// PathPrefix is where /ca.js and /event mount. Empty means
	// DefaultPathPrefix.
	PathPrefix   string `toml:"path_prefix"`
	TimescaleDSN string `toml:"timescale_dsn"`
	// Sites is the allowlist of site_ids this beacon accepts. Required:
	// the snippet is public, so without it anyone could write rows under
	// any site name they liked.
	Sites []string `toml:"sites"`
	// TrustedProxies lists the CIDRs (or bare addresses) whose
	// X-Forwarded-For / X-Real-IP headers are believed. Set this to the
	// reverse proxy in front of the beacon and nothing else - see
	// ClientIPResolver for what goes wrong if it is set too broadly.
	TrustedProxies []string `toml:"trusted_proxies"`
	// AllowedOrigins narrows CORS; empty allows every origin, which is
	// safe for this endpoint - see Server.AllowedOrigins.
	AllowedOrigins []string        `toml:"allowed_origins"`
	Buffer         BufferConfig    `toml:"buffer"`
	Limits         LimitsConfig    `toml:"limits"`
	ASNLookup      ASNLookupConfig `toml:"asn_lookup"`
}

// BufferConfig sizes the in-memory write buffer. Zero values take
// WriterConfig's defaults.
type BufferConfig struct {
	Size                 int `toml:"size"`
	BatchSize            int `toml:"batch_size"`
	FlushIntervalSeconds int `toml:"flush_interval_seconds"`
}

// LimitsConfig mirrors the collector's [limits] section, applied to
// beacon requests instead of proxied ones. Zero or negative means "no
// limit" for that dimension, exactly as in internal/limiter.
type LimitsConfig struct {
	MaxConcurrentRequests int    `toml:"max_concurrent_requests"`
	MaxRequestsPerSecond  int    `toml:"max_requests_per_second"`
	OverloadPolicy        string `toml:"overload_policy"`
	ThrottleQueueSize     int    `toml:"throttle_queue_size"`
}

// ASNLookupConfig controls country/ASN enrichment of beacon events.
//
// Off by default, and that default is the recommendation whenever a
// collector runs on the same host. The collector already resolves and
// stores country/ASN for every IP it sees, and every beacon event comes
// from a browser that necessarily connected through it - so the
// geography of a beacon event can be recovered at read time by joining
// on ip, at no memory cost. Turning this on loads a second full copy of
// the range tables into this process (on the order of a hundred
// megabytes), which is worth paying only when the beacon runs somewhere
// the collector does not.
type ASNLookupConfig struct {
	Enabled                bool   `toml:"enabled"`
	CacheMaxEntries        int    `toml:"cache_max_entries"`
	CacheTTLSeconds        int    `toml:"cache_ttl_seconds"`
	RefreshIntervalSeconds int    `toml:"refresh_interval_seconds"`
	LocalCSVPath           string `toml:"local_csv_path"`
}

func (b BufferConfig) FlushInterval() time.Duration {
	return time.Duration(b.FlushIntervalSeconds) * time.Second
}

func (a ASNLookupConfig) CacheTTL() time.Duration {
	return time.Duration(a.CacheTTLSeconds) * time.Second
}

func (a ASNLookupConfig) RefreshInterval() time.Duration {
	return time.Duration(a.RefreshIntervalSeconds) * time.Second
}

// LoadConfig reads and validates the beacon config at path.
func LoadConfig(path string) (Config, error) {
	cfg := Config{
		// Loopback by default: the recommended deployment has a web
		// server in front terminating TLS and forwarding the prefix, so
		// binding publicly should be a deliberate edit rather than
		// something that happens by omission.
		ListenAddr: "127.0.0.1:8081",
		ASNLookup: ASNLookupConfig{
			CacheMaxEntries:        100_000,
			CacheTTLSeconds:        3600,
			RefreshIntervalSeconds: 86400,
		},
	}
	if _, err := os.Stat(path); err != nil {
		return Config{}, fmt.Errorf("beacon: config file %s: %w", path, err)
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Config{}, fmt.Errorf("beacon: parse %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.TimescaleDSN == "" {
		return fmt.Errorf("beacon: timescale_dsn is required")
	}
	if len(c.Sites) == 0 {
		return fmt.Errorf("beacon: sites is required (the site_ids this beacon accepts events for)")
	}
	for _, site := range c.Sites {
		if !siteIDPattern.MatchString(site) {
			return fmt.Errorf("beacon: invalid site %q (want 1-64 characters, letters/digits/underscore/dash only)", site)
		}
	}
	if _, err := ParseTrustedProxies(c.TrustedProxies); err != nil {
		return fmt.Errorf("beacon: invalid trusted_proxies entry: %w", err)
	}
	switch c.Limits.OverloadPolicy {
	case "", "fail_open", "fail_closed", "throttle":
	default:
		return fmt.Errorf("beacon: invalid limits.overload_policy %q (want fail_open, fail_closed or throttle)", c.Limits.OverloadPolicy)
	}
	if c.ASNLookup.Enabled {
		if c.ASNLookup.CacheMaxEntries <= 0 {
			return fmt.Errorf("beacon: asn_lookup.cache_max_entries must be positive")
		}
		if c.ASNLookup.CacheTTLSeconds <= 0 {
			return fmt.Errorf("beacon: asn_lookup.cache_ttl_seconds must be positive")
		}
		if c.ASNLookup.RefreshIntervalSeconds <= 0 {
			return fmt.Errorf("beacon: asn_lookup.refresh_interval_seconds must be positive")
		}
	}
	return nil
}
