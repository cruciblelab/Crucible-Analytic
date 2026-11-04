//go:build integration

// Asking a service to restart, against a real database (B3e).
//
// Every refusal is tested with every other reason for it removed, and by
// the error's identity: "something refused" passes a call refused for
// the wrong reason exactly as well as one refused for the right one.
//
// The heartbeat rows are this suite's own copy of service_heartbeat
// (testdb.HeartbeatCopy), so the starts and beats each state needs are
// the ones written here and not another package's. The request rows are
// the real panel_settings - that is the table every service reads - so
// they are written under testdb.RestartRequestLock.

package panel

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/relupdate"
	"github.com/cruciblelab/crucible-analytic/internal/restartreq"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

const (
	restartNS     = "restartreq"
	restartSchema = "ca_panel_restart"
)

// restartFixture is a store reading its own heartbeat copy, the gate, and
// the owner who presses.
type restartFixture struct {
	t     *testing.T
	store *Store
	gate  *devgate.Gate
	admin *pgxpool.Pool
	owner Access
}

func newRestartFixture(t *testing.T) *restartFixture {
	t.Helper()
	// Before the store, so its cleanup (dropping the schema) runs after
	// the store's pool is closed.
	beats := testdb.NewHeartbeatCopy(t, restartSchema)
	store := newTestStoreAt(t, restartNS, beats.DSN(testDatabaseURL))
	beats.Reaches(store.Pool())

	admin := testdb.Admin(t)
	// After AccountsLock (newTestStoreAt), and before the cleanup is
	// registered, so the rows go while the lock is still held.
	testdb.Lock(t, admin, testdb.RestartRequestLock)
	clear := func() {
		if _, err := admin.Exec(context.Background(),
			`DELETE FROM panel_settings WHERE scope = 'global' AND site_id = '' AND key LIKE $1`,
			restartreq.KeyPrefix+"%"); err != nil {
			t.Errorf("clearing the restart requests: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)

	owner := mustUser(t, store, restartNS, "sahip", false)
	return &restartFixture{
		t: t, store: store, gate: testGate(t, store), admin: admin,
		// The owner of a one-customer deployment (W1b).
		owner: Access{Principal: principalOf(owner), Role: RoleOwner, Member: true, OwnsEverySite: true},
	}
}

// ran records a service's process: started startedAgo before the
// database's now, last beat beatAgo before it.
func (f *restartFixture) ran(service string, startedAgo, beatAgo time.Duration) {
	f.t.Helper()
	if _, err := f.admin.Exec(context.Background(), `
		INSERT INTO `+restartSchema+`.service_heartbeat (service, version, started_at, beat_at, counters)
		VALUES ($1, 'fixture', now() - make_interval(secs => $2), now() - make_interval(secs => $3), '{}'::jsonb)
		ON CONFLICT (service) DO UPDATE SET started_at = EXCLUDED.started_at, beat_at = EXCLUDED.beat_at`,
		service, startedAgo.Seconds(), beatAgo.Seconds()); err != nil {
		f.t.Fatalf("recording %s's heartbeat: %v", service, err)
	}
}

// startedAt sets a service's start to an exact instant.
func (f *restartFixture) startedAt(service string, at time.Time) {
	f.t.Helper()
	tag, err := f.admin.Exec(context.Background(),
		`UPDATE `+restartSchema+`.service_heartbeat SET started_at = $2 WHERE service = $1`, service, at)
	if err != nil || tag.RowsAffected() != 1 {
		f.t.Fatalf("setting %s's start: %v (%d rows)", service, err, tag.RowsAffected())
	}
}

// age moves a service's request d into the past - its value and its
// updated_at, which a real request always has equal - rather than
// sleeping for it.
func (f *restartFixture) age(service string, d time.Duration) {
	f.t.Helper()
	tag, err := f.admin.Exec(context.Background(), `
		UPDATE panel_settings
		   SET updated_at = updated_at - make_interval(secs => $2),
		       value = to_jsonb((value #>> '{}')::timestamptz - make_interval(secs => $2))
		 WHERE scope = 'global' AND site_id = '' AND key = $1`,
		restartreq.Key(service), d.Seconds())
	if err != nil || tag.RowsAffected() != 1 {
		f.t.Fatalf("ageing %s's request: %v (%d rows)", service, err, tag.RowsAffected())
	}
}

// request is the row as the database holds it, and whether there is one.
func (f *restartFixture) request(service string) (value, updated time.Time, ok bool) {
	f.t.Helper()
	err := f.admin.QueryRow(context.Background(), `
		SELECT (value #>> '{}')::timestamptz, updated_at FROM panel_settings
		 WHERE scope = 'global' AND site_id = '' AND key = $1`,
		restartreq.Key(service)).Scan(&value, &updated)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return time.Time{}, time.Time{}, false
		}
		f.t.Fatalf("reading %s's request: %v", service, err)
	}
	return value, updated, true
}

// entries counts this suite's audit entries for one service.
func (f *restartFixture) entries(service string) int {
	f.t.Helper()
	var n int
	if err := f.admin.QueryRow(context.Background(), `
		SELECT count(*) FROM panel_audit_log
		 WHERE action = $1 AND target = $2 AND actor_label = $3`,
		ActionRestartRequested, service, f.owner.Principal.Label).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *restartFixture) auth() devgate.Authorization {
	f.t.Helper()
	return authorizeAction(f.t, f.gate, RestartGateAction)
}

func (f *restartFixture) status() map[string]RestartService {
	f.t.Helper()
	st, err := f.store.RestartStatus(context.Background(), f.owner)
	if err != nil {
		f.t.Fatalf("RestartStatus: %v", err)
	}
	out := map[string]RestartService{}
	for _, s := range st.Services {
		out[s.Service] = s
	}
	return out
}

// TestAskingForARestartWritesTheDatabasesClockAndItsRecord is the
// positive case, and what every refusal below is measured against.
//
// The request is the database's clock: the row's value is exactly its
// updated_at, which is now() of the writing transaction. A value taken
// from the panel's clock would be a different instant - microseconds
// apart on one machine, minutes apart on two - and the service compares
// it with a start the database measured.
func TestAskingForARestartWritesTheDatabasesClockAndItsRecord(t *testing.T) {
	f := newRestartFixture(t)
	f.ran(testdb.Collector, 10*time.Minute, 5*time.Second)

	got, err := f.store.RequestRestart(context.Background(), f.owner, f.auth(), testdb.Collector)
	if err != nil {
		t.Fatalf("an owner with the developer password was refused: %v", err)
	}
	value, updated, ok := f.request(testdb.Collector)
	if !ok {
		t.Fatal("no request row was written")
	}
	if !value.Equal(updated) {
		t.Errorf("the request says %v and was written at %v; the value has to be the "+
			"database's now() at the write, not a clock of the panel's", value, updated)
	}
	if !got.At.Equal(value) {
		t.Errorf("RequestRestart returned %v, the row holds %v", got.At, value)
	}
	if n := f.entries(testdb.Collector); n != 1 {
		t.Errorf("%d audit entries for the request, want 1", n)
	}
	if got.AuditID == 0 {
		t.Error("no audit id came back, so the operation record cannot point at the entry")
	}
}

// TestARestartNeedsTheDeveloperPasswordEveryTime: no authorization, and
// an authorization minted for another action, are both refused - and
// write nothing. The owner is entitled and the collector can hear, so the
// password is the only reason left.
func TestARestartNeedsTheDeveloperPasswordEveryTime(t *testing.T) {
	f := newRestartFixture(t)
	f.ran(testdb.Collector, 10*time.Minute, 5*time.Second)
	ctx := context.Background()

	for name, auth := range map[string]devgate.Authorization{
		"none":            {},
		"another action":  authorizeAction(t, f.gate, ReleaseGateAction),
		"secrets backup":  authorizeAction(t, f.gate, SecretsGateAction),
		"a guarded field": authorize(t, f.gate, KeyPrivacyIPStorage),
	} {
		_, err := f.store.RequestRestart(ctx, f.owner, auth, testdb.Collector)
		if !errors.Is(err, ErrRestartNeedsPassword) {
			t.Errorf("%s: refused with %v, want ErrRestartNeedsPassword", name, err)
		}
	}
	if _, _, ok := f.request(testdb.Collector); ok {
		t.Error("a request without the password wrote a row")
	}
	if n := f.entries(testdb.Collector); n != 0 {
		t.Errorf("%d audit entries for refused requests", n)
	}
}

// TestARestartIsNotSomethingAViewerCanAsk: entitlement before the
// password, so even a valid authorization does not help somebody who may
// not manage settings.
func TestARestartIsNotSomethingAViewerCanAsk(t *testing.T) {
	f := newRestartFixture(t)
	f.ran(testdb.Collector, 10*time.Minute, 5*time.Second)

	viewer := f.owner
	viewer.Role = RoleViewer
	_, err := f.store.RequestRestart(context.Background(), viewer, f.auth(), testdb.Collector)
	if !errors.Is(err, ErrSettingNotWritable) {
		t.Fatalf("a viewer with a valid authorization was refused with %v, want ErrSettingNotWritable", err)
	}
	if _, _, ok := f.request(testdb.Collector); ok {
		t.Error("a viewer's request wrote a row")
	}
}

// TestOnlyTheFourServicesCanBeAsked: the list is closed, and the strings
// tried are the ones a hand-written form would send - another role that
// has a heartbeat row, a role with spaces, the empty string, a key.
func TestOnlyTheFourServicesCanBeAsked(t *testing.T) {
	f := newRestartFixture(t)
	// schema_admin can read panel_settings and is given a heartbeat, so
	// the closed list is the only thing that can refuse it.
	f.ran(testdb.SchemaAdmin, 10*time.Minute, 5*time.Second)

	for _, service := range []string{testdb.SchemaAdmin, "postgres", "", "collector ", "COLLECTOR",
		restartreq.Key(testdb.Collector)} {
		_, err := f.store.RequestRestart(context.Background(), f.owner, f.auth(), service)
		if !errors.Is(err, ErrRestartUnknownService) {
			t.Errorf("%q was refused with %v, want ErrRestartUnknownService", service, err)
		}
		if _, _, ok := f.request(service); ok {
			t.Errorf("%q wrote a request row", service)
		}
	}
	if len(relupdate.HealthServices) != 4 {
		t.Errorf("the closed list has %d services; this test and the page were written for "+
			"four (%v)", len(relupdate.HealthServices), relupdate.HealthServices)
	}
}

// TestAServiceThatCannotReadTheSettingsIsNotAsked.
//
// The read API's role is not granted panel_settings, so a request for it
// is a row nothing will ever read - and a page that then said "waiting"
// would be waiting forever. The premise is asked of the database first:
// if the grant ever changes, this test must say so rather than pass on a
// different refusal.
func TestAServiceThatCannotReadTheSettingsIsNotAsked(t *testing.T) {
	f := newRestartFixture(t)
	ctx := context.Background()
	for role, want := range map[string]bool{testdb.Reader: false, testdb.Collector: true} {
		var may bool
		if err := f.admin.QueryRow(ctx,
			`SELECT has_table_privilege($1, 'panel_settings', 'SELECT')`, role).Scan(&may); err != nil {
			t.Fatal(err)
		}
		if may != want {
			t.Fatalf("has_table_privilege(%s, panel_settings) = %v; this test was written for %v", role, may, want)
		}
	}
	f.ran(testdb.Reader, 10*time.Minute, 5*time.Second)
	f.ran(testdb.Collector, 10*time.Minute, 5*time.Second)

	_, err := f.store.RequestRestart(ctx, f.owner, f.auth(), testdb.Reader)
	if !errors.Is(err, ErrRestartCannotHear) {
		t.Fatalf("the read API was refused with %v, want ErrRestartCannotHear", err)
	}
	if _, _, ok := f.request(testdb.Reader); ok {
		t.Error("a request nothing can read was written")
	}

	// The same call for a role that can hear goes through: the refusal
	// above is about the role, not about the call.
	if _, err := f.store.RequestRestart(ctx, f.owner, f.auth(), testdb.Collector); err != nil {
		t.Errorf("the collector, which can read the settings, was refused: %v", err)
	}
}

// TestAServiceThatNeverReportedIsNotAsked, and is once it has.
func TestAServiceThatNeverReportedIsNotAsked(t *testing.T) {
	f := newRestartFixture(t)
	ctx := context.Background()

	_, err := f.store.RequestRestart(ctx, f.owner, f.auth(), testdb.Beacon)
	if !errors.Is(err, ErrRestartNeverReported) {
		t.Fatalf("a beacon with no heartbeat was refused with %v, want ErrRestartNeverReported", err)
	}
	if _, _, ok := f.request(testdb.Beacon); ok {
		t.Error("a request for a service that never reported was written")
	}

	f.ran(testdb.Beacon, 10*time.Minute, 5*time.Second)
	if _, err := f.store.RequestRestart(ctx, f.owner, f.auth(), testdb.Beacon); err != nil {
		t.Errorf("once the beacon has reported it was still refused: %v", err)
	}
}

// TestOneServiceIsAskedAtMostOnceAMinute.
//
// Both sides of the minute, by ageing the row in the database rather
// than sleeping: 59 seconds is refused, 61 is not. And per service - the
// collector's spacing does not hold the panel's request back.
func TestOneServiceIsAskedAtMostOnceAMinute(t *testing.T) {
	f := newRestartFixture(t)
	f.ran(testdb.Collector, 10*time.Minute, 5*time.Second)
	f.ran(testdb.Panel, 10*time.Minute, 5*time.Second)
	ctx := context.Background()

	first, err := f.store.RequestRestart(ctx, f.owner, f.auth(), testdb.Collector)
	if err != nil {
		t.Fatalf("first request: %v", err)
	}
	if _, err := f.store.RequestRestart(ctx, f.owner, f.auth(), testdb.Collector); !errors.Is(err, ErrRestartTooSoon) {
		t.Fatalf("an immediate second request was answered %v, want ErrRestartTooSoon", err)
	}

	f.age(testdb.Collector, 59*time.Second)
	before, _, _ := f.request(testdb.Collector)
	if _, err := f.store.RequestRestart(ctx, f.owner, f.auth(), testdb.Collector); !errors.Is(err, ErrRestartTooSoon) {
		t.Fatalf("a request 59 s after the last was answered %v, want ErrRestartTooSoon", err)
	}
	if after, _, _ := f.request(testdb.Collector); !after.Equal(before) {
		t.Errorf("a refused request moved the row from %v to %v", before, after)
	}
	if n := f.entries(testdb.Collector); n != 1 {
		t.Errorf("%d audit entries after one request and two refusals, want 1", n)
	}

	// Another service is not held back by this one's spacing.
	if _, err := f.store.RequestRestart(ctx, f.owner, f.auth(), testdb.Panel); err != nil {
		t.Errorf("the panel was refused while only the collector had been asked: %v", err)
	}

	f.age(testdb.Collector, 2*time.Second) // 61 s in all
	second, err := f.store.RequestRestart(ctx, f.owner, f.auth(), testdb.Collector)
	if err != nil {
		t.Fatalf("a request 61 s after the last was refused: %v", err)
	}
	if !second.At.After(first.At) {
		t.Errorf("the second request (%v) is not later than the first (%v)", second.At, first.At)
	}
	if n := f.entries(testdb.Collector); n != 2 {
		t.Errorf("%d audit entries after two accepted requests, want 2", n)
	}
}

// TestARequestWhoseRecordCannotBeWrittenDoesNotHappen.
//
// The audit entry is inside the request's transaction. The database is
// made to refuse it - a trigger matching this suite's actor and the
// restart action, nothing else (see refuseUnlockEntryFor for why a
// trigger) - and the request row must then be absent, or, for a service
// asked before, unchanged.
func TestARequestWhoseRecordCannotBeWrittenDoesNotHappen(t *testing.T) {
	f := newRestartFixture(t)
	f.ran(testdb.Collector, 10*time.Minute, 5*time.Second)
	f.ran(testdb.Panel, 10*time.Minute, 5*time.Second)
	ctx := context.Background()

	// The panel was asked before the trigger, and long enough ago that
	// the spacing would let it through.
	if _, err := f.store.RequestRestart(ctx, f.owner, f.auth(), testdb.Panel); err != nil {
		t.Fatal(err)
	}
	f.age(testdb.Panel, 2*time.Minute)
	earlier, _, _ := f.request(testdb.Panel)

	refuseRestartEntryFor(t, f.owner.Principal.Label)

	for _, service := range []string{testdb.Collector, testdb.Panel} {
		_, err := f.store.RequestRestart(ctx, f.owner, f.auth(), service)
		if err == nil || !strings.Contains(err.Error(), "record audit entry") {
			t.Fatalf("%s: the request failed with %v; this test is about the audit insert, "+
				"the last statement", service, err)
		}
	}
	if _, _, ok := f.request(testdb.Collector); ok {
		t.Error("a request whose audit entry was refused still left a row")
	}
	if now, _, _ := f.request(testdb.Panel); !now.Equal(earlier) {
		t.Errorf("a request whose audit entry was refused moved the earlier one from %v to %v",
			earlier, now)
	}
}

// refuseRestartEntryFor makes the database refuse one actor's restart
// entries.
func refuseRestartEntryFor(t *testing.T, label string) {
	t.Helper()
	admin := testdb.Admin(t)
	ctx := context.Background()
	const fn, trigger = "ca_test_refuse_restart_entry", "ca_test_refuse_restart_entry"
	drop := []string{
		`DROP TRIGGER IF EXISTS ` + trigger + ` ON panel_audit_log`,
		`DROP FUNCTION IF EXISTS ` + fn + `()`,
	}
	for _, sql := range drop {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if _, err := admin.Exec(ctx, `CREATE FUNCTION `+fn+`() RETURNS trigger
		LANGUAGE plpgsql AS $body$
		BEGIN
		  IF NEW.action = '`+ActionRestartRequested+`' AND NEW.actor_label = `+quoteLiteral(label)+` THEN
		    RAISE EXCEPTION 'test: this restart entry is refused on purpose';
		  END IF;
		  RETURN NEW;
		END
		$body$`); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, `CREATE TRIGGER `+trigger+` BEFORE INSERT ON panel_audit_log
		FOR EACH ROW EXECUTE FUNCTION `+fn+`()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, sql := range drop {
			if _, err := admin.Exec(context.Background(), sql); err != nil {
				t.Errorf("%s: %v - a trigger refusing restart entries is left on panel_audit_log", sql, err)
			}
		}
	})
}

// TestTwoPressesAtOnceWriteOneRequest.
//
// Two presses released together, several rounds: the first round often
// does not race at all, because the pool is still opening connections.
// Each round must end with exactly one request accepted, the other
// refused as too soon, and exactly one audit entry more.
func TestTwoPressesAtOnceWriteOneRequest(t *testing.T) {
	f := newRestartFixture(t)
	f.ran(testdb.Collector, 10*time.Minute, 5*time.Second)
	ctx := context.Background()

	const rounds = 6
	for round := 0; round < rounds; round++ {
		if _, err := f.admin.Exec(ctx, `DELETE FROM panel_settings WHERE scope = 'global'
			AND site_id = '' AND key = $1`, restartreq.Key(testdb.Collector)); err != nil {
			t.Fatal(err)
		}
		before := f.entries(testdb.Collector)
		auth := f.auth()

		var (
			wg    sync.WaitGroup
			start = make(chan struct{})
			errs  = make([]error, 2)
		)
		for i := range errs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = f.store.RequestRestart(ctx, f.owner, auth, testdb.Collector)
			}()
		}
		close(start)
		wg.Wait()

		ok, soon := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrRestartTooSoon):
				soon++
			default:
				t.Fatalf("round %d: a press failed with %v", round, err)
			}
		}
		if ok != 1 || soon != 1 {
			t.Fatalf("round %d: %d accepted and %d too soon, want one of each", round, ok, soon)
		}
		if n := f.entries(testdb.Collector) - before; n != 1 {
			t.Fatalf("round %d: %d audit entries for one accepted request", round, n)
		}
	}
}

// TestTheStatusSaysWhatHappenedToTheLastRequest walks every state on real
// rows, with each boundary from both sides.
func TestTheStatusSaysWhatHappenedToTheLastRequest(t *testing.T) {
	f := newRestartFixture(t)
	ctx := context.Background()
	c := testdb.Collector

	want := func(stage string, state RestartState) RestartService {
		t.Helper()
		got := f.status()[c]
		if got.State != state {
			t.Fatalf("%s: the collector's state is %q, want %q (%+v)", stage, got.State, state, got)
		}
		return got
	}

	// No heartbeat at all.
	want("never reported", RestartNeverReported)

	f.ran(c, 10*time.Minute, 5*time.Second)
	if got := want("not asked", RestartNone); !got.Requested.IsZero() {
		t.Errorf("a service never asked shows a request at %v", got.Requested)
	}

	req, err := f.store.RequestRestart(ctx, f.owner, f.auth(), c)
	if err != nil {
		t.Fatal(err)
	}
	got := want("just asked", RestartPending)
	if !got.Requested.Equal(req.At) {
		t.Errorf("the status shows the request at %v, the row holds %v", got.Requested, req.At)
	}

	// Both sides of the minute.
	f.age(c, 59*time.Second)
	want("asked 59 s ago", RestartPending)
	f.age(c, 2*time.Second)
	want("asked 61 s ago, heartbeat fresh", RestartIgnored)

	// Stale outranks the request's age: a young request to a service that
	// stopped reporting is not waiting for anything.
	if _, err := f.admin.Exec(ctx, `UPDATE panel_settings SET updated_at = now(), value = to_jsonb(now())
		WHERE scope = 'global' AND site_id = '' AND key = $1`, restartreq.Key(c)); err != nil {
		t.Fatal(err)
	}
	f.ran(c, 10*time.Minute, 4*time.Minute)
	want("asked now, last beat 4 min ago", RestartNotReporting)
	f.ran(c, 10*time.Minute, 2*time.Minute)
	want("asked now, last beat 2 min ago", RestartPending)

	// Honoured: a start at the request or after it, by
	// restartreq.Satisfied. At the very instant counts - the service
	// exits only while the request is after its start.
	at, _, _ := f.request(c)
	f.startedAt(c, at.Add(-time.Microsecond))
	want("started a microsecond before the request", RestartPending)
	f.startedAt(c, at)
	got = want("started at the request", RestartDone)
	if !got.Started.Equal(at) {
		t.Errorf("the status shows the start at %v, the heartbeat holds %v", got.Started, at)
	}
	f.startedAt(c, at.Add(2*time.Second))
	want("started after the request", RestartDone)
}

// TestTheStatusNamesWhoCannotBeAskedAndKeepsTheListsOrder: the read API
// cannot hear even with a request row and a fresh heartbeat; the order is
// relupdate.HealthServices'.
func TestTheStatusNamesWhoCannotBeAskedAndKeepsTheListsOrder(t *testing.T) {
	f := newRestartFixture(t)
	ctx := context.Background()
	f.ran(testdb.Reader, 10*time.Minute, 5*time.Second)
	// Written by hand: the panel would refuse it, which is the point - a
	// row left from before a grant was revoked still cannot be heard.
	if _, err := f.admin.Exec(ctx, `INSERT INTO panel_settings (scope, site_id, key, value)
		VALUES ('global', '', $1, to_jsonb(now()))`, restartreq.Key(testdb.Reader)); err != nil {
		t.Fatal(err)
	}

	st, err := f.store.RestartStatus(ctx, f.owner)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Allowed {
		t.Error("an owner is not allowed to press")
	}
	if len(st.Services) != len(relupdate.HealthServices) {
		t.Fatalf("%d services in the status, want %d", len(st.Services), len(relupdate.HealthServices))
	}
	for i, s := range st.Services {
		if s.Service != relupdate.HealthServices[i] {
			t.Errorf("row %d is %q, want %q", i, s.Service, relupdate.HealthServices[i])
		}
		if s.Service == testdb.Reader && s.State != RestartCannotHear {
			t.Errorf("the read API's state is %q, want %q", s.State, RestartCannotHear)
		}
		if s.State.Askable() != (s.State != RestartCannotHear && s.State != RestartNeverReported) {
			t.Errorf("%s: Askable() = %v in state %q", s.Service, s.State.Askable(), s.State)
		}
	}

	viewer := f.owner
	viewer.Role = RoleViewer
	if st, err := f.store.RestartStatus(ctx, viewer); err != nil || st.Allowed {
		t.Errorf("a viewer's status says Allowed = %v (%v)", st.Allowed, err)
	}
}

// TestAValueThatIsNotATimeIsNoRequestHereEither: the service reads a row
// whose value is not a time as no request (restartreq.ParseRequest), so
// the page must too - and must go on drawing the other services, rather
// than a cast in SQL turning one row into an unreadable section.
func TestAValueThatIsNotATimeIsNoRequestHereEither(t *testing.T) {
	f := newRestartFixture(t)
	ctx := context.Background()
	c, b := testdb.Collector, testdb.Beacon
	f.ran(c, 10*time.Minute, 5*time.Second)
	f.ran(b, 10*time.Minute, 5*time.Second)
	if _, err := f.store.RequestRestart(ctx, f.owner, f.auth(), b); err != nil {
		t.Fatal(err)
	}
	if _, err := f.admin.Exec(ctx, `
		INSERT INTO panel_settings (scope, site_id, key, value)
		VALUES ('global', '', $1, '"not a time"'::jsonb)
		ON CONFLICT (scope, site_id, key) DO UPDATE SET value = EXCLUDED.value`,
		restartreq.Key(c)); err != nil {
		t.Fatal(err)
	}
	status := f.status()
	if got := status[c]; got.State != RestartNone || !got.Requested.IsZero() {
		t.Errorf("a value that is not a time reads as %q at %v, want no request", got.State, got.Requested)
	}
	if got := status[b]; got.State != RestartPending {
		t.Errorf("the beacon's real request reads as %q beside a malformed row, want %q", got.State, RestartPending)
	}
}
