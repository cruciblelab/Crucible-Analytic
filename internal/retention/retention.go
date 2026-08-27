// Package retention bounds how long analytics data is kept.
//
// Until this existed, nothing in the project deleted a visit record.
// Both hypertables grew forever, on a machine the customer also serves
// their site from, and the first symptom of a full disk would have been
// the collector failing to write - an analytics feature taking down the
// traffic path, which is the one outcome this project refuses
// everywhere else.
//
// # Chunks, not DELETE
//
// TimescaleDB stores a hypertable as time-ranged chunks. Dropping a
// whole chunk unlinks a file; deleting rows rewrites pages, updates
// indexes, and leaves the space to VACUUM. On a year of traffic those
// are not two versions of the same operation. So the policy that runs
// every day is a chunk-drop, and row deletion is reserved for the one
// case chunks cannot express.
//
// # The case chunks cannot express
//
// Retention is a per-site setting, and a chunk holds every site's rows
// for its time range. There is no way to drop "site A's rows older than
// 30 days" by dropping a chunk.
//
// So the two are split by what each is good at:
//
//   - The hypertable policy uses the *longest* retention any site on
//     this deployment asks for. It is cheap, it runs daily, and it can
//     never remove data a site still wants.
//   - A site asking for less than that gets the difference removed by a
//     targeted delete. Only for that site, only when a shorter value is
//     actually configured, and never at all in the ordinary case where
//     every site uses the deployment-wide number.
//
// Doing it the other way round - a policy at the shortest value - would
// silently destroy the data of every site that asked to keep more, and
// it would look like the feature working.
package retention

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Tables this package manages. A closed set, because these names are
// interpolated into SQL that add_retention_policy will not accept as a
// bound parameter - so the only names that can ever reach it are the
// ones written here.
type Table string

const (
	// TableTrafficSnapshots is the collector's, one row per active
	// address per flush.
	TableTrafficSnapshots Table = "traffic_snapshots"
	// TableBeaconEvents is the beacon's, one row per pageview or event.
	TableBeaconEvents Table = "beacon_events"
)

// Valid reports whether t is a table this package knows.
//
// Checked at every entry point rather than trusted, because Table is a
// string type: a caller can write Table(userInput) and the compiler will
// not object. This is the check that makes that harmless.
func (t Table) Valid() bool {
	return t == TableTrafficSnapshots || t == TableBeaconEvents
}

// Bounds on a retention setting.
//
// Enforced here rather than in a config parser because both services
// reach this package and neither should be able to apply a policy the
// other would refuse. A file asking for more is a mistake, and Apply
// says so instead of clamping - a deployment that thinks it keeps five
// years and keeps two would find out from a customer, not from us.
const (
	// MinDays is one day. Zero would mean "delete everything", and there
	// is no way to type that by accident here.
	MinDays = 1
	// MaxDays is two years.
	//
	// It was ten, chosen as "the point past which keep it and keep it
	// forever stop differing". That was a statement about arithmetic and
	// not about the law this project is written under: visit records are
	// personal data, KVKK asks that they be kept for as long as the
	// purpose needs and no longer, and a product whose ceiling is a
	// decade invites a deployment nobody can defend.
	//
	// Two years rather than one because the honest use for old analytics
	// is "the same month last year", and a ceiling of 365 makes that
	// comparison impossible on the last day it is needed.
	MaxDays = 730
)

// Policy is what one deployment wants kept.
type Policy struct {
	// Days is the deployment-wide retention, applied to every site that
	// has no figure of its own.
	Days int
	// PerSite overrides Days for named sites. A site absent from this
	// map uses Days.
	PerSite map[string]int
}

// longest is the retention the hypertable policy must use: no chunk may
// be dropped while any site still wants what is in it.
func (p Policy) longest() int {
	longest := p.Days
	for _, days := range p.PerSite {
		if days > longest {
			longest = days
		}
	}
	return longest
}

// shorter lists the sites wanting less than the hypertable policy keeps,
// sorted so the work is deterministic and a log line reads the same way
// twice.
func (p Policy) shorter() []string {
	longest := p.longest()
	var sites []string
	for site, days := range p.PerSite {
		if days < longest {
			sites = append(sites, site)
		}
	}
	sort.Strings(sites)
	return sites
}

// Validate reports whether this policy can be applied at all.
func (p Policy) Validate() error {
	if p.Days < MinDays || p.Days > MaxDays {
		return fmt.Errorf("retention: %d days is outside %d..%d", p.Days, MinDays, MaxDays)
	}
	for site, days := range p.PerSite {
		if days < MinDays || days > MaxDays {
			return fmt.Errorf("retention: site %q asks for %d days, outside %d..%d", site, days, MinDays, MaxDays)
		}
	}
	return nil
}

// Manager applies a policy to one table.
//
// One per table rather than one for both, because the two tables are
// owned by different processes with different database roles: the
// collector may alter traffic_snapshots and the beacon may alter
// beacon_events, and neither may touch the other's. A single manager
// would need a role that could do both, which would undo the separation
// the whole deployment rests on.
type Manager struct {
	pool  *pgxpool.Pool
	table Table
}

// NewManager builds a manager for one table.
func NewManager(pool *pgxpool.Pool, table Table) (*Manager, error) {
	if !table.Valid() {
		return nil, fmt.Errorf("retention: unknown table %q", table)
	}
	return &Manager{pool: pool, table: table}, nil
}

// Report is what an Apply did, or what a DryRun would do.
type Report struct {
	Table Table
	// PolicyDays is the retention the hypertable policy now holds.
	PolicyDays int
	// PolicyChanged reports whether that differed from what was there.
	PolicyChanged bool
	// PreviousDays is what the policy held before, zero when there was
	// none.
	PreviousDays int
	// SiteRows counts rows a per-site trim removed, or would remove.
	SiteRows map[string]int64
	// Skipped explains why nothing was done, when nothing was.
	Skipped string
}

// Rows totals the per-site work.
func (r Report) Rows() int64 {
	var total int64
	for _, n := range r.SiteRows {
		total += n
	}
	return total
}

// Current reads the retention the table's policy holds now, in days.
//
// Zero means no policy - which, before this package existed, was every
// deployment.
func (m *Manager) Current(ctx context.Context) (int, error) {
	var interval *time.Duration
	err := m.pool.QueryRow(ctx, `
		SELECT (config->>'drop_after')::interval
		FROM timescaledb_information.jobs
		WHERE proc_name = 'policy_retention' AND hypertable_name = $1`,
		string(m.table)).Scan(&interval)
	if err != nil {
		// No row is the ordinary "no policy yet" case, not a failure.
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("retention: read current policy for %s: %w", m.table, err)
	}
	if interval == nil {
		return 0, nil
	}
	return int(interval.Hours() / 24), nil
}

// DryRun reports what Apply would do, without doing it.
//
// This exists because shortening retention destroys data, and "90 days
// to 30" is a number somebody types without picturing what it removes.
// A panel that can say "this will delete 4.2 million rows" before the
// button is pressed is telling the truth at the only moment it is
// useful.
func (m *Manager) DryRun(ctx context.Context, policy Policy) (Report, error) {
	return m.run(ctx, policy, true)
}

// Apply installs the hypertable policy and performs any per-site trim.
func (m *Manager) Apply(ctx context.Context, policy Policy) (Report, error) {
	return m.run(ctx, policy, false)
}

func (m *Manager) run(ctx context.Context, policy Policy, dry bool) (Report, error) {
	if !m.table.Valid() {
		return Report{}, fmt.Errorf("retention: unknown table %q", m.table)
	}
	if err := policy.Validate(); err != nil {
		return Report{}, err
	}

	report := Report{Table: m.table, PolicyDays: policy.longest(), SiteRows: map[string]int64{}}

	previous, err := m.Current(ctx)
	if err != nil {
		return report, err
	}
	report.PreviousDays = previous
	report.PolicyChanged = previous != report.PolicyDays

	if !dry && report.PolicyChanged {
		if err := m.setPolicy(ctx, report.PolicyDays); err != nil {
			return report, err
		}
	}

	// The per-site trim. Skipped entirely when every site uses the
	// deployment-wide figure, which is the ordinary case - so the
	// expensive path costs nothing until somebody actually asks for it.
	for _, site := range policy.shorter() {
		days := policy.PerSite[site]
		n, err := m.trimSite(ctx, site, days, dry)
		if err != nil {
			return report, err
		}
		if n > 0 || dry {
			report.SiteRows[site] = n
		}
	}
	return report, nil
}

// setPolicy installs or replaces the hypertable's retention policy.
//
// Through ca_set_retention rather than TimescaleDB's own function, and
// that is not indirection for its own sake. add_retention_policy
// requires the caller to *own* the hypertable - measured, on a
// database installed by release/install.sh:
//
//	ERROR:  must be owner of hypertable "traffic_snapshots"
//
// even with EXECUTE granted explicitly. The tables belong to the
// superuser that installed them, as they should, so this package can
// only ever reach them through something that runs as their owner. See
// schema.sql for the whole argument, including what the wrapper
// deliberately does not allow.
//
// Both arguments are bound parameters now, the table included: it is a
// value to this function rather than an identifier, which is a smaller
// surface than the closed-set-of-constants rule that used to be the
// only thing keeping it safe.
func (m *Manager) setPolicy(ctx context.Context, days int) error {
	if _, err := m.pool.Exec(ctx,
		`SELECT ca_set_retention($1, $2)`, string(m.table), days); err != nil {
		return fmt.Errorf("retention: set policy on %s: %w", m.table, err)
	}
	return nil
}

// trimSite removes one site's rows older than its own shorter figure, or
// counts them when dry.
//
// A row-level delete, and the only one in this package. It runs solely
// for a site that asked to keep less than the deployment does, which is
// the one thing chunk-dropping cannot express - see the package comment.
// Through the same privileged wrappers as the policy, and for a
// sharper reason here: grants.sql gives the collector SELECT and
// INSERT and the beacon INSERT alone. Neither has ever held DELETE, so
// this path failed on every installed deployment - and giving them
// DELETE to fix it would let either erase its table entirely.
// ca_trim_site_rows can only remove one named site's rows past a
// bounded age, which is all this function has ever wanted to do.
func (m *Manager) trimSite(ctx context.Context, site string, days int, dry bool) (int64, error) {
	fn := `SELECT ca_trim_site_rows($1, $2, $3)`
	what := "trim"
	if dry {
		fn = `SELECT ca_count_site_rows($1, $2, $3)`
		what = "count"
	}

	var n int64
	if err := m.pool.QueryRow(ctx, fn, string(m.table), site, days).Scan(&n); err != nil {
		return 0, fmt.Errorf("retention: %s %s rows for %s: %w", what, m.table, site, err)
	}
	return n, nil
}
