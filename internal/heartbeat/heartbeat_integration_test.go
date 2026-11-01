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
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/privacy"
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
	// The collector's row is the one internal/relupdate waits for and
	// internal/panel/web draws, so deleting and rewriting it is done
	// under their lock - before the first delete, which is a write too.
	testdb.Lock(t, admin, testdb.HeartbeatLock)

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
		Log: fixedLog{lost: 3, text: "copy rows: connection reset", at: started.Add(time.Hour)},
	})
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
	// Within a second rather than equal. The row's start is the
	// database's now() minus how long the reporter has been running
	// (see Reporter.Started), so it is the given start moved by however
	// long the statement took to reach the server - on one machine,
	// where the two clocks agree, that is the whole difference. The
	// skew test below is what says the database's clock is the one used.
	if diff := got.StartedAt.Sub(started); diff < -time.Second || diff > time.Second {
		t.Errorf("started_at = %v, want within a second of %v (off by %v)",
			got.StartedAt, started, diff)
	}
	if got.Counters[CounterWritten] != 4210 || got.Counters[CounterDropped] != 7 {
		t.Errorf("counters = %v", got.Counters)
	}
	// From the log copy, not from the service's own counters: the one
	// number no service can forget to report.
	if got.Counters[CounterLogLost] != 3 {
		t.Errorf("counters = %v, want %s = 3 from the log copy", got.Counters, CounterLogLost)
	}
	if got.LastError != "copy rows: connection reset" {
		t.Errorf("last_error = %q", got.LastError)
	}
	if !got.LastErrorAt.Equal(started.Add(time.Hour)) {
		t.Errorf("last_error_at = %v, want the time the log copy gave, %v",
			got.LastErrorAt, started.Add(time.Hour))
	}
	// Uptime comes out of the two timestamps rather than being stored,
	// so a clock that moved between them cannot make it a stored lie.
	if up := got.Uptime(); up < 89*time.Minute {
		t.Errorf("uptime = %v, want about 90 minutes", up)
	}
}

// TestTheStartIsInTheDatabasesClock.
//
// B3e's restart rule compares a request - the database's now() when the
// panel wrote it - against the service's start. A start taken from the
// service machine's clock makes that a comparison between two clocks,
// and the failure is quiet in both directions: a machine ahead makes
// every request look already honoured, so the service never restarts; a
// machine behind makes the new process look older than the request, so
// it restarts again.
//
// So the machine's clock here is an hour ahead and the process has been
// running five minutes by it. The row must say "five minutes before the
// database's now", which no value derived from the machine's wall clock
// can produce: that one is fifty-five minutes in the future.
func TestTheStartIsInTheDatabasesClock(t *testing.T) {
	pool := testPool(t)
	admin := testdb.Admin(t)
	ctx := context.Background()

	// An extra amount the test can move the reporter's clock by, which
	// the database does not see. Read and written on this goroutine only:
	// beat is called directly, never from Run.
	var shift time.Duration
	now := func() time.Time { return time.Now().Add(time.Hour + shift) }
	r := New(Options{
		Pool: pool, Version: "v-skew",
		Now:     now,
		Started: now().Add(-5 * time.Minute),
	})
	r.beat(ctx)

	var dbNow, first time.Time
	if err := admin.QueryRow(ctx,
		`SELECT now(), started_at FROM service_heartbeat WHERE service = $1 AND version = 'v-skew'`,
		testdb.Collector).Scan(&dbNow, &first); err != nil {
		t.Fatalf("the row was not written: %v", err)
	}
	want := dbNow.Add(-5 * time.Minute)
	if diff := first.Sub(want); diff < -2*time.Second || diff > 2*time.Second {
		t.Fatalf("started_at = %v, want about %v - five minutes before the database's now - "+
			"(off by %v).\nAn hour off means the machine's wall clock was written instead of "+
			"the database's", first, want, diff)
	}

	// What the watcher reads is what the page reads: the same instant,
	// not a second computation of it.
	got, known := r.Started()
	if !known {
		t.Fatal("Started() says unknown after a write that succeeded")
	}
	if !got.Equal(first) {
		t.Errorf("Started() = %v and the row says %v; the watcher and the health page "+
			"would be comparing a request against two different starts", got, first)
	}

	// And it is fixed. The reporter's clock moves on by three seconds
	// more than the database's between the beats - a start recomputed
	// from it would come out three seconds earlier, and one recomputed
	// against the second statement's now() would differ by that
	// statement's latency. Neither is allowed: equal, not close.
	shift = 3 * time.Second
	r.beat(ctx)
	var second time.Time
	if err := admin.QueryRow(ctx,
		`SELECT started_at FROM service_heartbeat WHERE service = $1 AND version = 'v-skew'`,
		testdb.Collector).Scan(&second); err != nil {
		t.Fatal(err)
	}
	if !second.Equal(first) {
		t.Errorf("the second beat wrote started_at = %v, the first %v (moved by %v); "+
			"the start must be measured once and then kept", second, first, second.Sub(first))
	}
	if got, _ := r.Started(); !got.Equal(first) {
		t.Errorf("Started() moved to %v after the second beat; the first write's %v must stay",
			got, first)
	}
}

// TestTheWaitForAConnectionIsNotPartOfTheStart.
//
// The start is the statement's now() minus an elapsed time the reporter
// measured, so anything between the measurement and the statement moves
// the start later than it was. The one wait that can be long is for a
// free connection - the monitoring pool has two, shared with the log
// copy - and write acquires before it measures for that reason.
//
// Measured on a pool of one connection that this test holds for a second
// and a half: the start must still be the one given, to well under that.
// The database and this test share a machine, so their clocks agree and
// any difference is the reporter's.
func TestTheWaitForAConnectionIsNotPartOfTheStart(t *testing.T) {
	testPool(t) // the lock and the cleanup; the reporter gets its own pool
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(testdb.DSN(testdb.Collector))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 1
	one, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(one.Close)

	started := time.Now().Add(-5 * time.Minute)
	r := New(Options{Pool: one, Version: "v-wait", Started: started})
	// The two questions a first beat asks before it writes - who am I,
	// which columns exist - are asked here, while the connection is free.
	// Both come before anything is measured, so a wait inside them is not
	// the wait this test is about, and the first version of this test,
	// which left the identity to the beat, measured nothing: the second
	// and a half went into SELECT current_user, and a reporter that
	// measured before acquiring passed.
	if !r.resolveService(ctx) {
		t.Fatal("could not resolve the role")
	}
	r.columnsOnce.Do(func() { r.detectColumns(ctx) })

	held, err := one.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const wait = 1500 * time.Millisecond
	go func() {
		time.Sleep(wait)
		held.Release()
	}()
	r.beat(ctx)

	got, known := r.Started()
	if !known {
		t.Fatal("the write did not succeed; nothing was measured")
	}
	if diff := got.Sub(started); diff < -500*time.Millisecond || diff > 500*time.Millisecond {
		t.Errorf("started_at = %v, want %v (off by %v); a start %v late is the wait for "+
			"the connection, counted as time the process had not been running",
			got, started, diff, wait)
	}
}

// TestTheStartIsUnknownUntilAWriteSucceeds.
//
// The watcher does nothing while the start is unknown, and that is the
// whole of its safety: with no start it cannot tell an old request from
// a new one. So "known" must mean a write succeeded - not that one was
// attempted, and not that the reporter was built.
func TestTheStartIsUnknownUntilAWriteSucceeds(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	refused := New(Options{
		Pool: pool, Version: "v-unknown",
		// Refused by row-level security on every write, as in
		// TestAMissingTableDoesNotStopTheService.
		Service: "beacon_writer",
	})
	if _, known := refused.Started(); known {
		t.Fatal("Started() is known before any write")
	}
	refused.beat(ctx)
	if at, known := refused.Started(); known {
		t.Fatalf("Started() = %v after a write the database refused", at)
	}

	accepted := New(Options{Pool: pool, Version: "v-unknown"})
	if _, known := accepted.Started(); known {
		t.Fatal("Started() is known before any write")
	}
	accepted.beat(ctx)
	got, known := accepted.Started()
	if !known {
		t.Fatal("Started() is still unknown after a write that succeeded")
	}
	var row time.Time
	if err := testdb.Admin(t).QueryRow(ctx,
		`SELECT started_at FROM service_heartbeat WHERE service = $1 AND version = 'v-unknown'`,
		testdb.Collector).Scan(&row); err != nil {
		t.Fatalf("the accepted write left no row: %v", err)
	}
	if !got.Equal(row) {
		t.Errorf("Started() = %v, the row says %v", got, row)
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

// TestTheIPTokenKeyStateReachesTheRowAndComesBack.
//
// The same plain case for the column 5b added, and it carries more than
// a label: the panel opens full IP mode on this value, so a state that
// did not survive the round trip would either refuse a deployment that
// is ready or - the direction that matters - offer full mode to one that
// is not.
//
// Both answers are asserted from one run, because the interesting
// failure is a writer that always reports the same thing. A test for
// "present" alone passes against a column hard-wired to it.
func TestTheIPTokenKeyStateReachesTheRowAndComesBack(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	// The key length is the product's own threshold rather than a number
	// typed here, so this fixture cannot drift from what TokenIP accepts.
	usable := bytes.Repeat([]byte{0x5}, privacy.MinHashKeyLen)

	for _, tc := range []struct {
		name string
		key  []byte
		want TokenKeyState
	}{
		{"a service that holds a usable key", usable, TokenKeyPresent},
		{"a service with none", nil, TokenKeyAbsent},
		{"a service whose key is too short to use", usable[:privacy.MinHashKeyLen-1], TokenKeyAbsent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			New(Options{
				Pool: pool, Version: "v-tokenkey",
				IPTokenKey: TokenKeyStateOf(tc.key),
			}).beat(ctx)

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
			if found.IPTokenKey != tc.want {
				t.Errorf("IPTokenKey = %q, want %q", found.IPTokenKey, tc.want)
			}
		})
	}
}

// TestAServiceThatWritesNoAddressesHasNoOpinion.
//
// The read API writes no addresses and sets nothing, and what it must
// leave behind is unknown rather than absent. The difference is a
// sentence on the setup wizard's list: absent sends the operator to edit
// that service's config file, and there is no key to put in it.
//
// The panel refuses full mode on either, so nothing here changes a
// decision - which is exactly why it needs a test. A wrong value costs
// nobody a feature and costs one person an afternoon.
func TestAServiceThatWritesNoAddressesHasNoOpinion(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	New(Options{Pool: pool, Version: "v-noaddresses"}).beat(ctx)

	var got string
	if err := testdb.Admin(t).QueryRow(ctx,
		`SELECT ip_token_key_state FROM service_heartbeat WHERE service = $1`,
		testdb.Collector).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if TokenKeyState(got) != TokenKeyUnknown {
		t.Errorf("ip_token_key_state = %q, want empty for a service that writes no addresses", got)
	}
}

// emptyWithoutTheColumn says what each optional column reads back as
// when the database does not have it.
//
// A map rather than a derivation, because no reflection answers "which
// Beat field does this column fill" - and a list of names with no rule
// is the shape this project avoids. So the rule is on the other side: the
// test below fails if optionalColumns grows an entry this map does not
// have. Adding a column therefore forces somebody to say what it reads
// back as, which is the question the accommodation is about.
var emptyWithoutTheColumn = map[string]func(Beat) string{
	"profile":            func(b Beat) string { return b.Profile },
	"ip_token_key_state": func(b Beat) string { return string(b.IPTokenKey) },
}

// TestTheHeartbeatKeepsWritingWhileTheSchemaIsBehind.
//
// # The window this is about
//
// The profile column arrived with schema 8 and ip_token_key_state with
// 24, and this project's upgrade order is schema first, binaries second.
// Somebody who does it the other way round - or who is simply between
// the two steps - is running a binary that knows about a column the
// database does not have.
//
// Every other writer in this repository refuses to start in that state,
// correctly: a row written with a column missing loses data. The
// heartbeat is the exception, and the reason is what it is for. It is
// how the panel knows whether a service is alive, so a heartbeat that
// stopped writing during an upgrade would report every service as down
// at the exact moment an operator is watching to see whether the upgrade
// worked.
//
// So the label gives way and the row survives.
//
// # Why it is a loop over the list, and why it renames rather than drops
//
// The accommodation is now a loop in the writer and another in the
// reader, over optionalColumns - so a test naming one column would leave
// the second untested while looking complete. This one takes the list
// itself, which is also what makes the missing-entry check above worth
// having.
//
// The column is renamed out of the way and renamed back, where the first
// version dropped it and added it again from a typed-out definition.
// Renaming is exact: the restore cannot reconstruct a slightly different
// column, and nothing typed here can drift from the schema file. It also
// keeps the rows, which a drop does not - and this runs against the
// shared development database.
func TestTheHeartbeatKeepsWritingWhileTheSchemaIsBehind(t *testing.T) {
	pool := testPool(t)
	admin := testdb.Admin(t)
	ctx := context.Background()

	for _, column := range optionalColumns {
		readsBack, ok := emptyWithoutTheColumn[column]
		if !ok {
			t.Errorf("%s is optional and nothing here says what it reads back as when the "+
				"database has no such column; add it to emptyWithoutTheColumn", column)
			continue
		}

		t.Run(column, func(t *testing.T) {
			hidden := column + "__hidden_by_test"
			if _, err := admin.Exec(ctx,
				`ALTER TABLE service_heartbeat RENAME COLUMN `+column+` TO `+hidden); err != nil {
				t.Fatalf("hiding %s, the column this case is about: %v", column, err)
			}
			t.Cleanup(func() {
				if _, err := admin.Exec(context.Background(),
					`ALTER TABLE service_heartbeat RENAME COLUMN `+hidden+` TO `+column); err != nil {
					t.Fatalf("restoring %s: %v - this database is now on a different shape "+
						"than the schema files, and every later run in it will be measuring "+
						"something other than what it thinks", column, err)
				}
			})

			// A fresh reporter each time: the column check is once per
			// process, so one that had already looked would carry the
			// answer from before the rename.
			//
			// The machine's clock is an hour ahead here too, as in
			// TestTheStartIsInTheDatabasesClock: the statement this
			// path builds is a different one, and the start it writes
			// must still be the database's.
			now := func() time.Time { return time.Now().Add(time.Hour) }
			r := New(Options{
				Pool: pool, Version: "v-behind-" + column,
				Profile: "tam", IPTokenKey: TokenKeyPresent,
				Now: now, Started: now().Add(-5 * time.Minute),
			})
			r.beat(ctx)

			var (
				version       string
				started, want time.Time
			)
			if err := admin.QueryRow(ctx,
				`SELECT version, started_at, now() - interval '5 minutes' FROM service_heartbeat WHERE service = $1`,
				testdb.Collector).Scan(&version, &started, &want); err != nil {
				t.Fatalf("no heartbeat row was written against the older schema: %v\n"+
					"The panel would show this service as down for as long as the upgrade "+
					"takes, which is when somebody is watching it", err)
			}
			if version != "v-behind-"+column {
				t.Errorf("version = %q, want %q", version, "v-behind-"+column)
			}
			if diff := started.Sub(want); diff < -2*time.Second || diff > 2*time.Second {
				t.Errorf("started_at = %v against the older schema, want about %v (off by %v)",
					started, want, diff)
			}
			if got, known := r.Started(); !known || !got.Equal(started) {
				t.Errorf("Started() = %v, %v against the older schema; the row says %v",
					got, known, started)
			}

			// And the reader survives it too, for the same reason: the
			// panel binary may be the new one while the database is still
			// the old one.
			beats, err := Read(ctx, admin)
			if err != nil {
				t.Fatalf("Read failed against the older schema: %v", err)
			}
			for _, b := range beats {
				if b.Service != testdb.Collector {
					continue
				}
				if got := readsBack(b); got != "" {
					t.Errorf("%s came back as %q against a database with no such column",
						column, got)
				}
			}
		})
	}
}
