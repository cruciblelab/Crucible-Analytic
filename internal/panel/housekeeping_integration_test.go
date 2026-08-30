//go:build integration

// The sweeps against a real database, as the role that runs them.
//
// A sweep is one of the few pieces of code whose bug is invisible from
// its own return value: deleting nothing and deleting everything both
// come back as a row count and an untroubled nil. So each test here
// seeds two rows on either side of the boundary and asserts which one
// survived, rather than asserting that the call succeeded.
package panel

import (
	"context"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// housekeepingStore is a Store on the panel's own role.
//
// panel_user rather than a superuser, because RLS is what the log sweep
// actually depends on and a superuser is exempt from it. A fixture more
// privileged than production does not test production.
func housekeepingStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.Pool(t, testdb.Panel)
	admin := testdb.Admin(t)

	// One statement per table, because they do not share a column to
	// match on. The first version of this ran one WHERE against both and
	// so cleaned neither - it logged two errors a test and the suite
	// stayed green, because every assertion here counts rows the test
	// itself just seeded. It would have started failing the first time a
	// leftover row landed inside a boundary.
	clean := func() {
		statements := map[string]string{
			"panel_logs":       `DELETE FROM panel_logs WHERE message LIKE 'süpürme testi%'`,
			"panel_operations": `DELETE FROM panel_operations WHERE target LIKE 'süpürme testi%'`,
		}
		for table, sql := range statements {
			if _, err := admin.Exec(context.Background(), sql); err != nil {
				t.Fatalf("clearing %s: %v — a cleanup that does not run leaves the next test reading this one's rows", table, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)

	return &Store{pool: pool}, admin
}

// TestTheLogSweepTakesTheOldAndLeavesTheNew.
//
// Both boundaries in one test on purpose: a sweep that deleted nothing
// and a sweep that deleted everything are the two ways this fails, and
// they need each other to be visible. A test that only checked the old
// row was gone would pass on `DELETE FROM panel_logs`.
func TestTheLogSweepTakesTheOldAndLeavesTheNew(t *testing.T) {
	store, admin := housekeepingStore(t)
	ctx := context.Background()

	// Seeded through the owner so the rows can carry a chosen `at` and a
	// service the panel is not: the point of the next test is that the
	// sweep crosses roles, and it needs a row it did not write.
	rows := []struct {
		at      time.Time
		service string
		message string
	}{
		{time.Now().Add(-logRetention - time.Hour), testdb.Collector, "süpürme testi: eski"},
		{time.Now().Add(-time.Hour), testdb.Collector, "süpürme testi: yeni"},
	}
	for _, r := range rows {
		if _, err := admin.Exec(ctx, `
			INSERT INTO panel_logs (at, service, level, message)
			VALUES ($1, $2, 'WARN', $3)`, r.at, r.service, r.message); err != nil {
			t.Fatalf("seeding %q: %v", r.message, err)
		}
	}

	removed, err := store.PurgeOldLogs(ctx)
	if err != nil {
		t.Fatalf("the sweep failed: %v", err)
	}
	if removed != 1 {
		t.Errorf("the sweep removed %d rows, want 1", removed)
	}

	left := logMessages(t, admin)
	if hasRow(left, "süpürme testi: eski") {
		t.Error("a row older than the retention is still there; the table is not bounded")
	}
	if !hasRow(left, "süpürme testi: yeni") {
		t.Error("a row inside the retention was deleted; the sweep is not reading the boundary")
	}
}

// TestTheLogSweepCrossesRoles.
//
// The property panel_logs_sweep exists for, asserted from the caller's
// side rather than from SQL. Under the write policy alone the panel
// deletes only what the panel wrote, so a sweep would leave the
// collector's, the beacon's and the API's lines behind - and those are
// most of the table.
func TestTheLogSweepCrossesRoles(t *testing.T) {
	store, admin := housekeepingStore(t)
	ctx := context.Background()

	old := time.Now().Add(-logRetention - time.Hour)
	for _, service := range []string{testdb.Collector, testdb.Beacon, testdb.Reader, testdb.Panel} {
		if _, err := admin.Exec(ctx, `
			INSERT INTO panel_logs (at, service, level, message)
			VALUES ($1, $2, 'WARN', 'süpürme testi: ' || $2)`, old, service); err != nil {
			t.Fatalf("seeding for %s: %v", service, err)
		}
	}

	removed, err := store.PurgeOldLogs(ctx)
	if err != nil {
		t.Fatalf("the sweep failed: %v", err)
	}
	if removed != 4 {
		t.Errorf("the sweep removed %d of 4 services' lines. "+
			"A sweep that trims only its own writer's rows leaves the table growing "+
			"while looking like it works", removed)
	}
}

// TestTheOperationSweepHandlesUnfinishedOperations.
//
// An operation that never finished has a null finished_at, and it is
// exactly the kind this table exists to record.
//
// The column the sweep keys on decides what happens to those rows, and
// the failure is the opposite of what it looks like: `NULL < now() -
// interval` is NULL, never true, so a sweep keyed on finished_at would
// not delete unfinished operations early - it would never delete them at
// all. Every operation interrupted by a restart or a crash would sit in
// the table permanently, which is an unbounded table made out of exactly
// the rows nobody is looking at any more.
//
// So both ends are asserted: a recent unfinished one stays, a stale
// unfinished one goes. An earlier version of this test seeded only the
// first, and keying on finished_at passed it - the assertion was on a
// row that survives either way.
func TestTheOperationSweepHandlesUnfinishedOperations(t *testing.T) {
	store, admin := housekeepingStore(t)
	ctx := context.Background()

	seed := func(id string, started time.Time, finished bool) {
		t.Helper()
		var fin any
		if finished {
			fin = started.Add(time.Second)
		}
		if _, err := admin.Exec(ctx, `
			INSERT INTO panel_operations (id, started_at, finished_at, action, target, outcome, actor_kind)
			VALUES ($1, $2, $3, 'test', 'süpürme testi', 'succeeded', 'test')`,
			id, started, fin); err != nil {
			t.Fatalf("seeding %s: %v", id, err)
		}
	}

	recent := time.Now().Add(-time.Hour)
	stale := time.Now().Add(-operationRetention - time.Hour)
	seed("op-testrecentfinish", recent, true)
	seed("op-testrecentopenop", recent, false) // never finished, still recent
	seed("op-teststalefinishd", stale, true)
	seed("op-teststaleopenopp", stale, false) // never finished, long past

	removed, err := store.PurgeOldOperations(ctx)
	if err != nil {
		t.Fatalf("the sweep failed: %v", err)
	}
	if removed != 2 {
		t.Errorf("the sweep removed %d rows, want 2", removed)
	}

	left := operationIDs(t, admin)
	if !hasRow(left, "op-testrecentopenop") {
		t.Error("a recent unfinished operation was deleted; it is the one somebody is about to ask about")
	}
	if !hasRow(left, "op-testrecentfinish") {
		t.Error("a recent finished operation was deleted; the sweep is not reading the boundary")
	}
	if hasRow(left, "op-teststalefinishd") {
		t.Error("a stale operation survived; the table is not bounded")
	}
	if hasRow(left, "op-teststaleopenopp") {
		t.Error("a stale *unfinished* operation survived. `NULL < now() - interval` is NULL, " +
			"never true, so a sweep keyed on finished_at never removes these at all - " +
			"and an interrupted operation is left in the table forever")
	}
}

// TestOneFailingSweepDoesNotStopTheOthers.
//
// Independent tables, and a permission problem on one is not a reason to
// let the other three grow. Measured by running the whole pass against a
// role that can sweep, and asserting the report accounts for each table
// separately rather than collapsing into a single number.
func TestHousekeepingReportsEachTableSeparately(t *testing.T) {
	store, admin := housekeepingStore(t)
	ctx := context.Background()

	old := time.Now().Add(-logRetention - time.Hour)
	if _, err := admin.Exec(ctx, `
		INSERT INTO panel_logs (at, service, level, message)
		VALUES ($1, $2, 'WARN', 'süpürme testi: rapor')`, old, testdb.Collector); err != nil {
		t.Fatal(err)
	}

	report, err := store.Housekeeping(ctx)
	if err != nil {
		t.Fatalf("the pass reported an error: %v", err)
	}
	if report.Logs != 1 {
		t.Errorf("report.Logs = %d, want 1", report.Logs)
	}
	if report.Total() < report.Logs {
		t.Errorf("Total() = %d is less than its own Logs field (%d)", report.Total(), report.Logs)
	}
}

func logMessages(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	return queryStrings(t, pool, `SELECT message FROM panel_logs WHERE message LIKE 'süpürme testi%'`)
}

func operationIDs(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	return queryStrings(t, pool, `SELECT id FROM panel_operations WHERE target LIKE 'süpürme testi%'`)
}

func queryStrings(t *testing.T, pool *pgxpool.Pool, sql string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func hasRow(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
