package applier

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/cruciblelab/crucible-analytic/internal/logging"
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
	return nil
}
