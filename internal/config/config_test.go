package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func writeTOML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

func TestLoad_Defaults(t *testing.T) {
	path := writeTOML(t, `
[network]
backend_addr = "127.0.0.1:8080"

[storage]
timescale_dsn = "postgres://localhost/test"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Mode != ModePassthrough {
		t.Errorf("Mode = %q, want %q", cfg.Mode, ModePassthrough)
	}
	if cfg.Network.ListenAddr != ":8443" {
		t.Errorf("ListenAddr = %q, want :8443", cfg.Network.ListenAddr)
	}
	if got, want := cfg.Cache.WindowSize(), 60*time.Second; got != want {
		t.Errorf("Cache.WindowSize() = %v, want %v", got, want)
	}
	if got, want := cfg.Cache.TTL(), 300*time.Second; got != want {
		t.Errorf("Cache.TTL() = %v, want %v", got, want)
	}
	if got, want := cfg.Cache.CleanupInterval(), 60*time.Second; got != want {
		t.Errorf("Cache.CleanupInterval() = %v, want %v", got, want)
	}
	if got, want := cfg.Network.HandshakeTimeout(), 5*time.Second; got != want {
		t.Errorf("Network.HandshakeTimeout() = %v, want %v", got, want)
	}
	if got, want := cfg.Network.DialTimeout(), 10*time.Second; got != want {
		t.Errorf("Network.DialTimeout() = %v, want %v", got, want)
	}
	if got, want := cfg.Storage.FlushInterval(), 10*time.Second; got != want {
		t.Errorf("Storage.FlushInterval() = %v, want %v", got, want)
	}
	// Limits must default to real, protective numbers - not to unlimited -
	// so a config with no [limits] section at all is still self-protecting.
	if cfg.Limits.MaxConcurrentConnections != 1000 {
		t.Errorf("Limits.MaxConcurrentConnections = %d, want 1000", cfg.Limits.MaxConcurrentConnections)
	}
	if cfg.Limits.MaxRequestsPerSecond != 500 {
		t.Errorf("Limits.MaxRequestsPerSecond = %d, want 500", cfg.Limits.MaxRequestsPerSecond)
	}
	if cfg.Limits.OverloadPolicy != PolicyFailOpen {
		t.Errorf("Limits.OverloadPolicy = %q, want %q", cfg.Limits.OverloadPolicy, PolicyFailOpen)
	}
	if cfg.Limits.ThrottleQueueSize != 200 {
		t.Errorf("Limits.ThrottleQueueSize = %d, want 200", cfg.Limits.ThrottleQueueSize)
	}
	// ASN lookup must default to disabled - a config with no [asn_lookup]
	// section at all must never start downloading RIR files or touching
	// its TimescaleDB table.
	if cfg.ASNLookup.Enabled {
		t.Error("ASNLookup.Enabled = true, want false by default")
	}
	if cfg.ASNLookup.ApplyToScoring {
		t.Error("ASNLookup.ApplyToScoring = true, want false by default")
	}
	if cfg.ASNLookup.CacheMaxEntries != 50_000 {
		t.Errorf("ASNLookup.CacheMaxEntries = %d, want 50000", cfg.ASNLookup.CacheMaxEntries)
	}
	if got, want := cfg.ASNLookup.CacheTTL(), 6*time.Hour; got != want {
		t.Errorf("ASNLookup.CacheTTL() = %v, want %v", got, want)
	}
	if got, want := cfg.ASNLookup.RefreshInterval(), 7*24*time.Hour; got != want {
		t.Errorf("ASNLookup.RefreshInterval() = %v, want %v", got, want)
	}
	if cfg.ASNLookup.LocalCSVPath != "" {
		t.Errorf("ASNLookup.LocalCSVPath = %q, want empty by default (download from GitHub Releases)", cfg.ASNLookup.LocalCSVPath)
	}
}

func TestLoad_MissingBackendAddr(t *testing.T) {
	path := writeTOML(t, `
[storage]
timescale_dsn = "postgres://localhost/test"
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for missing network.backend_addr")
	}
}

func TestLoad_MissingTimescaleDSN(t *testing.T) {
	path := writeTOML(t, `
[network]
backend_addr = "127.0.0.1:8080"
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for missing storage.timescale_dsn")
	}
}

func TestLoad_InvalidMode(t *testing.T) {
	path := writeTOML(t, `
mode = "bogus"
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for invalid mode")
	}
}

func TestLoad_FullModeRequiresCert(t *testing.T) {
	path := writeTOML(t, `
mode = "full"
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for full mode without tls.cert_file/key_file")
	}
}

func TestLoad_FullModeWithCertSucceeds(t *testing.T) {
	path := writeTOML(t, `
mode = "full"
[network]
backend_addr = "127.0.0.1:8080"
[tls]
cert_file = "/etc/ssl/cert.pem"
key_file = "/etc/ssl/key.pem"
[storage]
timescale_dsn = "postgres://localhost/test"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Mode != ModeFull {
		t.Errorf("Mode = %q, want %q", cfg.Mode, ModeFull)
	}
	if cfg.TLS.CertFile != "/etc/ssl/cert.pem" || cfg.TLS.KeyFile != "/etc/ssl/key.pem" {
		t.Errorf("TLS = %+v, want cert/key files preserved", cfg.TLS)
	}
}

func TestLoad_CustomValuesOverrideDefaults(t *testing.T) {
	path := writeTOML(t, `
[network]
listen_addr = ":9443"
backend_addr = "127.0.0.1:8080"
[cache]
window_size_seconds = 30
[storage]
timescale_dsn = "postgres://localhost/test"
flush_interval_seconds = 5
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Network.ListenAddr != ":9443" {
		t.Errorf("ListenAddr = %q, want :9443", cfg.Network.ListenAddr)
	}
	if got, want := cfg.Cache.WindowSize(), 30*time.Second; got != want {
		t.Errorf("Cache.WindowSize() = %v, want %v", got, want)
	}
	if got, want := cfg.Storage.FlushInterval(), 5*time.Second; got != want {
		t.Errorf("Storage.FlushInterval() = %v, want %v", got, want)
	}
	// Fields not present in this file must keep their defaults.
	if got, want := cfg.Cache.TTL(), 300*time.Second; got != want {
		t.Errorf("Cache.TTL() (unset in file) = %v, want default %v", got, want)
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml")); err == nil {
		t.Error("Load() error = nil, want error for a missing config file")
	}
}

func TestLoad_MalformedTOML(t *testing.T) {
	path := writeTOML(t, `this is not valid toml {{{`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for malformed TOML")
	}
}

func TestLoad_LimitsCanBeOverridden(t *testing.T) {
	path := writeTOML(t, `
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
[limits]
max_concurrent_connections = 5
max_requests_per_second = 0
overload_policy = "fail_closed"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Limits.MaxConcurrentConnections != 5 {
		t.Errorf("MaxConcurrentConnections = %d, want 5", cfg.Limits.MaxConcurrentConnections)
	}
	if cfg.Limits.MaxRequestsPerSecond != 0 {
		t.Errorf("MaxRequestsPerSecond = %d, want 0 (explicit opt-out, not the default 500)", cfg.Limits.MaxRequestsPerSecond)
	}
	if cfg.Limits.OverloadPolicy != PolicyFailClosed {
		t.Errorf("OverloadPolicy = %q, want %q", cfg.Limits.OverloadPolicy, PolicyFailClosed)
	}
	// ThrottleQueueSize wasn't set in this file - must keep its default.
	if cfg.Limits.ThrottleQueueSize != 200 {
		t.Errorf("ThrottleQueueSize (unset in file) = %d, want default 200", cfg.Limits.ThrottleQueueSize)
	}
}

func TestLoad_InvalidOverloadPolicy(t *testing.T) {
	path := writeTOML(t, `
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
[limits]
overload_policy = "yolo"
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for invalid limits.overload_policy")
	}
}

func TestLoad_ThrottleWithoutQueueSizeFails(t *testing.T) {
	path := writeTOML(t, `
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
[limits]
overload_policy = "throttle"
throttle_queue_size = 0
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for throttle policy with throttle_queue_size <= 0")
	}
}

func TestLoad_ThrottleWithQueueSizeSucceeds(t *testing.T) {
	path := writeTOML(t, `
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
[limits]
overload_policy = "throttle"
throttle_queue_size = 50
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Limits.OverloadPolicy != PolicyThrottle {
		t.Errorf("OverloadPolicy = %q, want %q", cfg.Limits.OverloadPolicy, PolicyThrottle)
	}
	if cfg.Limits.ThrottleQueueSize != 50 {
		t.Errorf("ThrottleQueueSize = %d, want 50", cfg.Limits.ThrottleQueueSize)
	}
}

func TestLoad_ASNLookupDisabledSkipsValidationOfItsOwnFields(t *testing.T) {
	// enabled = false (or the section omitted entirely) must load fine
	// even with nonsensical sub-fields - they're simply never consulted,
	// the same way tls.cert_file isn't required when mode != "full".
	path := writeTOML(t, `
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
[asn_lookup]
enabled = false
cache_max_entries = 0
cache_ttl_seconds = 0
refresh_interval_seconds = 0
blocked_countries = ["not-a-country-code"]
blocked_asns = [-1]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.ASNLookup.Enabled {
		t.Error("ASNLookup.Enabled = true, want false")
	}
}

func TestLoad_ASNLookupEnabledValidatesCacheMaxEntries(t *testing.T) {
	path := writeTOML(t, `
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
[asn_lookup]
enabled = true
cache_max_entries = 0
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for asn_lookup.enabled = true with cache_max_entries <= 0")
	}
}

func TestLoad_ASNLookupEnabledValidatesCacheTTL(t *testing.T) {
	path := writeTOML(t, `
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
[asn_lookup]
enabled = true
cache_ttl_seconds = -1
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for asn_lookup.enabled = true with cache_ttl_seconds <= 0")
	}
}

func TestLoad_ASNLookupEnabledValidatesRefreshInterval(t *testing.T) {
	path := writeTOML(t, `
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
[asn_lookup]
enabled = true
refresh_interval_seconds = 0
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for asn_lookup.enabled = true with refresh_interval_seconds <= 0")
	}
}

func TestLoad_ASNLookupEnabledValidatesBlockedCountries(t *testing.T) {
	path := writeTOML(t, `
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
[asn_lookup]
enabled = true
blocked_countries = ["USA"]
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for a 3-letter blocked_countries entry")
	}
}

func TestLoad_ASNLookupEnabledValidatesBlockedASNs(t *testing.T) {
	path := writeTOML(t, `
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
[asn_lookup]
enabled = true
blocked_asns = [0]
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for a non-positive blocked_asns entry")
	}
}

func TestLoad_ASNLookupEnabledWithValidFieldsSucceeds(t *testing.T) {
	path := writeTOML(t, `
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
[asn_lookup]
enabled = true
apply_to_scoring = true
cache_max_entries = 1000
cache_ttl_seconds = 3600
refresh_interval_seconds = 86400
local_csv_path = "/var/lib/crucible-analytic/geoip"
blocked_countries = ["CN", "ru"]
blocked_asns = [64512, 64513]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if !cfg.ASNLookup.Enabled {
		t.Error("ASNLookup.Enabled = false, want true")
	}
	if !cfg.ASNLookup.ApplyToScoring {
		t.Error("ASNLookup.ApplyToScoring = false, want true")
	}
	if cfg.ASNLookup.CacheMaxEntries != 1000 {
		t.Errorf("CacheMaxEntries = %d, want 1000", cfg.ASNLookup.CacheMaxEntries)
	}
	if got, want := cfg.ASNLookup.CacheTTL(), time.Hour; got != want {
		t.Errorf("CacheTTL() = %v, want %v", got, want)
	}
	if got, want := cfg.ASNLookup.RefreshInterval(), 24*time.Hour; got != want {
		t.Errorf("RefreshInterval() = %v, want %v", got, want)
	}
	if want := "/var/lib/crucible-analytic/geoip"; cfg.ASNLookup.LocalCSVPath != want {
		t.Errorf("LocalCSVPath = %q, want %q", cfg.ASNLookup.LocalCSVPath, want)
	}
	if want := []string{"CN", "ru"}; !slices.Equal(cfg.ASNLookup.BlockedCountries, want) {
		t.Errorf("BlockedCountries = %v, want %v (validation checks well-formedness, not casing - normalization happens in limiter.NewGeoBlocklist)", cfg.ASNLookup.BlockedCountries, want)
	}
	if want := []int{64512, 64513}; !slices.Equal(cfg.ASNLookup.BlockedASNs, want) {
		t.Errorf("BlockedASNs = %v, want %v", cfg.ASNLookup.BlockedASNs, want)
	}
}
