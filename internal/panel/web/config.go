// Package web is the panel's HTTP surface: configuration, routing, and
// the middleware every request passes through.
//
// It sits between two packages that deliberately know nothing about
// each other. internal/panel holds the rules - who may see what, which
// settings are locked, what the audit log records - and never imports
// net/http for a decision. internal/panel/ui renders HTML and never
// decides anything. This package is where a request becomes a call into
// the first and a page out of the second.
package web

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/sealed"
)

// RolesConfig names the database role each service connects as.
//
// The panel never connects with any of them - it has its own DSN. They
// are here so the setup checks can ask what a *different* role may do.
type RolesConfig struct {
	Collector string `toml:"collector"`
	Beacon    string `toml:"beacon"`
	API       string `toml:"api"`
	Panel     string `toml:"panel"`
}

// Config is the panel's own TOML file.
//
// Separate from the collector's, the beacon's and the API's for the
// same reason those are separate from each other: four processes, four
// database roles. The panel's DSN must name a role that can reach the
// panel_* tables and nothing else - it reads analytics over HTTP from
// the read-only API, exactly as an external panel would, and a
// deployment that hands it broader rights looks completely healthy
// until the day it matters.
type Config struct {
	ListenAddr string `toml:"listen_addr"`
	// PanelDSN is the panel's database. Required: every page below the
	// login form reads from it, and a panel that starts without one
	// would only be able to report its own failure.
	PanelDSN string `toml:"panel_dsn"`
	// AnalyticsAPIURL is the read-only API this panel pulls numbers
	// from. Not required yet - no page reads it before group D - but
	// declared here so a deployment configured today does not need
	// editing later.
	AnalyticsAPIURL string `toml:"analytics_api_url"`
	// AnalyticsAPIToken is the bearer token for that API.
	AnalyticsAPIToken string `toml:"analytics_api_token"`

	// SecureCookies marks the session cookie Secure. True by default;
	// it exists as a switch only so a developer on plain HTTP over
	// loopback can log in at all.
	SecureCookies *bool `toml:"secure_cookies"`
	// HSTS asks browsers to refuse plain HTTP for a year. Off by
	// default and deliberately not tied to SecureCookies: this is the
	// kind of software somebody runs on a spare machine first and puts
	// a certificate on afterwards, and a wrong HSTS locks them out of a
	// panel that has no HTTPS to fall back to.
	HSTS bool `toml:"hsts"`
	// SessionLifetimeHours bounds how long a login lasts.
	SessionLifetimeHours int `toml:"session_lifetime_hours"`
	// Timezone is the IANA zone every date and time on every page is
	// rendered in. Not cosmetic: a panel that reports the evening
	// traffic peak in UTC tells a customer in Istanbul it happened in
	// the afternoon.
	Timezone string `toml:"timezone"`

	// BeaconURL is the public address the beacon is reached at.
	//
	// Used for one thing: printing the snippet the customer embeds in
	// their website. The panel cannot discover it - the beacon is a
	// separate process behind whatever proxy the deployment put there -
	// so an unset value produces a step that says where to get the
	// snippet instead of one printing a tag that points nowhere.
	BeaconURL string `toml:"beacon_url"`

	// Roles names the database roles each service connects as.
	//
	// The panel never uses them to connect - it has its own DSN. They
	// exist so the setup checks can ask what a *different* role may do,
	// which is the deployment's whole security foundation: the panel
	// must not be able to read the analytics tables, and the API must
	// not be able to write.
	//
	// Unset means those checks cannot run, and a check that cannot run
	// blocks handover. That is deliberate and it is loud: a deployment
	// handed over without its isolation ever having been verified is
	// exactly the one where nobody finds out until it matters.
	Roles RolesConfig `toml:"roles"`
	// Language is the deployment's preferred language, by code ("tr",
	// "en"). It is a preference rather than a restriction: a reader
	// whose browser asks for another language this build carries gets
	// that one, which is what serves a colleague on a team that does not
	// all read the same language. This is the answer when the browser
	// expresses no preference the panel can serve.
	//
	// A code no pack declares is a startup error, checked in cmd/panel
	// where the packs are loaded - otherwise the deployment would run in
	// the base language while its config file named another.
	Language string `toml:"language"`

	// BotDataPath is the known-bot fingerprint file the collector reads
	// and writes. Declared here only so the setup wizard can report
	// whether it has ever been fetched; the panel never writes it.
	//
	// Empty makes that check a skip rather than a failure - "we did not
	// look" and "we looked and it was missing" are different facts.
	BotDataPath string `toml:"bot_data_path"`

	// DeveloperGate carries the hashed password guarding the settings
	// with legal weight. Absent means those settings cannot be changed
	// from the panel by anyone, which is the safe direction.
	DeveloperGate devgate.Config `toml:"developer_gate"`

	// SecretKey encrypts the one secret this product stores that it has
	// to be able to read back: the outgoing SMTP password. 32 bytes, as
	// 64 hex characters or base64. See internal/sealed.
	//
	// Empty is a supported state and means exactly one thing: no mail
	// account can be saved. The panel says so on the mail page and every
	// other page works, which is the same shape as every other optional
	// piece here - and much better than a panel that starts, accepts a
	// password, and stores it where a database dump hands it over.
	//
	// Losing or changing it does not break the panel and does not lose
	// anything else: the sealed password stops opening, the mail page
	// says so, and somebody types the password again. That is the whole
	// blast radius, and it is small on purpose - the key guards one
	// column rather than being woven through the schema.
	SecretKey string `toml:"secret_key"`

	Logging logging.Config `toml:"logging"`
}

// DefaultSessionLifetime is twelve hours: long enough for a working
// day, short enough that a laptop left open overnight is not a session.
const DefaultSessionLifetime = 12 * time.Hour

// LoadConfig reads and validates the panel config at path.
func LoadConfig(path string) (Config, error) {
	cfg := Config{
		// Loopback by default. The panel is an administrative surface;
		// putting it on a public interface should be an edit somebody
		// made on purpose, not something that happens by omission.
		ListenAddr:           "127.0.0.1:8090",
		SessionLifetimeHours: 12,
		Timezone:             "Europe/Istanbul",
		Language:             "tr",
	}
	if _, err := os.Stat(path); err != nil {
		return Config{}, fmt.Errorf("panel: config file %s: %w", path, err)
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return Config{}, fmt.Errorf("panel: parse %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	if c.PanelDSN == "" {
		return fmt.Errorf("panel: panel_dsn is required (the panel's own postgres role, with no access to the analytics tables)")
	}
	if c.SessionLifetimeHours < 1 || c.SessionLifetimeHours > 24*30 {
		return fmt.Errorf("panel: session_lifetime_hours %d is out of range (1..720)", c.SessionLifetimeHours)
	}
	if _, err := c.Location(); err != nil {
		return err
	}
	if _, err := logging.ParseLevel(c.Logging.Level); err != nil {
		return fmt.Errorf("panel: %w", err)
	}
	// The gate refuses a plaintext password in the file and says so
	// itself; surface that at startup rather than at the first attempt
	// to change a guarded setting.
	if _, err := devgate.New(c.DeveloperGate, devgate.Options{}); err != nil {
		return fmt.Errorf("panel: %w", err)
	}
	// A malformed key is a startup error while an absent one is not.
	//
	// The two are different situations and only one of them is a
	// mistake: nothing set means mail was never configured, and a
	// truncated or mistyped 32 bytes means somebody intended to
	// configure it. Letting the second start would produce a panel that
	// reports "this password cannot be decrypted" about every password
	// it was ever given, with the reason sitting in a config file
	// nobody has cause to re-read.
	if _, err := c.Secrets(); err != nil && !errors.Is(err, sealed.ErrNoKey) {
		return fmt.Errorf("panel: secret_key: %w", err)
	}
	return nil
}

// Secrets is the configured encryption key.
//
// Returns sealed.ErrNoKey when none is set, which callers treat as "mail
// cannot be configured" rather than as a failure.
func (c Config) Secrets() (sealed.Key, error) {
	return sealed.ParseKey(c.SecretKey)
}

// Location resolves the configured zone.
//
// An unknown name is an error rather than a silent fall back to UTC.
// Falling back would put every timestamp in the panel an hour or three
// away from the customer's clock while the config file said otherwise -
// wrong, and invisible.
func (c Config) Location() (*time.Location, error) {
	if c.Timezone == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return nil, fmt.Errorf("panel: timezone %q: %w (the host needs tzdata, or name a zone it has)", c.Timezone, err)
	}
	return loc, nil
}

// SessionLifetime is the configured lifetime as a duration.
func (c Config) SessionLifetime() time.Duration {
	if c.SessionLifetimeHours <= 0 {
		return DefaultSessionLifetime
	}
	return time.Duration(c.SessionLifetimeHours) * time.Hour
}

// CookiesAreSecure reports the effective setting. Absent means true:
// the safe value has to be the one you get by not thinking about it.
func (c Config) CookiesAreSecure() bool {
	return c.SecureCookies == nil || *c.SecureCookies
}
