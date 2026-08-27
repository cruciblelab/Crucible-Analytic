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
