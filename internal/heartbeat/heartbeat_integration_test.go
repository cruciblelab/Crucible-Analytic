//go:build integration

// The heartbeat against a real database.
//
// A fake pool would test that this package can format an INSERT. What
// only a real database can answer is whether the row is written by the
// role the policy expects, whether the counters survive the round trip
// as JSONB, and whether a service that cannot reach the table carries on
// regardless - which is the property the whole package is built around.

package heartbeat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// testPool connects as the collector - one of the four services that
// writes a heartbeat - and clears its row through the schema's owner.
//
// The clearing used to run on the same pool and discard its error. No
// service holds DELETE on service_heartbeat, deliberately: a row that
// disappears reads as "this service was never installed" rather than
// "this service is gone", which is the wrong sentence at the moment it
// matters. So the cleanup had silently stopped doing anything the
// moment the database was installed properly, and the suite went on
// looking tidy because the row is upserted anyway.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testdb.Pool(t, testdb.Collector)
	admin := testdb.Admin(t)

	clean := func() {
		if _, err := admin.Exec(context.Background(),
			`DELETE FROM service_heartbeat WHERE service = $1`, testdb.Collector); err != nil {
			t.Logf("clearing the collector's heartbeat row: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)
	return pool
}

func TestTheRowSaysWhatTheServiceKnows(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	started := time.Now().Add(-90 * time.Minute).Truncate(time.Second)

	r := New(Options{
		Pool:    pool,
		Version: "v1.2.3-test",
		Started: started,
		Counters: func() map[string]int64 {
			return map[string]int64{CounterWritten: 4210, CounterDropped: 7}
		},
	})
	r.Note(errors.New("copy rows: connection reset"))
	r.beat(ctx)

	beats, err := Read(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	var got Beat
	for _, b := range beats {
		if b.Version == "v1.2.3-test" {
			got = b
		}
	}
	if got.Service == "" {
		t.Fatalf("the row was not written; Read returned %d rows", len(beats))
	}

	// The service name comes from the connection, never from a setting.
	// Asserted because the alternative - a configured name - is the one
	// way to end up writing nothing while looking configured.
	if got.Service != "collector" {
		t.Errorf("service = %q, want the connection's role", got.Service)
	}
	if !got.StartedAt.Equal(started) {
		t.Errorf("started_at = %v, want %v", got.StartedAt, started)
	}
	if got.Counters[CounterWritten] != 4210 || got.Counters[CounterDropped] != 7 {
		t.Errorf("counters = %v", got.Counters)
	}
	if got.LastError != "copy rows: connection reset" {
		t.Errorf("last_error = %q", got.LastError)
	}
	if got.LastErrorAt.IsZero() {
		t.Error("last_error_at is unset with an error recorded")
	}
	// Uptime comes out of the two timestamps rather than being stored,
	// so a clock that moved between them cannot make it a stored lie.
	if up := got.Uptime(); up < 89*time.Minute {
		t.Errorf("uptime = %v, want about 90 minutes", up)
	}
}

// A service that has never failed must not carry a last-error timestamp,
// because a page showing one would send somebody looking for a fault
// that never happened.
func TestNoErrorMeansNoErrorTime(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	r := New(Options{Pool: pool, Version: "v-clean"})
	r.beat(ctx)

	beats, err := Read(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range beats {
		if b.Version != "v-clean" {
			continue
		}
		if b.LastError != "" || !b.LastErrorAt.IsZero() {
			t.Errorf("a service that never failed reports %q at %v", b.LastError, b.LastErrorAt)
		}
		return
	}
	t.Fatal("the row was not written")
}

// The identity is asked of the connection. This is the assertion the
// design rests on: no configuration can put the wrong name in the row,
// because no configuration puts a name in the row.
func TestTheServiceNameComesFromTheConnection(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	r := New(Options{Pool: pool, Version: "v-resolve"})
	if r.service != "" {
		t.Fatal("the reporter was given a service name it should have looked up")
	}
	if !r.resolveService(ctx) {
		t.Fatal("resolveService failed against a working connection")
	}
	if r.service != "collector" {
		t.Errorf("resolved %q, want the role this test connects as", r.service)
	}
}

// A service whose heartbeat cannot be written must keep running. This is
// the package's central promise and the one that would be easiest to
// break with a well-meaning error return.
func TestAMissingTableDoesNotStopTheService(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	r := New(Options{
		Pool: pool, Version: "v-broken", Interval: 20 * time.Millisecond,
		// A name no policy will accept for this connection: the write is
		// refused by row-level security every time, which is the closest
		// reproduction of a broken heartbeat a working database allows.
		Service: "beacon_writer",
	})

	done := make(chan struct{})
	runCtx, cancel := context.WithCancel(ctx)
	go func() {
		r.Run(runCtx)
		close(done)
	}()

	// Long enough for several failed beats.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	// And nothing was written under the wrong name, which is the other
	// half: a failing heartbeat must fail, not succeed quietly at
	// somebody else's expense.
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM service_heartbeat WHERE version = 'v-broken'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d rows written despite every write being refused", n)
	}
}

// Run writes immediately rather than waiting out its first interval.
// Without it the panel calls every service dead for the first minute
// after a restart - which is exactly when somebody is watching.
func TestRunBeatsImmediately(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := New(Options{Pool: pool, Version: "v-immediate", Interval: time.Hour})
	go r.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM service_heartbeat WHERE version = 'v-immediate'`).Scan(&n); err == nil && n == 1 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("no row appeared within three seconds, with an interval of one hour")
}

// TestTheProfileReachesTheRowAndComesBack.
//
// The plain case, against the real column: what the collector reports is
// what the panel reads. Asserted through Read rather than by selecting
// the column directly, because Read is what the panel actually calls and
// a column written but never selected is a column nobody sees.
func TestTheProfileReachesTheRowAndComesBack(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	r := New(Options{Pool: pool, Version: "v-profile", Profile: "dengeli"})
	r.beat(ctx)

	beats, err := Read(ctx, testdb.Admin(t))
	if err != nil {
		t.Fatal(err)
	}
	var found *Beat
	for i := range beats {
		if beats[i].Service == testdb.Collector {
			found = &beats[i]
		}
	}
	if found == nil {
		t.Fatalf("no row for %s after a beat", testdb.Collector)
	}
	if found.Profile != "dengeli" {
		t.Errorf("Profile = %q, want %q", found.Profile, "dengeli")
	}
}

// TestAServiceWithNoProfileReportsNone.
//
// Three of the four services have no resource profile, and the empty
// string is what they must write. The alternative a future edit might
// reach for - a placeholder like "none" or "-" - would reach the panel
// as a profile named "none" and cost somebody a minute working out which
// one that is.
func TestAServiceWithNoProfileReportsNone(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	New(Options{Pool: pool, Version: "v-noprofile"}).beat(ctx)

	var got string
	if err := testdb.Admin(t).QueryRow(ctx,
		`SELECT profile FROM service_heartbeat WHERE service = $1`,
		testdb.Collector).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("profile = %q, want empty for a service that has none", got)
	}
}

// TestTheHeartbeatKeepsWritingWhileTheSchemaIsBehind.
//
// # The window this is about
//
// The profile column arrived with schema 8, and this project's upgrade
// order is schema first, binaries second. Somebody who does it the other
// way round - or who is simply between the two steps - is running a
// binary that knows about a column the database does not have.
//
// Every other writer in this repository refuses to start in that state,
// correctly: a row written with a column missing loses data. The
// heartbeat is the exception, and the reason is what it is for. It is
// how the panel knows whether a service is alive, so a heartbeat that
// stopped writing during an upgrade would report every service as down
// at the exact moment an operator is watching to see whether the upgrade
// worked.
//
// So the label gives way and the row survives. This test drops the
// column, writes, and checks that the beat is still there.
func TestTheHeartbeatKeepsWritingWhileTheSchemaIsBehind(t *testing.T) {
	pool := testPool(t)
	admin := testdb.Admin(t)
	ctx := context.Background()

	if _, err := admin.Exec(ctx, `ALTER TABLE service_heartbeat DROP COLUMN profile`); err != nil {
		t.Fatalf("dropping the column this test is about: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(),
			`ALTER TABLE service_heartbeat ADD COLUMN IF NOT EXISTS profile TEXT NOT NULL DEFAULT ''`); err != nil {
			t.Fatalf("restoring the column: %v - this database is now on an older shape "+
				"than the schema files, and every later run in it will be measuring "+
				"something other than what it thinks", err)
		}
	})

	r := New(Options{Pool: pool, Version: "v-behind", Profile: "tam"})
	r.beat(ctx)

	var version string
	if err := admin.QueryRow(ctx,
		`SELECT version FROM service_heartbeat WHERE service = $1`,
		testdb.Collector).Scan(&version); err != nil {
		t.Fatalf("no heartbeat row was written against the older schema: %v\n"+
			"The panel would show this service as down for as long as the upgrade "+
			"takes, which is when somebody is watching it", err)
	}
	if version != "v-behind" {
		t.Errorf("version = %q, want %q", version, "v-behind")
	}

	// And the reader survives it too, for the same reason: the panel
	// binary may be the new one while the database is still the old one.
	beats, err := Read(ctx, admin)
	if err != nil {
		t.Fatalf("Read failed against the older schema: %v", err)
	}
	for _, b := range beats {
		if b.Service == testdb.Collector && b.Profile != "" {
			t.Errorf("Profile = %q against a database with no such column", b.Profile)
		}
	}
}
