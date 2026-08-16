package beacon

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/privacy"
	"github.com/cruciblelab/crucible-analytic/internal/retention"
	"github.com/cruciblelab/crucible-analytic/internal/settings"
)

// siteIDPattern must stay identical to config.siteIDPattern in the
// collector: a beacon accepting a site_id the collector could never
// have written would produce beacon_events rows that no
// traffic_snapshots row can ever join to.
var siteIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// paramNamePattern bounds a configured extra query parameter name.
// Narrow on purpose: the name is compared against what a browser sent,
// never interpolated anywhere, and keeping it to an obvious character
// set means a typo in the config is an error at startup rather than a
// parameter that silently never matches.
var paramNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

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
	Campaign       CampaignConfig  `toml:"campaign"`
	Privacy        PrivacyConfig   `toml:"privacy"`
	Retention      RetentionConfig `toml:"retention"`
	Settings       SettingsConfig  `toml:"settings"`
	Logging        logging.Config  `toml:"logging"`
}

// PrivacyConfig decides what personal data reaches the disk. It mirrors
// the collector's section of the same name, and it has to: the two write
// the columns the crossover join compares, so a deployment that masked
// one and not the other would produce a join that finds nothing.
type PrivacyConfig struct {
	// IPStorage is "masked" (the default), "full" or "hashed". Empty
	// means masked - a config file written before this setting existed
	// stores less rather than more.
	IPStorage string `toml:"ip_storage"`
	// IPHashKey keys the pseudonym in hashed mode.
	//
	// Both writers must carry the same key or the crossover join finds
	// nothing, and the failure is silent - which is why the key lives in
	// a file an operator copies between the two rather than being
	// generated per process.
	IPHashKey string `toml:"ip_hash_key"`
}

// HashKey returns the configured key as bytes.
func (p PrivacyConfig) HashKey() []byte { return []byte(p.IPHashKey) }

// IPMode resolves the configured value, defaulting to masked.
func (p PrivacyConfig) IPMode() privacy.IPMode { return privacy.ParseIPMode(p.IPStorage) }

// Live resolves the mode in force, preferring the panel's setting over
// the config file and falling back to the file when nothing is stored.
func (p PrivacyConfig) Live(source *settings.Source) privacy.IPMode {
	if source == nil {
		return p.IPMode()
	}
	return privacy.ParseIPMode(source.String(
		settings.KeyPrivacyIPStorage, "", string(p.IPMode()),
		[]string{string(privacy.IPFull), string(privacy.IPMasked), string(privacy.IPHashed)}))
}

// CampaignConfig tunes which query parameters reach the database.
//
// It is configuration rather than a constant because the answer is a
// legal question as much as a technical one, and legal answers differ
// per deployment and change after the fact. See CampaignPolicy.
type CampaignConfig struct {
	// DropParams removes standard parameters this deployment refuses.
	//
	// utm_term is the usual candidate: it normally holds the keyword an
	// advertiser bid on, but an ad platform can be configured to
	// substitute the visitor's actual search text instead - so whether
	// it may be stored depends on how the customer advertises and what
	// their counsel says about it.
	DropParams []string `toml:"drop_params"`
	// ExtraParams keeps additional, non-standard parameters. They appear
	// in the stored query string but get no column of their own, so they
	// are visible rather than groupable.
	//
	// Every addition here is a decision to store something this project
	// has no control over the contents of. Add a name only after
	// checking what the site actually puts in it.
	ExtraParams []string `toml:"extra_params"`
	// StoreClickIDs keeps the raw gclid/fbclid/msclkid value rather than
	// only which network it came from. False by default - see the
	// click_id column comment in schema.sql.
	StoreClickIDs bool `toml:"store_click_ids"`
}

// Policy turns the config into the runtime policy.
func (c CampaignConfig) Policy() CampaignPolicy {
	return NewCampaignPolicy(c.DropParams, c.ExtraParams, c.StoreClickIDs)
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
	for _, name := range c.Campaign.DropParams {
		if _, known := indexOf(standardParams, strings.ToLower(strings.TrimSpace(name))); !known {
			return fmt.Errorf("beacon: campaign.drop_params %q is not a standard parameter (one of %s)",
				name, strings.Join(standardParams, ", "))
		}
	}
	for _, name := range c.Campaign.ExtraParams {
		if !paramNamePattern.MatchString(strings.TrimSpace(name)) {
			return fmt.Errorf("beacon: invalid campaign.extra_params entry %q (want 1-64 characters, letters/digits/underscore/dash)", name)
		}
	}
	if _, err := logging.ParseLevel(c.Logging.Level); err != nil {
		return fmt.Errorf("beacon: %w", err)
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

// SettingsConfig controls how often live settings are re-read.
type SettingsConfig struct {
	// IntervalSeconds between reads. Zero takes the package default.
	IntervalSeconds int `toml:"interval_seconds"`
}

// Interval is the polling period.
func (s SettingsConfig) Interval() time.Duration {
	if s.IntervalSeconds <= 0 {
		return time.Minute
	}
	return time.Duration(s.IntervalSeconds) * time.Second
}

// Live overlays whatever the panel has stored onto the file's campaign
// configuration, and returns the resulting policy.
//
// The file is the fallback for every field, so an empty or unreachable
// settings table leaves behaviour exactly as configured. That ordering
// is the point: turning on live settings must not be able to change what
// a deployment does until somebody deliberately sets something.
func (c CampaignConfig) Live(src *settings.Source) CampaignPolicy {
	drop := src.Strings(settings.KeyCampaignDropParams, "", c.DropParams)
	extra := src.Strings(settings.KeyCampaignExtraParams, "", c.ExtraParams)
	storeClickIDs := src.Bool(settings.KeyCampaignStoreClickID, "", c.StoreClickIDs)
	return NewCampaignPolicy(drop, extra, storeClickIDs)
}

// RetentionPolicy resolves how long this deployment keeps analytics
// data, preferring the panel's settings over the config file.
//
// The per-site figures come from the sites this process is configured to
// accept. A site nobody serves cannot have its retention read, and does
// not need to be: nothing is writing rows for it.
func (c Config) RetentionPolicy(source *settings.Source, sites []string) retention.Policy {
	policy := retention.Policy{Days: c.Retention.Resolved(), PerSite: map[string]int{}}
	if source == nil {
		return policy
	}

	policy.Days = source.Int(settings.KeyAnalyticsRetention, "", policy.Days, retention.MinDays, retention.MaxDays)
	for _, site := range sites {
		days := source.Int(settings.KeyAnalyticsRetention, site, policy.Days, retention.MinDays, retention.MaxDays)
		if days != policy.Days {
			// Only the sites that differ. An entry equal to the
			// deployment-wide figure would put a site on the row-delete
			// path for no reason - see internal/retention.
			policy.PerSite[site] = days
		}
	}
	return policy
}

// RetentionConfig is the [retention] section.
type RetentionConfig struct {
	// Days is the fallback when the panel has no figure stored. Zero
	// takes DefaultRetentionDays.
	Days int `toml:"days"`
	// IntervalHours is how often the policy is re-applied. Zero takes
	// the default.
	//
	// Hourly rather than on the settings tick: applying is idempotent,
	// but a site with a shorter retention than the deployment gets a
	// row-level delete each time, and running that every minute would
	// scan for nothing sixty times an hour.
	IntervalHours int `toml:"interval_hours"`
}

// DefaultRetentionDays matches the panel's own default and the read
// API's maximum range.
const DefaultRetentionDays = 90

// Resolved is the configured retention, or the default when the file
// says nothing usable. Out-of-range is treated as unset rather than
// clamped: a config saying 20000 days is a mistake, and silently turning
// it into ten years would hide it.
func (r RetentionConfig) Resolved() int {
	if r.Days >= retention.MinDays && r.Days <= retention.MaxDays {
		return r.Days
	}
	return DefaultRetentionDays
}

// Interval resolves how often to re-apply, defaulting to an hour.
func (r RetentionConfig) Interval() time.Duration {
	if r.IntervalHours > 0 {
		return time.Duration(r.IntervalHours) * time.Hour
	}
	return time.Hour
}
