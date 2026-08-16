//go:build integration

package retention

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURL = "postgres://collector:collector@localhost:5432/analytics"

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping: %v (is docker compose up, with the schema files applied?)", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// clearPolicies puts the database back the way the test found it, so a
// retention policy left behind by one test cannot silently delete
// another test's rows.
func clearPolicies(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, table := range []Table{TableTrafficSnapshots, TableBeaconEvents} {
			if _, err := pool.Exec(ctx, `SELECT remove_retention_policy($1, if_exists => true)`, string(table)); err != nil {
				t.Logf("cleanup: removing policy on %s: %v", table, err)
			}
		}
	})
}

// The plan's own done-criterion: the policy has to be readable from
// TimescaleDB's job catalogue, not merely reported as applied by the
// code that applied it.
func TestApply_RegistersARealPolicy(t *testing.T) {
	pool := testPool(t)
	clearPolicies(t, pool)
	ctx := context.Background()

	for _, table := range []Table{TableTrafficSnapshots, TableBeaconEvents} {
		t.Run(string(table), func(t *testing.T) {
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

// TimescaleDB refuses a second retention policy on one hypertable, so a
// change that only ever added would fail on every edit after the first -
// and the failure would look like a permissions problem rather than what
// it is.
func TestApply_ReplacesRatherThanDuplicates(t *testing.T) {
	pool := testPool(t)
	clearPolicies(t, pool)
	ctx := context.Background()

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
	pool := testPool(t)
	clearPolicies(t, pool)
	ctx := context.Background()

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
	pool := testPool(t)
	clearPolicies(t, pool)
	ctx := context.Background()

	const shortSite, longSite = "ret-kisa", "ret-uzun"
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM beacon_events WHERE site_id = ANY($1)`, []string{shortSite, longSite})
	})
	_, _ = pool.Exec(ctx, `DELETE FROM beacon_events WHERE site_id = ANY($1)`, []string{shortSite, longSite})

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
		if _, err := pool.Exec(ctx, `
			INSERT INTO beacon_events (time, site_id, visitor_id, event_type, path, ip)
			VALUES ($1, $2, 'testvisitor', 'pageview', '/', $3)`,
			now.Add(-row.age), row.site, netip.MustParseAddr("203.0.113.0")); err != nil {
			t.Fatalf("inserting a row: %v", err)
		}
	}

	manager, err := NewManager(pool, TableBeaconEvents)
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
	if countRows(t, pool, shortSite) != 2 {
		t.Fatal("DryRun deleted rows")
	}

	if _, err := manager.Apply(ctx, policy); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := countRows(t, pool, shortSite); got != 1 {
		t.Errorf("%s has %d rows, want 1 - only the row past its own 30 days should go", shortSite, got)
	}
	if got := countRows(t, pool, longSite); got != 2 {
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
func TestNewManager_RefusesAnUnknownTable(t *testing.T) {
	pool := testPool(t)
	for _, name := range []string{
		"panel_users",
		"beacon_events; DROP TABLE panel_users",
		"",
	} {
		if _, err := NewManager(pool, Table(name)); err == nil {
			t.Errorf("NewManager accepted %q", name)
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
