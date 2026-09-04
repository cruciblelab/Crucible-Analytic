package applier

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/releasesign"
)

// Config is the applier's own TOML file.
//
// Its own, like the other four services', and for a sharper reason than
// theirs: this is the only configuration file in the deployment that
// carries a DSN able to run DDL. Keeping it separate is what lets its
// permissions be tighter than the panel's - a file the panel's user
// cannot read at all.
type Config struct {
	// SchemaAdminDSN names the fifth role, which owns the tables.
	//
	// Required, and checked to be nothing else: an applier pointed at
	// the panel's role would fail on the first ALTER, half way through a
	// migration, rather than at startup.
	SchemaAdminDSN string `toml:"schema_admin_dsn"`

	// Interval is how often to look for a waiting request.
	//
	// The queue is a table, not a channel, so this is a poll. Thirty
	// seconds by default: an upgrade is something a person just clicked
	// and is watching a page for, so minutes would feel broken, and the
	// query is one indexed lookup that finds nothing almost every time.
	IntervalSeconds int `toml:"interval_seconds"`

	Logging logging.Config `toml:"logging"`

	// Release is where new versions come from and how they are checked.
	//
	// Here rather than in the database, and that placement is the whole
	// security argument for the panel's update button. This file is
	// mode 0640 owned by crucible-upgrader; the four services run as
	// crucible and cannot read it. So a compromised panel can write a
	// row asking for a version, and cannot influence where the package
	// comes from or what key it is checked against.
	//
	// A base_url in the database would give it the first. A public key
	// in the database would give it the second. Either alone is enough
	// to make the button a way to run code.
	Release ReleaseConfig `toml:"release"`
}

// ReleaseConfig is the [release] table.
//
// Absent by default, and absence means the update button does nothing:
// V5 shows the section only when this is configured. That is the safe
// direction - a deployment that has not been told where its packages
// come from must not guess.
type ReleaseConfig struct {
	// BaseURL is the directory packages live under. The upgrader
	// appends the version and the file name; nothing from the request
	// row reaches this string except a version whose shape
	// relupdate.ValidVersion has already checked.
	//
	// https only, checked in Validate. A plain-http base_url would let
	// anybody on the path serve the package - and while the signature
	// would still refuse a substituted one, it would not refuse a
	// *stale* one. Downgrading a customer to a version with a known
	// hole is an attack a signature does not stop, because we signed
	// that version too.
	BaseURL string `toml:"base_url"`

	// PublicKey verifies the SHA256SUMS in the package. See
	// internal/releasesign.
	//
	// Required whenever BaseURL is set. An update path with no key is
	// not a slightly weaker update path, it is a remote code execution
	// feature, so the two are validated together rather than
	// independently.
	PublicKey string `toml:"public_key"`

	// Prefix is the installation root; binaries live in Prefix/bin.
	//
	// Defaults to /opt/crucible-analytic, which is what install.sh uses.
	// Configurable because a deployment that installed somewhere else
	// would otherwise have an update button that replaced binaries in a
	// directory nothing runs from - and that failure is silent: the
	// install succeeds, the services keep running the old ones, and the
	// page says it worked.
	Prefix string `toml:"prefix"`
}

// DefaultPrefix is where install.sh puts things.
const DefaultPrefix = "/opt/crucible-analytic"

// InstallPrefix is Prefix with the default applied.
func (r ReleaseConfig) InstallPrefix() string {
	if r.Prefix != "" {
		return r.Prefix
	}
	return DefaultPrefix
}

// Interval is the poll interval, with the default applied.
func (c Config) Interval() time.Duration {
	if c.IntervalSeconds <= 0 {
		return 30 * time.Second
	}
	return time.Duration(c.IntervalSeconds) * time.Second
}

// Load reads and validates the file.
func Load(path string) (Config, error) {
	var c Config
	body, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("upgrader: reading %s: %w", path, err)
	}
	if err := toml.Unmarshal(body, &c); err != nil {
		return c, fmt.Errorf("upgrader: parsing %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

// Validate refuses a configuration that would fail later instead of now.
func (c Config) Validate() error {
	if c.SchemaAdminDSN == "" {
		return errors.New("upgrader: schema_admin_dsn is required " +
			"(the fifth role, which owns the tables; install.sh creates it and writes " +
			"its password into this file)")
	}
	return c.Release.Validate()
}

// Validate checks the release settings, which are all-or-nothing.
//
// # Why the halves are checked together
//
// A base_url with no public key is not a slightly weaker update path.
// It is a feature that downloads code from the network and runs it, and
// the only thing that was ever going to stop that is the signature. So
// a file carrying one and not the other is a mistake this refuses to
// start on rather than one it works around - the alternative is a
// deployment that looks configured and installs anything.
//
// A key with no base_url is harmless and still refused, because it means
// somebody believed they had configured updates and had not. Silence
// there would be found out at the worst moment: pressing the button.
func (r ReleaseConfig) Validate() error {
	switch {
	case r.BaseURL == "" && r.PublicKey == "":
		// Not configured, which is the default and a supported state.
		return nil
	case r.BaseURL == "":
		return errors.New("upgrader: [release] public_key is set and base_url is not, " +
			"so nothing can be fetched. Set both or neither")
	case r.PublicKey == "":
		return errors.New("upgrader: [release] base_url is set and public_key is not. " +
			"An update path with no signature to check is not a weaker update path, " +
			"it is a way to run code from the network. Set both or neither")
	}

	u, err := url.Parse(r.BaseURL)
	if err != nil {
		return fmt.Errorf("upgrader: [release] base_url is not a URL: %w", err)
	}
	if u.Scheme != "https" {
		// The signature would still refuse a substituted package, so
		// this is not about substitution. It is about a *stale* one:
		// anybody on the path could serve a genuinely signed older
		// release with a hole in it, and no signature refuses a version
		// we really did sign.
		return fmt.Errorf("upgrader: [release] base_url must be https, not %q. "+
			"Over plain http anybody on the path can serve an older release we "+
			"genuinely signed, and a signature does not refuse that", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("upgrader: [release] base_url has no host")
	}
	if _, err := releasesign.ParsePublicKey(r.PublicKey); err != nil {
		return fmt.Errorf("upgrader: [release] public_key: %w", err)
	}
	return nil
}

// Key returns the parsed verifying key, or a zero key when updates are
// not configured.
//
// A zero key verifies nothing - see releasesign.PublicKey.IsSet - so a
// caller that forgets to check IsSet refuses packages rather than
// accepting them. That is the direction this has to fail in.
func (r ReleaseConfig) Key() releasesign.PublicKey {
	k, err := releasesign.ParsePublicKey(r.PublicKey)
	if err != nil {
		return releasesign.PublicKey{}
	}
	return k
}

// Configured reports whether this deployment can fetch updates at all.
func (r ReleaseConfig) Configured() bool {
	return r.BaseURL != "" && r.Key().IsSet()
}
