//go:build integration

package restartreq

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// # Whose rows these are
//
// Every request this suite writes is under test.restartreq.*, and the
// watchers it runs read that key through the unexported key override -
// while connected as the real role, so the privilege question is the
// real one.
//
// Not process.restart.<anything>, for two reasons, the second measured.
// The panel's own suites will write process.restart.<real role>, and
// reading one of those here would make this suite's verdicts depend on
// another package's timing. And internal/panel's settings suite clears
// the table on its way out with DELETE ... WHERE key NOT LIKE 'test.%' -
// "the panel owns every unprefixed key", internal/settings' live suite
// says, "this suite owns test.". The first version of this file wrote
// process.restart.restartreq-*, and with internal/panel running beside
// it four runs in four lost rows mid-test ("no rows in result set" for a
// key the test had just written).
//
// So no suite lock: none guards panel_settings as a whole (the one that
// exists, IPModeSettingLock, guards one row), and these rows are this
// suite's alone. Each test deletes its own key before it writes and
// again when it ends, by its exact name.

// tracer records every statement a watcher's pool sends and how it
// ended. A statement the server refused comes back as a *pgconn.PgError,
// so "no server errors" here is "no ERROR in the server's log from these
// connections".
type tracer struct {
	mu    sync.Mutex
	stmts []traced
}

type traced struct {
	sql  string
	args []any
	err  error
}

type traceKey struct{}

func (tr *tracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, traceKey{}, traced{sql: data.SQL, args: data.Args})
}

func (tr *tracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	stmt, _ := ctx.Value(traceKey{}).(traced)
	stmt.err = data.Err
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.stmts = append(tr.stmts, stmt)
}

// reads are the statements that read panel_settings, and asks the ones
// that asked whether they may.
func (tr *tracer) counts() (reads, asks int, readKeys []string, serverErrs []error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	for _, s := range tr.stmts {
		switch {
		case strings.Contains(s.sql, "FROM panel_settings"):
			reads++
			if len(s.args) == 1 {
				if k, ok := s.args[0].(string); ok {
					readKeys = append(readKeys, k)
				}
			}
		case strings.Contains(s.sql, "has_table_privilege"):
			asks++
		}
		var pgErr *pgconn.PgError
		if errors.As(s.err, &pgErr) {
			serverErrs = append(serverErrs, pgErr)
		}
	}
	return
}

// watchPool is a pool shaped like resources.OpenMonitor's - two
// connections - as role, with a tracer on it.
func watchPool(t *testing.T, role string, tr *tracer) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testdb.DSN(role))
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 2
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool as %s: %v", role, err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping as %s: %v", role, err)
	}
	return pool
}

// ownKey names this test's request row and removes it at both ends.
func ownKey(t *testing.T, panel *pgxpool.Pool, name string) string {
	t.Helper()
	key := "test.restartreq." + name
	wipe := func() {
		if _, err := panel.Exec(context.Background(),
			`DELETE FROM panel_settings WHERE scope = 'global' AND site_id = '' AND key = $1`, key); err != nil {
			t.Errorf("removing %s: %v", key, err)
		}
	}
	wipe()
	var left int
	if err := panel.QueryRow(context.Background(),
		`SELECT count(*) FROM panel_settings WHERE key = $1`, key).Scan(&left); err != nil || left != 0 {
		t.Fatalf("%s still has %d rows after the first clean (%v)", key, left, err)
	}
	t.Cleanup(wipe)
	return key
}

// write stores a request the way the panel does, as panel_user, with
// value given as an SQL expression.
func write(t *testing.T, panel *pgxpool.Pool, key, valueSQL string, args ...any) {
	t.Helper()
	_, err := panel.Exec(context.Background(), `
		INSERT INTO panel_settings (scope, site_id, key, value)
		VALUES ('global', '', $1, `+valueSQL+`)
		ON CONFLICT (scope, site_id, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		append([]any{key}, args...)...)
	if err != nil {
		t.Fatalf("writing %s: %v", key, err)
	}
}

func dbNow(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	if err := pool.QueryRow(context.Background(), `SELECT now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

// run is one watcher, run for at most d or until it exits, the way a
// main runs it: Exit cancels the context the watcher runs under. The
// backstop is pointed at a counter - os.Exit would end the test binary.
type run struct {
	w      *Watcher
	log    *recorder
	tr     *tracer
	exits  atomic.Int32
	hard   atomic.Int32
	exitAt atomic.Int64
}

func newRun(t *testing.T, role, key string, started func() (time.Time, bool)) *run {
	t.Helper()
	r := &run{log: &recorder{}, tr: &tracer{}}
	r.w = &Watcher{
		Pool:       watchPool(t, role, r.tr),
		Started:    started,
		Interval:   50 * time.Millisecond,
		ExitWithin: time.Hour,
		Logger:     slog.New(r.log),
		key:        key,
		hardExit:   func(int) { r.hard.Add(1) },
	}
	return r
}

func startedAt(at time.Time) func() (time.Time, bool) {
	return func() (time.Time, bool) { return at, true }
}

// watch runs the watcher for d, or until it exits, and reports whether it
// returned on its own.
func (r *run) watch(d time.Duration) (exited bool) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	r.w.Exit = func() {
		r.exits.Add(1)
		r.exitAt.Store(time.Now().UnixNano())
		cancel()
	}
	r.w.Run(ctx)
	return r.exits.Load() > 0
}

// An old request is not a restart, and the same row against an earlier
// start is: the pair shows the row was read and compared.
func TestARequestOlderThanTheStartIsNotARestart(t *testing.T) {
	panel := testdb.Pool(t, "panel_user")
	key := ownKey(t, panel, "old")
	write(t, panel, key, `to_jsonb(now() - interval '1 hour')`)
	now := dbNow(t, panel)

	r := newRun(t, "collector", key, startedAt(now))
	if r.watch(600 * time.Millisecond) {
		t.Fatalf("exited for a request an hour older than the start\n%s", r.log.dump())
	}
	reads, _, keys, errs := r.tr.counts()
	if reads < 5 {
		t.Errorf("%d reads in 600ms at a 50ms interval; the watcher was not reading", reads)
	}
	for _, k := range keys {
		if k != key {
			t.Errorf("read key %q, want %q", k, key)
		}
	}
	if len(errs) != 0 {
		t.Errorf("the server refused %d statements: %v", len(errs), errs)
	}

	r = newRun(t, "collector", key, startedAt(now.Add(-2*time.Hour)))
	if !r.watch(5 * time.Second) {
		t.Fatalf("did not exit for the same row against a start before it\n%s", r.log.dump())
	}
	if n := r.exits.Load(); n != 1 {
		t.Errorf("Exit called %d times, want 1", n)
	}
}

// A request written after the start: Exit once. And the process that
// replaces it - started after the exit - is not asked again: no loop.
func TestANewRequestRestartsOnceAndTheNextProcessStays(t *testing.T) {
	panel := testdb.Pool(t, "panel_user")
	key := ownKey(t, panel, "new")
	started := dbNow(t, panel)
	write(t, panel, key, `to_jsonb(now())`)

	r := newRun(t, "collector", key, startedAt(started))
	begun := time.Now()
	if !r.watch(5 * time.Second) {
		t.Fatalf("did not exit for a request newer than the start\n%s", r.log.dump())
	}
	if n := r.exits.Load(); n != 1 {
		t.Errorf("Exit called %d times, want 1", n)
	}
	t.Logf("exited %s after the watcher began", time.Unix(0, r.exitAt.Load()).Sub(begun))
	lines := r.log.lines(slog.LevelInfo, "exiting")
	if len(lines) != 1 || lines[0].attrs["service"] != "collector" {
		t.Errorf("want one exit line naming collector\n%s", r.log.dump())
	}
	if r.hard.Load() != 0 {
		t.Error("the backstop fired within an hour's ExitWithin")
	}

	// The next process: its start is the database's clock after the exit.
	next := newRun(t, "collector", key, startedAt(dbNow(t, panel)))
	if next.watch(600 * time.Millisecond) {
		t.Fatalf("the restarted process exited again for the same request\n%s", next.log.dump())
	}
	if reads, _, _, _ := next.tr.counts(); reads < 5 {
		t.Errorf("%d reads; the second watcher was not reading", reads)
	}
}

// No row, no restart - and the watcher was asking.
func TestNoRowIsNoRestart(t *testing.T) {
	panel := testdb.Pool(t, "panel_user")
	key := ownKey(t, panel, "none")
	r := newRun(t, "collector", key, startedAt(dbNow(t, panel).Add(-time.Hour)))
	if r.watch(600 * time.Millisecond) {
		t.Fatalf("exited with no request\n%s", r.log.dump())
	}
	reads, _, _, errs := r.tr.counts()
	if reads < 5 || len(errs) != 0 {
		t.Errorf("%d reads, %d server errors; want at least 5 and none", reads, len(errs))
	}
	if n := len(r.log.all()); n != 0 {
		t.Errorf("%d log lines with nothing to say\n%s", n, r.log.dump())
	}
}

// The role that cannot read panel_settings today: the read API's.
// Nothing reaches the server's log, one Info line here, the question is
// asked again on its interval, and the table is never read.
func TestARoleThatCannotReadLeavesNoErrorAndOneLine(t *testing.T) {
	panel := testdb.Pool(t, "panel_user")
	key := ownKey(t, panel, "noread")
	write(t, panel, key, `to_jsonb(now())`)

	r := newRun(t, "analytics_reader", key, startedAt(dbNow(t, panel).Add(-time.Hour)))
	r.w.recheck = 100 * time.Millisecond
	if r.watch(700 * time.Millisecond) {
		t.Fatalf("exited without being able to read\n%s", r.log.dump())
	}
	reads, asks, _, errs := r.tr.counts()
	if reads != 0 {
		t.Errorf("read panel_settings %d times as a role that may not", reads)
	}
	if asks < 3 {
		t.Errorf("asked %d times in 700ms with a 100ms recheck; want at least 3", asks)
	}
	if len(errs) != 0 {
		t.Errorf("the server refused %d statements: %v", len(errs), errs)
	}
	lines := r.log.lines(slog.LevelInfo, "cannot read")
	if len(lines) != 1 || lines[0].attrs["service"] != "analytics_reader" {
		t.Errorf("want exactly one Info line naming analytics_reader\n%s", r.log.dump())
	}
	if n := len(r.log.all()); n != 1 {
		t.Errorf("%d log lines, want the one\n%s", n, r.log.dump())
	}

	// Why the question is asked first: the read itself, as this role, is
	// an ERROR in the server's log. And the question is honest for a role
	// that may: the pair.
	reader := testdb.Pool(t, "analytics_reader")
	_, _, _, err := poolDB{reader}.request(context.Background(), key)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Errorf("reading as analytics_reader: %v, want permission denied (42501)", err)
	}
	if role, may, err := (poolDB{reader}).whoAndMay(context.Background()); err != nil || may || role != "analytics_reader" {
		t.Errorf("whoAndMay as analytics_reader = %q, %v, %v; want analytics_reader, false, nil", role, may, err)
	}
	for _, role := range []string{"collector", "beacon_writer"} {
		if got, may, err := (poolDB{testdb.Pool(t, role)}).whoAndMay(context.Background()); err != nil || !may || got != role {
			t.Errorf("whoAndMay as %s = %q, %v, %v; want %s, true, nil", role, got, may, err, role)
		}
	}
}

// Until the start is known: nothing read, nothing done, whatever the row.
func TestAnUnknownStartIsNoRestart(t *testing.T) {
	panel := testdb.Pool(t, "panel_user")
	key := ownKey(t, panel, "unknown")
	write(t, panel, key, `to_jsonb(now())`)
	r := newRun(t, "collector", key, func() (time.Time, bool) { return time.Time{}, false })
	if r.watch(500 * time.Millisecond) {
		t.Fatalf("exited before the start was known\n%s", r.log.dump())
	}
	if reads, asks, _, _ := r.tr.counts(); reads != 0 || asks != 1 {
		t.Errorf("%d reads and %d questions; want 0 and 1", reads, asks)
	}
}

// A row somebody wrote that holds no time: no exit, one warning, and no
// ERROR in the server's log - which the draft's cast in SQL would have
// raised on every interval.
func TestAValueThatIsNotATimeIsIgnoredQuietly(t *testing.T) {
	panel := testdb.Pool(t, "panel_user")
	for i, raw := range []string{`"garbage"`, `123`, `{}`, `null`, `"infinity"`} {
		t.Run(raw, func(t *testing.T) {
			key := ownKey(t, panel, "malformed-"+string(rune('a'+i)))
			write(t, panel, key, `$2::jsonb`, raw)
			r := newRun(t, "collector", key, startedAt(dbNow(t, panel).Add(-time.Hour)))
			if r.watch(500 * time.Millisecond) {
				t.Fatalf("exited on %s\n%s", raw, r.log.dump())
			}
			reads, _, _, errs := r.tr.counts()
			if reads < 4 || len(errs) != 0 {
				t.Errorf("%d reads, server errors %v; want at least 4 and none", reads, errs)
			}
			if n := len(r.log.lines(slog.LevelWarn, "does not hold a time")); n != 1 {
				t.Errorf("%d warnings, want 1\n%s", n, r.log.dump())
			}

			// The cast the draft used, on the same row.
			var at time.Time
			err := panel.QueryRow(context.Background(),
				`SELECT (value #>> '{}')::timestamptz FROM panel_settings WHERE key = $1`, key).Scan(&at)
			if errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("%s is gone: something removed this test's row while it ran, so "+
					"nothing above measured it", key)
			}
			var pgErr *pgconn.PgError
			if raw != `"infinity"` && raw != `null` && !errors.As(err, &pgErr) {
				t.Errorf("the cast in SQL did not raise on %s (err %v); the premise of parsing in Go is gone", raw, err)
			}
		})
	}
}

// A request ahead of the database's clock waits for it, then acts once -
// and the process after it stays. Without the wait, the second half is a
// restart every few seconds until the clock reaches the request.
func TestARequestAheadOfTheClockWaitsAndDoesNotLoop(t *testing.T) {
	panel := testdb.Pool(t, "panel_user")
	key := ownKey(t, panel, "future")
	write(t, panel, key, `to_jsonb(now() + interval '1 hour')`)
	r := newRun(t, "collector", key, startedAt(dbNow(t, panel)))
	if r.watch(500 * time.Millisecond) {
		t.Fatalf("exited for a request an hour ahead\n%s", r.log.dump())
	}
	if n := len(r.log.lines(slog.LevelWarn, "later than the database's clock")); n != 1 {
		t.Errorf("%d warnings, want 1\n%s", n, r.log.dump())
	}

	// Due in a second and a half: not before, once after.
	write(t, panel, key, `to_jsonb(now() + interval '1500 milliseconds')`)
	var due time.Time
	if err := panel.QueryRow(context.Background(),
		`SELECT (value #>> '{}')::timestamptz FROM panel_settings WHERE key = $1`, key).Scan(&due); err != nil {
		t.Fatal(err)
	}
	r = newRun(t, "collector", key, startedAt(dbNow(t, panel)))
	if !r.watch(10 * time.Second) {
		t.Fatalf("did not exit once the request was due\n%s", r.log.dump())
	}
	exitedAt := time.Unix(0, r.exitAt.Load())
	// The database is on this machine, so its clock and ours are one.
	if exitedAt.Before(due) {
		t.Errorf("exited at %s, before the request was due at %s",
			exitedAt.UTC().Format(time.RFC3339Nano), due.UTC().Format(time.RFC3339Nano))
	}

	next := newRun(t, "collector", key, startedAt(dbNow(t, panel)))
	if next.watch(500 * time.Millisecond) {
		t.Fatalf("the next process exited again for the same request\n%s", next.log.dump())
	}
}

// Without the override, the key is the connection's own role. Read only:
// the start is an hour ahead, so whatever the panel's suites may have
// written under process.restart.collector is satisfied, and this test
// writes nothing there.
func TestTheKeyReadIsTheConnectionsRole(t *testing.T) {
	panel := testdb.Pool(t, "panel_user")
	r := newRun(t, "collector", "", startedAt(dbNow(t, panel).Add(time.Hour)))
	if r.watch(300 * time.Millisecond) {
		t.Fatalf("exited with a start an hour ahead\n%s", r.log.dump())
	}
	_, _, keys, _ := r.tr.counts()
	if len(keys) == 0 {
		t.Fatal("no read was made")
	}
	for _, k := range keys {
		if k != "process.restart.collector" {
			t.Errorf("read key %q, want process.restart.collector", k)
		}
	}
}

// A row under the same key for one site is not a request: only the
// deployment-wide row is. The pair is the same request in the global row.
func TestOnlyTheDeploymentWideRowIsARequest(t *testing.T) {
	panel := testdb.Pool(t, "panel_user")
	key := ownKey(t, panel, "scope")
	const site = "restartreq-scope-site"
	wipeSite := func() {
		if _, err := panel.Exec(context.Background(),
			`DELETE FROM panel_settings WHERE scope = 'site' AND site_id = $1 AND key = $2`, site, key); err != nil {
			t.Errorf("removing the site row: %v", err)
		}
	}
	wipeSite()
	t.Cleanup(wipeSite)
	started := dbNow(t, panel)
	if _, err := panel.Exec(context.Background(), `
		INSERT INTO panel_settings (scope, site_id, key, value)
		VALUES ('site', $1, $2, to_jsonb(now()))`, site, key); err != nil {
		t.Fatalf("writing the site row: %v", err)
	}

	r := newRun(t, "collector", key, startedAt(started))
	if r.watch(500 * time.Millisecond) {
		t.Fatalf("exited for a site's row under the request key\n%s", r.log.dump())
	}
	if reads, _, _, _ := r.tr.counts(); reads < 4 {
		t.Errorf("%d reads; the watcher was not reading", reads)
	}

	write(t, panel, key, `to_jsonb(now())`)
	r = newRun(t, "collector", key, startedAt(started))
	if !r.watch(5 * time.Second) {
		t.Fatalf("did not exit for the same request in the deployment-wide row\n%s", r.log.dump())
	}
}
