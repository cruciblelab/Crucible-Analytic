// Package backup takes a copy of the data and records what it took.
//
// # Why pg_dump is not used here, measured rather than assumed
//
// The obvious implementation is `pg_dump --table=traffic_snapshots`.
// It produces a file. The file restores. It contains no rows.
//
// A hypertable's rows do not live in the table named on the command
// line: they live in chunks, in _timescaledb_internal, and pg_dump's
// --table filter does not follow them. Measured on a real database with
// real data:
//
//	rows in traffic_snapshots                    8050
//	bytes in the table-filtered dump             3957
//	rows after restoring that dump into a
//	fresh database                                  0
//
// No error, no warning, a plausible file, and nothing in it. That is the
// worst shape a backup feature can have - it fails only when somebody
// needs it, which is the one moment there is no second chance.
//
// So the data comes out through COPY, table by table, which follows the
// chunks because it is an ordinary query. Restoring the same data
// through COPY produced 8050 rows in 6 chunks, matching the original.
//
// # And why the schema does not come out with it
//
// It is already in the binary. internal/schemafiles carries every
// schema.sql and internal/schemaver fingerprints them, so a restore
// builds the tables from the same bytes an install does. A dump that
// carried its own DDL would be a second definition of the schema, and
// the two would agree until they did not.
//
// # The file
//
//	<name>.tar.gz
//	  manifest.json        what is inside, and what it came from
//	  data/<table>.copy    PostgreSQL COPY text, one file per table
//
// tar and gzip from the standard library, so the upgrader needs no
// postgresql-client and no external binary of any kind.
package backup

import (
	"fmt"
	"sort"
	"strings"
)

// A Set is a group of tables somebody can choose to include.
//
// # Why the customer chooses
//
// Because the sets differ by three orders of magnitude and by what they
// cost to lose. The panel tables are small and irreplaceable: accounts,
// sites, settings, the audit log. The traffic tables are the product and
// they are large. A machine with room for one and not the other should
// be able to keep the one that cannot be rebuilt.
type Set struct {
	// Name is what the row carries and the page shows a label for.
	Name string
	// Tables are the tables in it, in the order they are dumped.
	Tables []string
	// Secrets marks the one set that is not tables at all: the
	// configuration files, sealed to the developer password.
	//
	// A field rather than "Tables is empty", although that would be
	// derivable. A table set that lost its tables in an edit would then
	// silently become a secrets set - a backup that reported success,
	// contained the configuration, and contained none of the data
	// somebody asked for. The field says which kind this is, and
	// TestExactlyOneSetIsSecrets checks the two never disagree.
	Secrets bool
}

// Set names. A closed list, because a request naming a set this build
// does not know must be refused rather than skipped: "backup taken"
// beside an empty file is the failure this package exists to avoid.
const (
	// SetAnalitik is the traffic itself.
	SetAnalitik = "analitik"
	// SetPanel is the accounts, sites, settings and the audit log.
	SetPanel = "panel"
)

// Sets are the sets this build knows, in the order a page shows them.
var Sets = []Set{
	{
		Name: SetAnalitik,
		// Both hypertables. These are the ones pg_dump gets wrong, and
		// the reason this package copies rather than dumps.
		Tables: []string{"traffic_snapshots", "beacon_events"},
	},
	{
		Name: SetPanel,
		Tables: []string{
			"panel_users",
			"panel_site_members",
			"panel_settings",
			"panel_api_tokens",
			"panel_recovery_codes",
			"panel_smtp",
			"panel_audit_log",
			"panel_dev_access",
			"panel_owner_claims",
		},
	},
	{
		// The configuration, not the database. See secrets.go for why
		// it is a set at all rather than a separate button: it goes
		// through the same queue, the same catalogue and the same page,
		// and the one thing it may never do is share a file with the
		// two above.
		Name:    SetSirlar,
		Secrets: true,
	},
}

// Excluded are tables that are deliberately in no set, with the reason.
//
// Written down rather than left out. A table that is simply absent from
// the lists above is indistinguishable from one somebody forgot, and an
// invariant reads this map so a new table has to be placed on purpose.
var Excluded = map[string]string{
	// Re-downloadable, and 135 MB of it. Backing up a public dataset
	// would be the largest thing in the file and the only thing in it
	// that can be fetched again by asking.
	"ip_asn_ranges":     "public dataset, re-downloaded on demand",
	"ip_country_ranges": "public dataset, re-downloaded on demand",
	"ip_range_fetches":  "a log of those downloads, meaningless without them",

	// Live tokens. Restoring them would restore somebody's open session
	// on a machine that has been rebuilt, which is the one thing a
	// restore must not do. They expire anyway.
	"panel_sessions": "live session tokens; restoring a session is not restoring data",
	// Rate-limiter state about failed logins. Seconds old by design.
	"panel_login_attempts": "rate-limiter state, meaningless after a restore",

	// Queues. Every row is an instruction to a process, and replaying
	// one after a restore would re-run work that already happened - or
	// worse, work that failed.
	"panel_upgrade_requests":    "a queue; restoring it would replay instructions",
	"panel_release_requests":    "a queue; restoring it would replay instructions",
	"panel_backup_requests":     "a queue; restoring it would replay instructions",
	"ip_range_refresh_requests": "a queue; restoring it would replay instructions",
	"panel_release_available":   "one row of cached fact, refreshed within six hours",
	"panel_operations":          "in-flight operation state",

	// Facts about this machine, which the restored machine is not.
	"service_heartbeat": "what these services were doing on the old machine",
	"schema_version":    "recorded by the applier; a restored row would claim a state",
	"panel_logs":        "the log sink; large, and about the machine rather than the customer",
	"panel_backups":     "the catalogue of backups on the old machine's disk",
}

// SetByName finds a set, or says the name is not one.
func SetByName(name string) (Set, error) {
	for _, s := range Sets {
		if s.Name == name {
			return s, nil
		}
	}
	return Set{}, fmt.Errorf("backup: %q is not a set this build knows (%s)",
		name, strings.Join(SetNames(), ", "))
}

// SetNames is every set name, for messages and for callers building a
// request.
func SetNames() []string {
	out := make([]string, 0, len(Sets))
	for _, s := range Sets {
		out = append(out, s.Name)
	}
	return out
}

// TablesFor resolves a request's sets into the tables to copy.
//
// Refuses an unknown name rather than ignoring it. A request written by
// an older panel, naming a set this build renamed, must fail loudly:
// the alternative is a backup that reports success and contains less
// than the person who asked for it believes.
//
// Duplicates are dropped and the order is the order of Sets, so the same
// request always produces the same file.
func TablesFor(sets []string) ([]string, error) {
	if len(sets) == 0 {
		return nil, fmt.Errorf("backup: no set was named; there would be nothing in the file")
	}
	seen := map[string]bool{}
	var out []string
	for _, name := range sets {
		s, err := SetByName(name)
		if err != nil {
			return nil, err
		}
		if s.Secrets {
			// Refused rather than contributing nothing.
			//
			// Without this, a request naming only the secrets set would
			// resolve to an empty table list, and the data writer would
			// produce a valid archive with a manifest, a checksum and a
			// catalogue row - containing nothing. Callers route on
			// KindOf and should never reach here; this is what makes
			// "should never" produce an error rather than an empty
			// backup.
			return nil, fmt.Errorf("backup: %q is not a set of tables; it is the "+
				"configuration, and it is written to its own file", s.Name)
		}
		for _, table := range s.Tables {
			if seen[table] {
				continue
			}
			seen[table] = true
			out = append(out, table)
		}
	}
	return out, nil
}

// Normalise puts a request's sets in a fixed order with no duplicates,
// so two requests for the same thing produce the same row.
func Normalise(sets []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range Sets {
		for _, want := range sets {
			if want == s.Name && !seen[want] {
				seen[want] = true
				out = append(out, want)
			}
		}
	}
	// Anything left is unknown; kept so the caller's validation can
	// report it rather than having it silently disappear here.
	var unknown []string
	for _, want := range sets {
		if !seen[want] {
			seen[want] = true
			unknown = append(unknown, want)
		}
	}
	sort.Strings(unknown)
	return append(out, unknown...)
}
