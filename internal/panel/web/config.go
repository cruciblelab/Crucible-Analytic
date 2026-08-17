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
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/logging"
)

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
	return nil
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
