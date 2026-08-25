package collector

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func writeTOML(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "collector.toml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing test config: %v", err)
	}
	return path
}

func TestLoad_Defaults(t *testing.T) {
	path := writeTOML(t, `
site_id = "test-site"
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

func TestLoad_MissingSiteID(t *testing.T) {
	path := writeTOML(t, `
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for missing site_id")
	}
}

func TestLoad_InvalidSiteID(t *testing.T) {
	// site_id ends up in the read API's URL paths, so anything needing
	// escaping (or a path separator, which could change what route a
	// request actually hits) has to be rejected up front rather than
	// escaped inconsistently later.
	for _, siteID := range []string{
		"has space",
		"has/slash",
		"has.dot",
		"tırnaklı",              // non-ASCII
		strings.Repeat("a", 65), // over the 64-char cap
	} {
		t.Run(siteID, func(t *testing.T) {
			path := writeTOML(t, `
site_id = "`+siteID+`"
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
`)
			if _, err := Load(path); err == nil {
				t.Errorf("Load() error = nil, want error for site_id %q", siteID)
			}
		})
	}
}

func TestLoad_ValidSiteIDIsAccepted(t *testing.T) {
	for _, siteID := range []string{"ahmetteknoloji", "site-a", "site_b", "Site123", "a", strings.Repeat("a", 64)} {
		t.Run(siteID, func(t *testing.T) {
			path := writeTOML(t, `
site_id = "`+siteID+`"
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
`)
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load() error = %v, want success for site_id %q", err, siteID)
			}
			if cfg.SiteID != siteID {
				t.Errorf("SiteID = %q, want %q", cfg.SiteID, siteID)
			}
		})
	}
}

func TestLoad_MissingBackendAddr(t *testing.T) {
	path := writeTOML(t, `
site_id = "test-site"
[storage]
timescale_dsn = "postgres://localhost/test"
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for missing network.backend_addr")
	}
}

func TestLoad_MissingTimescaleDSN(t *testing.T) {
	path := writeTOML(t, `
site_id = "test-site"
[network]
backend_addr = "127.0.0.1:8080"
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for missing storage.timescale_dsn")
	}
}

func TestLoad_InvalidMode(t *testing.T) {
	path := writeTOML(t, `
site_id = "test-site"
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
site_id = "test-site"
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
site_id = "test-site"
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
site_id = "test-site"
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
site_id = "test-site"
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
site_id = "test-site"
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
site_id = "test-site"
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
site_id = "test-site"
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
site_id = "test-site"
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
known_bot_asns = [-1]
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
site_id = "test-site"
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
site_id = "test-site"
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
site_id = "test-site"
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
site_id = "test-site"
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
site_id = "test-site"
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

func TestLoad_ASNLookupEnabledValidatesKnownBotASNs(t *testing.T) {
	path := writeTOML(t, `
site_id = "test-site"
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
[asn_lookup]
enabled = true
known_bot_asns = [-5]
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for a non-positive known_bot_asns entry")
	}
}

func TestLoad_ASNLookupEnabledWithValidFieldsSucceeds(t *testing.T) {
	path := writeTOML(t, `
site_id = "test-site"
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
known_bot_asns = [64514, 64515]
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
	if want := []int{64514, 64515}; !slices.Equal(cfg.ASNLookup.KnownBotASNs, want) {
		t.Errorf("KnownBotASNs = %v, want %v", cfg.ASNLookup.KnownBotASNs, want)
	}
}

// --- A5.2: the settings that stop an attack ---

// TestLiveBlocklistTakesTheStoredListOverTheFile.
//
// A nil source is the deployment that never granted SELECT on
// panel_settings: it has to keep working from its file, which is the
// promise A6 made and the reason a settings failure is logged rather
// than fatal.
func TestLiveBlocklistFallsBackToTheFile(t *testing.T) {
	cfg := ASNLookupConfig{
		BlockedCountries: []string{"CN"},
		BlockedASNs:      []int{64512},
	}
	countries, asns := cfg.LiveBlocklist(nil)
	if !slices.Equal(countries, []string{"CN"}) {
		t.Errorf("countries = %v, want the file's", countries)
	}
	if !slices.Equal(asns, []int{64512}) {
		t.Errorf("asns = %v, want the file's", asns)
	}
}

// TestLiveKnownBotASNsIsNilWhenTheSignalIsOff.
//
// nil rather than an empty map, because that is what scoring.Score
// already treats as "no ASN component" - so turning the signal off needs
// no separate switch anywhere downstream, and a deployment that never
// chose one gets exactly the scores it got before.
func TestLiveKnownBotASNsIsNilWhenTheSignalIsOff(t *testing.T) {
	cases := []struct {
		name string
		cfg  ASNLookupConfig
	}{
		{"apply_to_scoring off", ASNLookupConfig{KnownBotASNs: []int{64512}}},
		{"on but no list", ASNLookupConfig{ApplyToScoring: true}},
		{"neither", ASNLookupConfig{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.LiveKnownBotASNs(nil); got != nil {
				t.Errorf("LiveKnownBotASNs = %v, want nil", got)
			}
		})
	}

	on := ASNLookupConfig{ApplyToScoring: true, KnownBotASNs: []int{64512, 64513}}
	got := on.LiveKnownBotASNs(nil)
	if len(got) != 2 {
		t.Fatalf("LiveKnownBotASNs = %v, want two entries", got)
	}
	if _, ok := got[64512]; !ok {
		t.Error("64512 is missing from the list the file configured")
	}
}

// TestASNsSurviveTheTextRoundTrip.
//
// ASNs are numbers in a config file and text in the settings column,
// because the column is text. The round trip is not a smell - the panel
// validates each entry as a positive number before storing it, and this
// parses again before using it - but a value that could not survive it
// would become a rule nobody wrote.
func TestASNsSurviveTheTextRoundTrip(t *testing.T) {
	in := []int{64512, 4294967294}
	if got := asnNumbers(intsAsStrings(in)); !slices.Equal(got, in) {
		t.Errorf("round trip = %v, want %v", got, in)
	}
}

// TestRubbishASNsAreDroppedRatherThanBecomingZero.
//
// Zero is asnlookup's "not resolved", so an entry that parsed to zero
// would match every address the lookup could not place - the opposite of
// what a malformed line means. These come out of a database column that
// a person can edit by hand.
func TestRubbishASNsAreDroppedRatherThanBecomingZero(t *testing.T) {
	got := asnNumbers([]string{"64512", "", "  ", "abc", "-1", "0", "64513"})
	if !slices.Equal(got, []int{64512, 64513}) {
		t.Errorf("asnNumbers = %v, want only the two real ones", got)
	}
	for _, n := range got {
		if n == 0 {
			t.Error("a dropped entry became 0, which matches every unresolved address")
		}
	}
}

// TestLoad_RetentionOutsideTheBoundsIsRefused.
//
// The ceiling came down from ten years to two when retention left the
// panel for the config files, which means a file written against an
// older build can now be out of range. The old behaviour for out of
// range was to fall back to 90 days without saying so - and a
// deployment that believes it keeps five years while keeping three
// months finds out from a customer asking for last year's figures.
//
// 3650 is in the table on purpose: it was the previous ceiling and is
// therefore the exact value an existing deployment is most likely to
// have written down.
func TestLoad_RetentionOutsideTheBoundsIsRefused(t *testing.T) {
	for _, days := range []int{-1, 3650, 731, 20000} {
		t.Run(strconv.Itoa(days), func(t *testing.T) {
			path := writeTOML(t, `
site_id = "bir-site"
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
[retention]
days = `+strconv.Itoa(days)+`
`)
			if _, err := Load(path); err == nil {
				t.Errorf("Load() accepted retention.days = %d", days)
			}
		})
	}
}

// TestLoad_RetentionInsideTheBoundsIsKept, including the two ends and
// the "said nothing" case that takes the default.
func TestLoad_RetentionInsideTheBoundsIsKept(t *testing.T) {
	for _, tc := range []struct{ days, want int }{
		{0, DefaultRetentionDays}, // unset
		{1, 1},                    // the floor
		{90, 90},                  // the default, written out
		{730, 730},                // the ceiling
	} {
		t.Run(strconv.Itoa(tc.days), func(t *testing.T) {
			path := writeTOML(t, `
site_id = "bir-site"
[network]
backend_addr = "127.0.0.1:8080"
[storage]
timescale_dsn = "postgres://localhost/test"
[retention]
days = `+strconv.Itoa(tc.days)+`
`)
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if got := cfg.Retention.Resolved(); got != tc.want {
				t.Errorf("Resolved() = %d, want %d", got, tc.want)
			}
		})
	}
}
