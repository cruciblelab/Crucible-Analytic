//go:build integration

// Retention against a real TimescaleDB.
//
// # The connection this suite uses, and why it changed
//
// Every test here used to run on one pool, as `collector`, against a
// development database `collector` had created - so collector owned both
// hypertables and TimescaleDB let it do anything to them. The suite
// passed for years while the feature had never once worked on an
// installed deployment, where the tables belong to the superuser that
// ran release/install.sh and ownership, not privilege, is what
// add_retention_policy checks.
//
// It also had collector managing beacon_events, which no deployment has
// ever allowed and which the wrappers now refuse by name.
//
// So the suite now connects as each table's own service role, the way
// the running services do, against a database installed the way a
// customer's is. Three roles rather than one, because that is how many
// the product has:
//
//	collector        writes and retains traffic_snapshots
//	beacon_writer    writes and retains beacon_events
//	analytics_reader reads both, and is the only one here that can
//	                 count rows - beacon_writer holds INSERT alone
//
// A test that needs a role's privileges has to use that role. Anything
// else measures the fixture.
package retention

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The development cluster's roles. Overridable per role by environment,
// for a cluster whose passwords are not these.
func dsnFor(t *testing.T, role string) string {
	t.Helper()
	if dsn := os.Getenv("CA_DSN_" + role); dsn != "" {
		return dsn
	}
	return fmt.Sprintf("postgres://%s:%s@localhost:5432/analytics", role, role)
}

func poolAs(t *testing.T, role string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsnFor(t, role))
	if err != nil {
		t.Fatalf("pgxpool.New as %s: %v", role, err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping as %s: %v (is the database up, with the schema files and release/sql/grants.sql applied?)", role, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// ownerOf names the role a table's retention belongs to. The same
// mapping ca_check_retention_caller enforces, which is the point: a test
// that used the wrong one would be asking for something no deployment
// permits.
func ownerOf(table Table) string {
	if table == TableTrafficSnapshots {
		return "collector"
	}
	return "beacon_writer"
}

// restorePolicy puts a table's retention back to the ceiling when a test
// finishes, so a short policy left behind cannot quietly drop another
// test's rows.
//
// Set rather than removed, because there is no wrapper that removes one
// and there should not be: nothing in the product ever removes a
// retention policy, and adding a way to do it so that a test can tidy up
// would be production surface that exists for the tests. MaxDays is
// older than anything any test inserts.
func restorePolicy(t *testing.T, pool *pgxpool.Pool, table Table) {
	t.Helper()
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(),
			`SELECT ca_set_retention($1, $2)`, string(table), MaxDays); err != nil {
			t.Logf("cleanup: resetting the policy on %s: %v", table, err)
		}
	})
}

// The plan's own done-criterion: the policy has to be readable from
// TimescaleDB's job catalogue, not merely reported as applied by the
// code that applied it.
func TestApply_RegistersARealPolicy(t *testing.T) {
	ctx := context.Background()

	for _, table := range []Table{TableTrafficSnapshots, TableBeaconEvents} {
		t.Run(string(table), func(t *testing.T) {
			pool := poolAs(t, ownerOf(table))
			restorePolicy(t, pool, table)

			manager, err := NewManager(pool, table)
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}

			before, err := manager.Current(ctx)
			if err != nil {
				t.Fatalf("Current: %v", err)
			}

			report, err := manager.Apply(ctx, Policy{Days: 90})
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if report.PolicyDays != 90 {
				t.Errorf("PolicyDays = %d, want 90", report.PolicyDays)
			}
			if report.PreviousDays != before {
				t.Errorf("PreviousDays = %d, want %d", report.PreviousDays, before)
			}

			// Read back from TimescaleDB itself.
			var count int
			var dropAfter string
			err = pool.QueryRow(ctx, `
				SELECT count(*), coalesce(max(config->>'drop_after'), '')
				FROM timescaledb_information.jobs
				WHERE proc_name = 'policy_retention' AND hypertable_name = $1`,
				string(table)).Scan(&count, &dropAfter)
			if err != nil {
				t.Fatalf("reading the job catalogue: %v", err)
			}
			if count != 1 {
				t.Fatalf("TimescaleDB holds %d retention jobs for %s, want exactly 1", count, table)
			}
			if dropAfter != "90 days" {
				t.Errorf("drop_after = %q, want \"90 days\"", dropAfter)
			}

			// And the manager reads its own work back correctly.
			if got, err := manager.Current(ctx); err != nil || got != 90 {
				t.Errorf("Current = %d (err %v), want 90", got, err)
			}
		})
	}
}

// The job belongs to the role that installed, not to the service that
// asked for it.
//
// This is what makes the wrapper acceptable next to release/sql/
// harden.sql, which revokes job scheduling from everything: a service
// can ask for a retention interval on its own table, and what appears in
// the catalogue is still owned by the superuser. A job owned by a
// service role would be persistence held by a process on the traffic
// path, which is the thing that file exists to prevent.
func TestTheRetentionJobIsNotOwnedByTheServiceThatAskedForIt(t *testing.T) {
	ctx := context.Background()
	pool := poolAs(t, "collector")
	restorePolicy(t, pool, TableTrafficSnapshots)

	manager, err := NewManager(pool, TableTrafficSnapshots)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Apply(ctx, Policy{Days: 120}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var owner string
	if err := pool.QueryRow(ctx, `
		SELECT owner::text FROM timescaledb_information.jobs
		WHERE proc_name = 'policy_retention' AND hypertable_name = 'traffic_snapshots'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	for _, service := range []string{"collector", "beacon_writer", "analytics_reader", "panel_user"} {
		if owner == service {
			t.Fatalf("the retention job is owned by %s; a service role owning a scheduled job is persistence that survives every restart", service)
		}
	}
}

// A service may not manage the other one's table.
//
// The wrappers are granted to both roles - the guard inside them, not
// the grant, is what separates the two - so this is the assertion that
// the separation exists at all.
func TestAServiceCannotRetainTheOtherServicesTable(t *testing.T) {
	ctx := context.Background()

	for _, c := range []struct {
		role  string
		table Table
	}{
		{"collector", TableBeaconEvents},
		{"beacon_writer", TableTrafficSnapshots},
	} {
		t.Run(c.role+"/"+string(c.table), func(t *testing.T) {
			pool := poolAs(t, c.role)
			manager, err := NewManager(pool, c.table)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Apply(ctx, Policy{Days: 90}); err == nil {
				t.Fatalf("%s set a retention policy on %s", c.role, c.table)
			}
		})
	}
}

// Neither writer may empty its own table, which is what the trim wrapper
// exists instead of.
func TestAWriterCannotDeleteItsOwnTable(t *testing.T) {
	ctx := context.Background()

	for _, c := range []struct {
		role  string
		table Table
	}{
		{"collector", TableTrafficSnapshots},
		{"beacon_writer", TableBeaconEvents},
	} {
		t.Run(c.role, func(t *testing.T) {
			pool := poolAs(t, c.role)
			if _, err := pool.Exec(ctx, `DELETE FROM `+string(c.table)); err == nil {
				t.Fatalf("%s emptied %s in one statement", c.role, c.table)
			}
		})
	}
}

// TimescaleDB refuses a second retention policy on one hypertable, so a
// change that only ever added would fail on every edit after the first -
// and the failure would look like a permissions problem rather than what
// it is.
func TestApply_ReplacesRatherThanDuplicates(t *testing.T) {
	ctx := context.Background()
	pool := poolAs(t, "beacon_writer")
	restorePolicy(t, pool, TableBeaconEvents)

	manager, err := NewManager(pool, TableBeaconEvents)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	for _, days := range []int{90, 30, 365} {
		report, err := manager.Apply(ctx, Policy{Days: days})
		if err != nil {
			t.Fatalf("Apply(%d): %v", days, err)
		}
		if !report.PolicyChanged {
			t.Errorf("Apply(%d) reported no change", days)
		}
		if got, _ := manager.Current(ctx); got != days {
			t.Errorf("after Apply(%d), Current = %d", days, got)
		}
	}

	var jobs int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM timescaledb_information.jobs
		WHERE proc_name = 'policy_retention' AND hypertable_name = 'beacon_events'`).Scan(&jobs); err != nil {
		t.Fatalf("counting jobs: %v", err)
	}
	if jobs != 1 {
		t.Errorf("three applies left %d jobs, want 1", jobs)
	}

	// Applying the same figure again is a no-op, so a service that calls
	// this every minute does not churn the job catalogue.
	report, err := manager.Apply(ctx, Policy{Days: 365})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if report.PolicyChanged {
		t.Error("re-applying the same retention reported a change")
	}
}

// The hypertable policy must keep what the longest-retaining site wants.
// A policy at the shortest value would destroy every other site's data
// and look like the feature working.
func TestApply_HypertablePolicyKeepsTheLongestSitesData(t *testing.T) {
	ctx := context.Background()
	pool := poolAs(t, "beacon_writer")
	restorePolicy(t, pool, TableBeaconEvents)

	manager, err := NewManager(pool, TableBeaconEvents)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	report, err := manager.Apply(ctx, Policy{
		Days:    90,
		PerSite: map[string]int{"kisa": 30, "uzun": 365},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if report.PolicyDays != 365 {
		t.Fatalf("the hypertable policy is %d days; the site asking for 365 would lose data", report.PolicyDays)
	}
	if got, _ := manager.Current(ctx); got != 365 {
		t.Errorf("Current = %d, want 365", got)
	}
}

// The one case chunks cannot express: a site that wants less than the
// deployment keeps. Verified against real rows.
func TestApply_TrimsOnlyTheSiteThatAskedForLess(t *testing.T) {
	ctx := context.Background()
	writer := poolAs(t, "beacon_writer")
	reader := poolAs(t, "analytics_reader")
	restorePolicy(t, writer, TableBeaconEvents)

	const shortSite, longSite = "ret-kisa", "ret-uzun"

	// Removed through the wrapper, as the writer, because that is the
	// only way this role can remove anything - which is the property
	// being tested three tests up. A cleanup that needed DELETE would
	// need a privilege the product deliberately withholds.
	clear := func() {
		for _, site := range []string{shortSite, longSite} {
			if _, err := writer.Exec(context.Background(),
				`SELECT ca_trim_site_rows('beacon_events', $1, 1)`, site); err != nil {
				t.Logf("cleanup: trimming %s: %v", site, err)
			}
		}
	}
	t.Cleanup(clear)

	// Two rows per site: one inside 30 days, one well outside it.
	now := time.Now()
	for _, row := range []struct {
		site string
		age  time.Duration
	}{
		{shortSite, 2 * 24 * time.Hour},
		{shortSite, 60 * 24 * time.Hour},
		{longSite, 2 * 24 * time.Hour},
		{longSite, 60 * 24 * time.Hour},
	} {
		if _, err := writer.Exec(ctx, `
			INSERT INTO beacon_events (time, site_id, visitor_id, event_type, path, ip)
			VALUES ($1, $2, 'testvisitor', 'pageview', '/', $3)`,
			now.Add(-row.age), row.site, netip.MustParseAddr("203.0.113.0")); err != nil {
			t.Fatalf("inserting a row: %v", err)
		}
	}

	manager, err := NewManager(writer, TableBeaconEvents)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	policy := Policy{Days: 90, PerSite: map[string]int{shortSite: 30}}

	// The dry run reports before anything is destroyed. That is the
	// whole point of it: "90 days to 30" is a number somebody types
	// without picturing what it removes.
	dry, err := manager.DryRun(ctx, policy)
	if err != nil {
		t.Fatalf("DryRun: %v", err)
	}
	if dry.SiteRows[shortSite] != 1 {
		t.Errorf("DryRun says %d rows for %s, want 1", dry.SiteRows[shortSite], shortSite)
	}
	if _, ok := dry.SiteRows[longSite]; ok {
		t.Errorf("DryRun proposes touching %s, which asked for no change", longSite)
	}
	if countRows(t, reader, shortSite) != 2 {
		t.Fatal("DryRun deleted rows")
	}

	if _, err := manager.Apply(ctx, policy); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := countRows(t, reader, shortSite); got != 1 {
		t.Errorf("%s has %d rows, want 1 - only the row past its own 30 days should go", shortSite, got)
	}
	if got := countRows(t, reader, longSite); got != 2 {
		t.Errorf("%s has %d rows, want 2 - it never asked for a shorter retention", longSite, got)
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, site string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM beacon_events WHERE site_id = $1`, site).Scan(&n); err != nil {
		t.Fatalf("counting rows for %s: %v", site, err)
	}
	return n
}

// A table name this package does not know must never reach SQL. Table is
// a string type, so a caller can write Table(whatever) and the compiler
// will not object - this is the check that makes that harmless.
//
// The database refuses the same names a second time, from inside the
// wrappers, which is why they take a closed set too: a Manager is not
// the only thing that can call ca_set_retention.
func TestNewManager_RefusesAnUnknownTable(t *testing.T) {
	pool := poolAs(t, "collector")
	for _, name := range []string{
		"panel_users",
		"beacon_events; DROP TABLE panel_users",
		"",
	} {
		if _, err := NewManager(pool, Table(name)); err == nil {
			t.Errorf("NewManager accepted %q", name)
		}
		if _, err := pool.Exec(context.Background(),
			`SELECT ca_set_retention($1, 90)`, name); err == nil {
			t.Errorf("the database accepted %q as a retention table", name)
		}
	}
}

// And the ceiling is held on both sides. Go refuses it before the call;
// the database refuses it again for anything that did not come through
// Go at all.
func TestTheDatabaseHoldsTheCeilingToo(t *testing.T) {
	pool := poolAs(t, "collector")
	for _, days := range []int{0, -1, MaxDays + 1} {
		if _, err := pool.Exec(context.Background(),
			`SELECT ca_set_retention('traffic_snapshots', $1)`, days); err == nil {
			t.Errorf("the database accepted a retention of %d days", days)
		}
	}
}

func TestPolicy_RefusesValuesOutsideItsBounds(t *testing.T) {
	for _, policy := range []Policy{
		{Days: 0},
		{Days: -1},
		{Days: MaxDays + 1},
		{Days: 90, PerSite: map[string]int{"site": 0}},
		{Days: 90, PerSite: map[string]int{"site": MaxDays + 1}},
	} {
		if err := policy.Validate(); err == nil {
			t.Errorf("Validate accepted %+v", policy)
		}
	}
	if err := (Policy{Days: 90, PerSite: map[string]int{"site": 30}}).Validate(); err != nil {
		t.Errorf("Validate refused a sound policy: %v", err)
	}
}
