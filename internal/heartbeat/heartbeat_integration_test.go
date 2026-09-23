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
	if !got.StartedAt.Equal(started) {
		t.Errorf("started_at = %v, want %v", got.StartedAt, started)
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
			New(Options{
				Pool: pool, Version: "v-behind-" + column,
				Profile: "tam", IPTokenKey: TokenKeyPresent,
			}).beat(ctx)

			var version string
			if err := admin.QueryRow(ctx,
				`SELECT version FROM service_heartbeat WHERE service = $1`,
				testdb.Collector).Scan(&version); err != nil {
				t.Fatalf("no heartbeat row was written against the older schema: %v\n"+
					"The panel would show this service as down for as long as the upgrade "+
					"takes, which is when somebody is watching it", err)
			}
			if version != "v-behind-"+column {
				t.Errorf("version = %q, want %q", version, "v-behind-"+column)
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
