package restartreq

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// The rule, at its boundary, from both sides.
func TestSatisfiedIsTheOneBoundary(t *testing.T) {
	started := time.Date(2026, 11, 1, 10, 0, 0, 123456000, time.UTC)
	istanbul := time.FixedZone("+03", 3*60*60)

	for _, tc := range []struct {
		name      string
		requested time.Time
		want      bool
	}{
		{"the same instant", started, true},
		{"the same instant in another zone", started.In(istanbul), true},
		{"one microsecond before the start", started.Add(-time.Microsecond), true},
		{"an hour before the start", started.Add(-time.Hour), true},
		{"one microsecond after the start", started.Add(time.Microsecond), false},
		{"an hour after the start", started.Add(time.Hour), false},
	} {
		if got := Satisfied(tc.requested, started); got != tc.want {
			t.Errorf("%s: Satisfied(%s, %s) = %v, want %v",
				tc.name, tc.requested.Format(time.RFC3339Nano), started.Format(time.RFC3339Nano), got, tc.want)
		}
	}
}

// The key is spelled out here rather than built from KeyPrefix: the panel
// writes it and the service reads it, and a test that took the prefix
// from the constant would follow the constant anywhere.
func TestTheKeyIsTheRoleUnderProcessRestart(t *testing.T) {
	if got := Key("collector"); got != "process.restart.collector" {
		t.Errorf("Key(collector) = %q, want process.restart.collector", got)
	}
	if got := Key("beacon_writer"); got != "process.restart.beacon_writer" {
		t.Errorf("Key(beacon_writer) = %q, want process.restart.beacon_writer", got)
	}
}

// What to_jsonb(now()) writes - measured on PostgreSQL 16.15, with the
// session in UTC, in Asia/Kolkata, and truncated to the second - and what
// it does not.
func TestParseRequestReadsWhatThePanelWrites(t *testing.T) {
	want := time.Date(2026, 11, 1, 9, 59, 58, 949582000, time.UTC)
	for _, raw := range []string{
		`"2026-11-01T09:59:58.949582+00:00"`,
		`"2026-11-01T15:29:58.949582+05:30"`,
	} {
		got, err := ParseRequest([]byte(raw))
		if err != nil {
			t.Errorf("ParseRequest(%s): %v", raw, err)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("ParseRequest(%s) = %s, want %s", raw, got.UTC().Format(time.RFC3339Nano), want.Format(time.RFC3339Nano))
		}
	}
	if got, err := ParseRequest([]byte(`"2026-11-01T15:29:58+05:30"`)); err != nil ||
		!got.Equal(time.Date(2026, 11, 1, 9, 59, 58, 0, time.UTC)) {
		t.Errorf("whole seconds: got %v, %v", got, err)
	}

	for _, raw := range []string{
		`"garbage"`, `123`, `{}`, `[]`, `null`, `true`, `""`,
		// to_jsonb('infinity'::timestamptz)
		`"infinity"`,
		// PostgreSQL's text form rather than its JSON form
		`"2026-11-01 09:59:58+00"`,
		// a historical zone offset with seconds, which RFC 3339 cannot say
		`"1850-01-01T05:33:56+05:53:28"`,
		`not json`,
	} {
		if got, err := ParseRequest([]byte(raw)); err == nil {
			t.Errorf("ParseRequest(%s) = %s, want an error", raw, got)
		}
	}
}

// fakeDB answers the two questions from fields a test sets.
type fakeDB struct {
	mu      sync.Mutex
	role    string
	may     bool
	whoErr  error
	value   []byte
	found   bool
	now     time.Time
	readErr error

	whoCalls  int
	readCalls int
	keys      []string
}

func (f *fakeDB) whoAndMay(ctx context.Context) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.whoCalls++
	return f.role, f.may, f.whoErr
}

func (f *fakeDB) request(ctx context.Context, key string) ([]byte, bool, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readCalls++
	f.keys = append(f.keys, key)
	if f.readErr != nil {
		return nil, false, time.Time{}, f.readErr
	}
	return f.value, f.found, f.now, nil
}

func (f *fakeDB) set(fn func(*fakeDB)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeDB) calls() (who, read int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.whoCalls, f.readCalls
}

// jsonTime is what the panel's to_jsonb(now()) would have written for at.
func jsonTime(at time.Time) []byte {
	return []byte(`"` + at.Format("2006-01-02T15:04:05.999999-07:00") + `"`)
}

// recorder is a slog handler that keeps every record.
type recorder struct {
	mu   sync.Mutex
	recs []record
}

type record struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

func (r *recorder) Enabled(context.Context, slog.Level) bool { return true }
func (r *recorder) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *recorder) WithGroup(string) slog.Handler            { return r }
func (r *recorder) Handle(_ context.Context, rec slog.Record) error {
	attrs := map[string]string{}
	rec.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recs = append(r.recs, record{rec.Level, rec.Message, attrs})
	return nil
}

// lines are the records at level whose message contains part.
func (r *recorder) lines(level slog.Level, part string) []record {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []record
	for _, rec := range r.recs {
		if rec.level == level && strings.Contains(rec.msg, part) {
			out = append(out, rec)
		}
	}
	return out
}

func (r *recorder) all() []record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]record(nil), r.recs...)
}

func (r *recorder) dump() string {
	var b strings.Builder
	for _, rec := range r.all() {
		fmt.Fprintf(&b, "  %s %q %v\n", rec.level, rec.msg, rec.attrs)
	}
	return b.String()
}

// harness is a watcher on a fake database, stepped by hand.
type harness struct {
	w      *Watcher
	db     *fakeDB
	log    *recorder
	logger *slog.Logger
	st     state

	mu      sync.Mutex
	started time.Time
	known   bool
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		db:  &fakeDB{role: "collector", may: true},
		log: &recorder{},
	}
	h.logger = slog.New(h.log)
	h.w = &Watcher{
		Started: func() (time.Time, bool) {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.started, h.known
		},
		Exit:   func() { t.Error("Exit called by a test that steps by hand") },
		Logger: h.logger,
		db:     h.db,
	}
	return h
}

func (h *harness) setStarted(at time.Time, known bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started, h.known = at, known
}

func (h *harness) step() bool { return h.w.step(context.Background(), &h.st, h.logger) }

// The comparison in the loop is Satisfied, in both directions, and the
// exit line says why.
func TestAStepExitsOnlyForARequestNewerThanTheStart(t *testing.T) {
	dbNow := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	started := dbNow.Add(-time.Minute)

	for _, tc := range []struct {
		name      string
		requested time.Time
		want      bool
	}{
		{"an hour before the start", started.Add(-time.Hour), false},
		{"exactly at the start", started, false},
		{"one microsecond after the start", started.Add(time.Microsecond), true},
		{"at the database's now", dbNow, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.setStarted(started, true)
			h.db.set(func(f *fakeDB) { f.value, f.found, f.now = jsonTime(tc.requested), true, dbNow })
			if got := h.step(); got != tc.want {
				t.Fatalf("step() = %v, want %v\n%s", got, tc.want, h.log.dump())
			}
			exits := h.log.lines(slog.LevelInfo, "exiting")
			if tc.want && (len(exits) != 1 || exits[0].attrs["service"] != "collector") {
				t.Errorf("an exit must say so once, naming the service; got\n%s", h.log.dump())
			}
			if !tc.want && len(exits) != 0 {
				t.Errorf("no exit, and a line saying there was one:\n%s", h.log.dump())
			}
		})
	}
}

// The key read is the connection's role under the prefix - or the test
// override, which exists only so integration tests need not read a real
// role's key.
func TestTheRequestIsReadUnderTheConnectionsRole(t *testing.T) {
	h := newHarness(t)
	h.db.set(func(f *fakeDB) { f.role = "beacon_writer" })
	h.setStarted(time.Now(), true)
	h.step()
	if len(h.db.keys) != 1 || h.db.keys[0] != "process.restart.beacon_writer" {
		t.Errorf("read keys %q, want [process.restart.beacon_writer]", h.db.keys)
	}

	h = newHarness(t)
	h.w.key = "test.restartreq.unit"
	h.setStarted(time.Now(), true)
	h.step()
	if len(h.db.keys) != 1 || h.db.keys[0] != "test.restartreq.unit" {
		t.Errorf("read keys %q, want [test.restartreq.unit]", h.db.keys)
	}
}

// Until the start is known nothing is read and nothing happens, however
// new the request.
func TestNothingHappensUntilTheStartIsKnown(t *testing.T) {
	h := newHarness(t)
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	h.db.set(func(f *fakeDB) { f.value, f.found, f.now = jsonTime(now), true, now })
	for i := 0; i < 3; i++ {
		if h.step() {
			t.Fatal("exited before the start was known")
		}
	}
	if _, reads := h.db.calls(); reads != 0 {
		t.Errorf("%d reads before the start was known, want 0", reads)
	}

	// The same row, once the start is known and older than it: the pair
	// that shows the first half was the start, not the row.
	h.setStarted(now.Add(-time.Minute), true)
	if !h.step() {
		t.Fatalf("did not exit once the start was known\n%s", h.log.dump())
	}
}

// A role that may not read: one Info line, no read, and the question
// asked again only on its own interval - after which a grant is noticed.
func TestARoleThatCannotReadIsSaidOnceAndAskedAgainLater(t *testing.T) {
	h := newHarness(t)
	h.db.set(func(f *fakeDB) { f.role, f.may = "analytics_reader", false })
	h.setStarted(time.Now(), true)
	for i := 0; i < 5; i++ {
		if h.step() {
			t.Fatal("exited without being able to read")
		}
	}
	who, reads := h.db.calls()
	if who != 1 || reads != 0 {
		t.Errorf("asked %d times and read %d times within the recheck, want 1 and 0", who, reads)
	}
	lines := h.log.lines(slog.LevelInfo, "cannot read")
	if len(lines) != 1 || lines[0].attrs["service"] != "analytics_reader" {
		t.Errorf("want one Info line naming analytics_reader, got\n%s", h.log.dump())
	}

	// The recheck comes due and the answer is still no: asked, not said.
	h.st.askedAt = time.Now().Add(-privilegeRecheck)
	h.step()
	if who, _ := h.db.calls(); who != 2 {
		t.Errorf("asked %d times after the recheck came due, want 2", who)
	}
	if n := len(h.log.lines(slog.LevelInfo, "cannot read")); n != 1 {
		t.Errorf("%d cannot-read lines after a second no, want still 1\n%s", n, h.log.dump())
	}

	// The recheck not yet due, and the grant already there: not noticed.
	h.db.set(func(f *fakeDB) { f.may = true })
	h.st.askedAt = time.Now().Add(-privilegeRecheck + time.Minute)
	h.step()
	if who, reads := h.db.calls(); who != 2 || reads != 0 {
		t.Errorf("asked %d, read %d before the recheck was due, want 2 and 0", who, reads)
	}
	// Due: noticed, said, and read from then on.
	h.st.askedAt = time.Now().Add(-privilegeRecheck)
	h.step()
	if _, reads := h.db.calls(); reads != 1 {
		t.Errorf("read %d times after the grant was noticed, want 1", reads)
	}
	if n := len(h.log.lines(slog.LevelInfo, "can now read")); n != 1 {
		t.Errorf("%d can-now-read lines, want 1\n%s", n, h.log.dump())
	}
}

// A role that can read is not asked again: the answer only changes by a
// grant being removed, and the read then says so itself.
func TestAPermittedRoleIsNotAskedAgain(t *testing.T) {
	h := newHarness(t)
	h.setStarted(time.Now(), true)
	h.step()
	h.st.askedAt = time.Now().Add(-2 * privilegeRecheck)
	h.step()
	if who, reads := h.db.calls(); who != 1 || reads != 2 {
		t.Errorf("asked %d, read %d, want 1 and 2", who, reads)
	}
}

// The grant removed under a running service: one ERROR in the server's
// log, one Info here, and back to the silent question.
func TestARevokedGrantGoesBackToTheSilentQuestion(t *testing.T) {
	h := newHarness(t)
	h.setStarted(time.Now(), true)
	h.step()
	h.db.set(func(f *fakeDB) {
		f.readErr = &pgconn.PgError{Code: "42501", Message: "permission denied for table panel_settings"}
		f.may = false
	})
	for i := 0; i < 4; i++ {
		h.step()
	}
	who, reads := h.db.calls()
	if reads != 2 {
		t.Errorf("read %d times, want 2: the one before the revoke and the one that met it", reads)
	}
	if who != 1 {
		t.Errorf("asked %d times within the recheck, want 1", who)
	}
	if n := len(h.log.lines(slog.LevelInfo, "any more")); n != 1 {
		t.Errorf("%d any-more lines, want 1\n%s", n, h.log.dump())
	}
	if n := len(h.log.lines(slog.LevelWarn, "")); n != 0 {
		t.Errorf("a revoked grant is not a failure to read; %d warnings\n%s", n, h.log.dump())
	}
}

// Each failure is said once, and its recovery once - and one recovering
// does not silence the other.
func TestEachFailureIsSaidOnceAndItsRecoveryOnce(t *testing.T) {
	h := newHarness(t)
	h.setStarted(time.Now(), true)
	h.db.set(func(f *fakeDB) { f.whoErr = errors.New("connection refused") })
	for i := 0; i < 3; i++ {
		h.step()
	}
	if n := len(h.log.lines(slog.LevelWarn, "could not ask")); n != 1 {
		t.Errorf("%d could-not-ask lines over three failures, want 1\n%s", n, h.log.dump())
	}

	// The first question recovers, and the read fails at once.
	h.db.set(func(f *fakeDB) { f.whoErr, f.readErr = nil, errors.New("connection reset") })
	for i := 0; i < 3; i++ {
		h.step()
	}
	if n := len(h.log.lines(slog.LevelWarn, "could not read")); n != 1 {
		t.Errorf("%d could-not-read lines over three failures, want 1\n%s", n, h.log.dump())
	}
	h.db.set(func(f *fakeDB) { f.readErr = nil })
	h.step()
	h.step()
	if n := len(h.log.lines(slog.LevelInfo, "read again")); n != 1 {
		t.Errorf("%d recovery lines, want 1\n%s", n, h.log.dump())
	}
	h.db.set(func(f *fakeDB) { f.readErr = errors.New("connection reset") })
	h.step()
	if n := len(h.log.lines(slog.LevelWarn, "could not read")); n != 2 {
		t.Errorf("a failure after a recovery is a new one; %d lines, want 2\n%s", n, h.log.dump())
	}
}

// The privilege question failing again after it once succeeded is a new
// failure, and said: the first question's flag is reset by its own
// success, not by the read's.
func TestTheFirstQuestionFailingAgainIsSaidAgain(t *testing.T) {
	h := newHarness(t)
	h.setStarted(time.Now(), true)
	h.db.set(func(f *fakeDB) { f.whoErr = errors.New("connection refused") })
	h.step()
	h.db.set(func(f *fakeDB) { f.whoErr, f.role, f.may = nil, "analytics_reader", false })
	h.step()
	h.db.set(func(f *fakeDB) { f.whoErr = errors.New("connection refused") })
	h.st.askedAt = time.Now().Add(-privilegeRecheck)
	h.step()
	if n := len(h.log.lines(slog.LevelWarn, "could not ask")); n != 2 {
		t.Errorf("%d could-not-ask lines for two separate failures, want 2\n%s", n, h.log.dump())
	}
}

// A question cut short because the service is stopping is not a failure.
func TestAStoppingServiceReportsNoFailure(t *testing.T) {
	for _, which := range []string{"who", "read"} {
		t.Run(which, func(t *testing.T) {
			h := newHarness(t)
			h.setStarted(time.Now(), true)
			if which == "read" {
				h.step()
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			h.db.set(func(f *fakeDB) {
				if which == "who" {
					f.whoErr = context.Canceled
				} else {
					f.readErr = context.Canceled
				}
			})
			h.w.step(ctx, &h.st, h.logger)
			if n := len(h.log.lines(slog.LevelWarn, "")); n != 0 {
				t.Errorf("%d warnings for a cancelled question\n%s", n, h.log.dump())
			}
			// The pair: the same error without the cancel is said.
			h.step()
			if n := len(h.log.lines(slog.LevelWarn, "")); n != 1 {
				t.Errorf("%d warnings for the same error uncancelled, want 1\n%s", n, h.log.dump())
			}
		})
	}
}

// A row that holds no time is reported once per value, and never exits.
func TestAMalformedRequestIsReportedOncePerValueAndIgnored(t *testing.T) {
	h := newHarness(t)
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	h.setStarted(now.Add(-time.Hour), true)
	h.db.set(func(f *fakeDB) { f.value, f.found, f.now = []byte(`"garbage"`), true, now })
	for i := 0; i < 4; i++ {
		if h.step() {
			t.Fatal("exited on a value that is not a time")
		}
	}
	lines := h.log.lines(slog.LevelWarn, "does not hold a time")
	if len(lines) != 1 || lines[0].attrs["value"] != `"garbage"` {
		t.Errorf("want one warning carrying the value, got\n%s", h.log.dump())
	}
	h.db.set(func(f *fakeDB) { f.value = []byte(`123`) })
	h.step()
	h.step()
	if n := len(h.log.lines(slog.LevelWarn, "does not hold a time")); n != 2 {
		t.Errorf("a different bad value is a new line; %d lines, want 2", n)
	}
	// Fixed by the panel: acted on.
	h.db.set(func(f *fakeDB) { f.value = jsonTime(now) })
	if !h.step() {
		t.Errorf("a valid request after a malformed one did not exit\n%s", h.log.dump())
	}
}

// A value somebody wrote is clipped in the line about it.
func TestAMalformedValueIsClippedInTheLog(t *testing.T) {
	h := newHarness(t)
	h.setStarted(time.Now(), true)
	long := `"` + strings.Repeat("x", 500) + `"`
	h.db.set(func(f *fakeDB) { f.value, f.found, f.now = []byte(long), true, time.Now() })
	h.step()
	lines := h.log.lines(slog.LevelWarn, "does not hold a time")
	if len(lines) != 1 || len(lines[0].attrs["value"]) != 64+len("...") {
		t.Errorf("want one line with the value clipped to 64 bytes and an ellipsis, got\n%s", h.log.dump())
	}
}

// A request later than the database's clock: not acted on, said once,
// and acted on once the clock reaches it. Without this a request an hour
// ahead restarts the service every few seconds for an hour - each new
// process starts before the request.
func TestARequestAheadOfTheDatabaseClockWaitsUntilItIsDue(t *testing.T) {
	h := newHarness(t)
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	requested := now.Add(time.Hour)
	h.setStarted(now.Add(-time.Minute), true)
	h.db.set(func(f *fakeDB) { f.value, f.found, f.now = jsonTime(requested), true, now })
	for i := 0; i < 3; i++ {
		if h.step() {
			t.Fatalf("exited for a request an hour ahead of the database's clock\n%s", h.log.dump())
		}
	}
	if n := len(h.log.lines(slog.LevelWarn, "later than the database's clock")); n != 1 {
		t.Errorf("%d warnings, want 1\n%s", n, h.log.dump())
	}

	// A process started after now but before the request - the one a
	// restart would have produced - is still not satisfied by Satisfied
	// alone. That is the loop the guard is for.
	h.setStarted(now.Add(time.Second), true)
	if h.step() {
		t.Fatal("exited for a request still ahead of the clock")
	}
	if Satisfied(requested, now.Add(time.Second)) {
		t.Fatal("the premise: a restart started now does not satisfy a request an hour ahead")
	}

	// One microsecond short of due, then due.
	h.db.set(func(f *fakeDB) { f.now = requested.Add(-time.Microsecond) })
	if h.step() {
		t.Fatal("exited a microsecond before the request was due")
	}
	h.db.set(func(f *fakeDB) { f.now = requested })
	if !h.step() {
		t.Fatalf("did not exit once the request was due\n%s", h.log.dump())
	}
}

// Run with no database is nothing at all; Run without Started or Exit
// says so once and stops, rather than panicking in a loop.
func TestRunWithoutItsPartsReturns(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		(&Watcher{Started: func() (time.Time, bool) { return time.Time{}, false }, Exit: func() {}}).Run(context.Background())
		var nilWatcher *Watcher
		nilWatcher.Run(context.Background())
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run without a pool did not return")
	}

	for _, tc := range []struct {
		name string
		w    func(*harness)
	}{
		{"no Started", func(h *harness) { h.w.Started = nil }},
		{"no Exit", func(h *harness) { h.w.Exit = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.w(h)
			done := make(chan struct{})
			go func() { defer close(done); h.w.Run(context.Background()) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Run did not return")
			}
			if n := len(h.log.lines(slog.LevelError, "without Started or Exit")); n != 1 {
				t.Errorf("%d error lines, want 1\n%s", n, h.log.dump())
			}
			if who, _ := h.db.calls(); who != 0 {
				t.Errorf("asked the database %d times with nothing to act on", who)
			}
		})
	}
}

// Run: asks at once and on every interval, calls Exit once, returns, and
// the backstop ends the process if Exit did not.
func TestRunExitsOnceAndTheBackstopFiresWhenTheProcessStays(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.setStarted(now.Add(-time.Minute), true)
	h.db.set(func(f *fakeDB) { f.value, f.found, f.now = jsonTime(now), true, now })

	const within = 300 * time.Millisecond
	exitAt := make(chan time.Time, 4)
	hardAt := make(chan time.Time, 4)
	codes := make(chan int, 4)
	h.w.Exit = func() { exitAt <- time.Now() } // does nothing: the process stays
	h.w.ExitWithin = within
	h.w.Interval = 20 * time.Millisecond
	h.w.hardExit = func(code int) { hardAt <- time.Now(); codes <- code }

	done := make(chan struct{})
	go func() { defer close(done); h.w.Run(context.Background()) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after exiting")
	}
	var exited time.Time
	select {
	case exited = <-exitAt:
	default:
		t.Fatal("Run returned without calling Exit")
	}

	var hard time.Time
	select {
	case hard = <-hardAt:
	case <-time.After(5 * time.Second):
		t.Fatal("the backstop did not fire when the process stayed")
	}
	if code := <-codes; code != 1 {
		t.Errorf("backstop exit code %d, want 1", code)
	}
	if waited := hard.Sub(exited); waited < within {
		t.Errorf("the backstop fired %s after Exit, before ExitWithin (%s)", waited, within)
	}
	if n := len(h.log.lines(slog.LevelError, "had not exited")); n != 1 {
		t.Errorf("%d backstop error lines, want 1\n%s", n, h.log.dump())
	}

	// Once: nothing more after a further interval's worth of waiting.
	time.Sleep(3 * h.w.Interval)
	if len(exitAt) != 0 || len(hardAt) != 0 {
		t.Errorf("Exit or the backstop ran again: %d, %d", len(exitAt), len(hardAt))
	}
}

// An Exit that blocks - a shutdown that hangs inside the call - must not
// keep the backstop from being armed.
func TestTheBackstopIsArmedBeforeExitIsCalled(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.setStarted(now.Add(-time.Minute), true)
	h.db.set(func(f *fakeDB) { f.value, f.found, f.now = jsonTime(now), true, now })

	block := make(chan struct{})
	defer close(block)
	fired := make(chan int, 1)
	h.w.Exit = func() { <-block }
	h.w.ExitWithin = 50 * time.Millisecond
	h.w.hardExit = func(code int) { fired <- code }

	go h.w.Run(context.Background())
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("the backstop did not fire while Exit was blocked")
	}
}

// Cancelling the context ends Run without an exit.
func TestRunEndsWithItsContext(t *testing.T) {
	h := newHarness(t)
	h.setStarted(time.Now(), true)
	h.w.Interval = 10 * time.Millisecond
	h.w.Exit = func() { t.Error("Exit called with no request") }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.w.Run(ctx) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not end with its context")
	}
	if _, reads := h.db.calls(); reads < 2 {
		t.Errorf("read %d times in 50ms at a 10ms interval; the loop is not ticking", reads)
	}
}

// stopWith ends a context with cause and calls the function LogStopping
// returned, the way a main does before "shutdown complete", and returns
// the stopping lines that exist when it returns. No sleep: the function
// returning is the claim that the line is already written.
func stopWith(t *testing.T, cause error) []record {
	t.Helper()
	log := &recorder{}
	ctx, cancel := context.WithCancelCause(context.Background())
	written := LogStopping(ctx, slog.New(log))
	cancel(cause)
	returned := make(chan struct{})
	go func() { defer close(returned); written() }()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the function LogStopping returned did not return after its context ended")
	}
	return log.lines(slog.LevelInfo, "stopping")
}

// The stopping line names its cause, and the causes a main's context can
// end with read differently: a restart request and a signal. Compared
// across calls, because one call cannot show the cause is read at all -
// a fixed sentence would pass it.
func TestTheStoppingLineSaysWhy(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  string
	}{
		{ErrRequested, "restart requested from the panel"},
		{errors.New("terminated signal received"), "terminated signal received"},
		{nil, "context canceled"},
	} {
		lines := stopWith(t, tc.cause)
		if len(lines) != 1 {
			t.Errorf("cause %v: want exactly one stopping line once the wait returned, got %d", tc.cause, len(lines))
			continue
		}
		if got := lines[0].attrs["cause"]; got != tc.want {
			t.Errorf("cause %v: the line says %q, want %q", tc.cause, got, tc.want)
		}
	}
}

// Many times, because the first version lost this race only sometimes:
// the line was written by a goroutine, and the real read API said
// "shutdown complete" before "stopping" - and a main that returns first
// exits with no "stopping" at all.
func TestTheStoppingLineIsWrittenBeforeTheWaitReturns(t *testing.T) {
	for i := 0; i < 200; i++ {
		if lines := stopWith(t, ErrRequested); len(lines) != 1 {
			t.Fatalf("round %d: %d stopping lines when the wait returned, want 1", i, len(lines))
		}
	}
}

// Nothing is written while the context is alive, and the wait returns at
// once then - a server that failed rather than one that was stopped must
// not hang its main - and returns again if called twice.
func TestNoStoppingLineWhileRunningAndTheWaitDoesNotHang(t *testing.T) {
	log := &recorder{}
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	written := LogStopping(ctx, slog.New(log))
	returned := make(chan struct{})
	go func() { defer close(returned); written(); written() }()
	select {
	case <-returned:
	case <-time.After(5 * time.Second):
		t.Fatal("the wait blocked on a context that had not ended")
	}
	cancel(ErrRequested)
	time.Sleep(50 * time.Millisecond)
	if n := len(log.all()); n != 0 {
		t.Errorf("%d lines written; a wait that returned before the context ended "+
			"must leave nothing behind:\n%s", n, log.dump())
	}
}
