//go:build integration

// A log line is text somebody else chose.
//
// The done criterion this file exists for, in the plan's words: *düşmanca
// dize (NUL, geçersiz UTF-8, 1 MB tek satır) yazıcıyı bozmuyor.* Every
// case below is something a visitor can put in a URL, a header or a user
// agent, which is how it reaches a log line in the first place.
//
// # Why every pool here comes from testdb
//
// The first version of this file opened one pool from CA_SUPERUSER_DSN
// and wrote every row as `postgres`. All four tests passed and none of
// them tested anything: PostgreSQL exempts superusers from row-level
// security outright, and the write policy compares `service` against
// current_user - so writing as postgres under the name postgres
// satisfies it by accident. The policies had been measured by hand at a
// psql prompt and the suite was guarding none of them.
//
// That is the failure internal/testdb was written to end, and its
// package comment lists three real bugs it hid the first time. So: the
// sink writes as a service role, the panel reads as the panel, and
// Admin appears only where a test has to remove what it made.
package logsink

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// newTestSink returns a sink writing as one service role, and the pool
// the panel reads it back through.
//
// Two pools rather than one because that is the shape in production: the
// collector holds INSERT and nothing else, so a sink that could read its
// own rows back would be running with authority no deployment gives it.
func newTestSink(t *testing.T, role string) (*Sink, *pgxpool.Pool) {
	t.Helper()
	write := testdb.Pool(t, role)
	read := testdb.Pool(t, testdb.Panel)

	level := &slog.LevelVar{}
	level.Set(slog.LevelDebug) // so the tests choose what is filtered, not the default
	s := New(write, Config{Service: role, Level: level, Buffer: 64})
	t.Cleanup(func() {
		s.Close()
		clearLines(t, role)
	})
	return s, read
}

// clearLines removes one service's rows through the schema's owner.
//
// Admin rather than the panel, though the panel holds DELETE: the sweep
// is itself under test below, and a cleanup that used it would turn a
// broken policy into rows leaking between tests instead of one red test.
func clearLines(t *testing.T, service string) {
	t.Helper()
	admin := testdb.Admin(t)
	if _, err := admin.Exec(context.Background(),
		`DELETE FROM panel_logs WHERE service = $1`, service); err != nil {
		t.Logf("clearing %s's log lines: %v", service, err)
	}
}

// waitForRows blocks until the sink has written n rows or gives up.
//
// The sink is asynchronous on purpose - the database is never in the
// request path - so a test that read immediately would be testing its
// own timing.
func waitForRows(t *testing.T, s *Sink, n uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if written, _, failed := s.Counters(); written >= n {
			return
		} else if failed > 0 {
			t.Fatalf("the sink reported %d failed writes", failed)
		}
		time.Sleep(20 * time.Millisecond)
	}
	written, dropped, failed := s.Counters()
	t.Fatalf("waited for %d rows; written=%d dropped=%d failed=%d", n, written, dropped, failed)
}

// TestAHostileLineIsWrittenAndDoesNotBreakTheWriter.
//
// Four shapes, each of which has broken a logger somewhere: a NUL byte,
// invalid UTF-8, a megabyte on one line, and a newline that would split
// one record into two - the last being log injection, where an attacker
// forges entries in the record an operator reads to find out what an
// attacker did.
func TestAHostileLineIsWrittenAndDoesNotBreakTheWriter(t *testing.T) {
	s, read := newTestSink(t, testdb.Collector)
	logger := slog.New(s.Handler())

	cases := map[string]string{
		"NUL":             "önce\x00sonra",
		"geçersiz UTF-8":  "önce\xff\xfe sonra",
		"1 MB tek satır":  strings.Repeat("A", 1<<20),
		"satır bölme":     "gerçek satır\nsahte satır: kullanıcı silindi",
		"kontrol dizisi":  "\x1b[31mkırmızı\x1b[0m",
		"unicode ayırıcı": "önce sonra",
	}
	for name, hostile := range cases {
		logger.Warn(hostile, "hostile_attr", hostile)
		t.Logf("gönderildi: %s", name)
	}

	waitForRows(t, s, uint64(len(cases)))

	// Every row landed, and none of them carries what was sent.
	rows, err := read.Query(context.Background(),
		`SELECT message, attrs::text FROM panel_logs WHERE service = $1`, testdb.Collector)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	seen := 0
	for rows.Next() {
		var message, attrs string
		if err := rows.Scan(&message, &attrs); err != nil {
			t.Fatal(err)
		}
		seen++
		for _, forbidden := range []struct{ what, s string }{
			{"a NUL byte", "\x00"},
			{"a newline", "\n"},
			{"an escape", "\x1b"},
			{"a unicode line separator", " "},
		} {
			if strings.Contains(message, forbidden.s) {
				t.Errorf("the stored message still contains %s", forbidden.what)
			}
			if strings.Contains(attrs, forbidden.s) {
				t.Errorf("a stored attribute still contains %s", forbidden.what)
			}
		}
		// The megabyte line has to have been cut down. A log table that
		// accepts a megabyte per line is the disk-full failure by a
		// second road.
		if len(message) > 64<<10 {
			t.Errorf("a stored message is %d bytes; nothing capped it", len(message))
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != len(cases) {
		t.Errorf("%d rows landed, sent %d", seen, len(cases))
	}
}

// TestTheSinkDropsRatherThanBlocks.
//
// The rule the whole package is shaped around: a service under load,
// producing more log lines than usual, is the service you least want to
// block. What must not happen is dropping in silence.
func TestTheSinkDropsRatherThanBlocks(t *testing.T) {
	write := testdb.Pool(t, testdb.Collector)
	level := &slog.LevelVar{}
	level.Set(slog.LevelDebug)

	// A buffer of one, and nothing draining it fast enough.
	s := New(write, Config{Service: testdb.Collector, Level: level, Buffer: 1})
	t.Cleanup(func() {
		s.Close()
		clearLines(t, testdb.Collector)
	})
	logger := slog.New(s.Handler())

	// Far more than the buffer, as fast as the caller can go. If Handle
	// ever waited, this would take as long as 5000 database inserts.
	start := time.Now()
	for i := 0; i < 5000; i++ {
		logger.Warn(fmt.Sprintf("satır %d", i))
	}
	elapsed := time.Since(start)

	// 500ms, not the 2s this started at. Measured both ways: dropping
	// takes ~3ms and blocking took 1.88s, so a 2s ceiling sat just above
	// the failure it was meant to catch - on a faster database a
	// blocking sink would have slipped under it, leaving only the
	// dropped counter below to notice. 500ms is still 150x the measured
	// cost of the correct behaviour.
	if elapsed > 500*time.Millisecond {
		t.Errorf("5000 log calls took %s; the sink is waiting on the database", elapsed)
	}

	_, dropped, _ := s.Counters()
	if dropped == 0 {
		t.Error("nothing was dropped with a buffer of 1 and 5000 records; is the counter wired?")
	}
	t.Logf("5000 satır %s sürdü, %d düşürüldü", elapsed, dropped)
}

// TestLinesBelowTheLevelNeverReachTheDatabase.
//
// The first of the three things that keep this table small, and the one
// a caller can undo by accident.
func TestLinesBelowTheLevelNeverReachTheDatabase(t *testing.T) {
	write := testdb.Pool(t, testdb.Collector)
	read := testdb.Pool(t, testdb.Panel)
	level := &slog.LevelVar{}
	level.Set(slog.LevelWarn) // the default this deployment ships with

	s := New(write, Config{Service: testdb.Collector, Level: level, Buffer: 64})
	t.Cleanup(func() {
		s.Close()
		clearLines(t, testdb.Collector)
	})
	logger := slog.New(s.Handler())

	logger.Debug("bu görünmemeli")
	logger.Info("bu da görünmemeli")
	logger.Warn("bu görünmeli")
	waitForRows(t, s, 1)

	var count int
	if err := read.QueryRow(context.Background(),
		`SELECT count(*) FROM panel_logs WHERE service = $1`, testdb.Collector).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("%d rows reached the table; only the WARN should have", count)
	}

	// And raising the level mid-flight lets debug through, which is what
	// the per-site verbose switch does.
	level.Set(slog.LevelDebug)
	logger.Debug("ayrıntılı kayıt açıkken bu görünmeli")
	waitForRows(t, s, 2)
}

// TestTheOperationIdAndSiteBecomeColumns.
//
// The coupling that made B1 and B2 one phase. If these stayed inside the
// attribute blob, the panel's streaming window would have to filter in
// Go over every line the system produced.
func TestTheOperationIdAndSiteBecomeColumns(t *testing.T) {
	s, read := newTestSink(t, testdb.Collector)
	logger := slog.New(s.Handler())

	logger.Warn("bir operasyonun satırı",
		OperationKey, "op-1234",
		SiteKey, "bir-site",
		"baska", "deger")
	waitForRows(t, s, 1)

	var op, site, attrs string
	if err := read.QueryRow(context.Background(),
		`SELECT operation_id, site_id, attrs::text FROM panel_logs WHERE service = $1`, testdb.Collector).
		Scan(&op, &site, &attrs); err != nil {
		t.Fatal(err)
	}
	if op != "op-1234" {
		t.Errorf("operation_id = %q", op)
	}
	if site != "bir-site" {
		t.Errorf("site_id = %q", site)
	}
	// And they are not duplicated into the blob, which would let the two
	// disagree after an edit.
	if strings.Contains(attrs, "op-1234") || strings.Contains(attrs, "bir-site") {
		t.Errorf("the columns are also inside attrs: %s", attrs)
	}
	if !strings.Contains(attrs, "baska") {
		t.Errorf("an ordinary attribute did not reach attrs: %s", attrs)
	}
}

// TestOneServiceCannotWriteALineUnderAnothersName.
//
// The one place a forgery pays. An operator reading this table to find
// out what a compromised beacon did is reading rows the beacon could
// have written - so the beacon must not be able to sign them
// `collector`.
//
// Written as raw SQL rather than through the sink, deliberately: the
// sink always stamps its own configured service, so putting it in the
// way would test the Go struct and not the policy. The threat is a
// process an attacker already controls, and such a process issues
// whatever SQL it likes on the connection it holds.
func TestOneServiceCannotWriteALineUnderAnothersName(t *testing.T) {
	beacon := testdb.Pool(t, testdb.Beacon)
	t.Cleanup(func() { clearLines(t, testdb.Collector) })

	_, err := beacon.Exec(context.Background(), `
		INSERT INTO panel_logs (service, level, message)
		VALUES ($1, 'WARN', 'toplayıcı adına sahte satır')`, testdb.Collector)
	if err == nil {
		t.Fatal("the beacon wrote a line signed `collector`; the record an operator reads to find out what happened can be forged by the thing they are investigating")
	}
	if !strings.Contains(err.Error(), "row-level security") {
		t.Errorf("the write was refused, but not by the policy: %v", err)
	}

	// And its own name still works - a policy that refused everything
	// would pass the assertion above while breaking logging outright.
	if _, err := beacon.Exec(context.Background(), `
		INSERT INTO panel_logs (service, level, message)
		VALUES ($1, 'WARN', 'kendi adına gerçek satır')`, testdb.Beacon); err != nil {
		t.Fatalf("the beacon cannot write its own log line either: %v", err)
	}
	clearLines(t, testdb.Beacon)
}

// TestTheSweepRemovesEveryServicesLinesAndNotOnlyItsOwn.
//
// panel_logs_sweep is a second policy on a table that already had a
// permissive one, which reads like belt and braces until it is measured:
// with only the write policy in place, a sweep run as panel_user removes
// its own rows and silently leaves the collector's. That is the shape of
// a retention job that looks like it works and does not - and the table
// it fails to trim is the one whose growth is the disk-full failure this
// package's schema is written around.
func TestTheSweepRemovesEveryServicesLinesAndNotOnlyItsOwn(t *testing.T) {
	admin := testdb.Admin(t)
	panelPool := testdb.Pool(t, testdb.Panel)
	ctx := context.Background()

	t.Cleanup(func() {
		clearLines(t, testdb.Collector)
		clearLines(t, testdb.Panel)
	})

	// One row from a service that cannot delete, one from the sweeper
	// itself. The sweeper's own row is the control: if it vanishes and
	// the collector's does not, the delete ran and the policy is what
	// stopped it.
	for _, role := range []string{testdb.Collector, testdb.Panel} {
		if _, err := admin.Exec(ctx, `
			INSERT INTO panel_logs (service, level, message)
			VALUES ($1, 'WARN', 'süpürülecek satır')`, role); err != nil {
			t.Fatalf("seeding a row for %s: %v", role, err)
		}
	}

	// Scoped by time rather than by service, which is what the retention
	// sweep itself does, and which leaves anything written later alone.
	cutoff := time.Now()
	tag, err := panelPool.Exec(ctx, `DELETE FROM panel_logs WHERE at <= $1`, cutoff)
	if err != nil {
		t.Fatalf("the sweep failed outright: %v", err)
	}

	var left []string
	rows, err := panelPool.Query(ctx,
		`SELECT service FROM panel_logs WHERE at <= $1 ORDER BY service`, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var service string
		if err := rows.Scan(&service); err != nil {
			t.Fatal(err)
		}
		left = append(left, service)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	if len(left) != 0 {
		t.Errorf("the sweep deleted %d rows and left %v behind; a retention job that trims only its own writer's lines does not bound this table",
			tag.RowsAffected(), left)
	}
}

// TestAnInfoLineReachesTheTreeAndNotTheTable.
//
// The composition, not either guard inside it.
//
// Until this change the tree and the sink shared one LevelVar, so they
// were never at different levels and none of this could go wrong. Now
// the sink floors at WARN while the tree may sit at INFO, and two
// separate things keep that true: logging.Tee asks each child whether it
// wants the record, and this package's Handle re-checks its own level.
//
// Either one alone is enough, which is why removing just one changes no
// behaviour and no test can catch it. What must not happen is losing
// both - so the assertion is on the property they exist for, measured
// against the real table through the real Attach path. An INFO line in
// panel_logs is the disk-full failure this package's schema is written
// around.
func TestAnInfoLineReachesTheTreeAndNotTheTable(t *testing.T) {
	write := testdb.Pool(t, testdb.Collector)
	read := testdb.Pool(t, testdb.Panel)
	t.Cleanup(func() { clearLines(t, testdb.Collector) })

	// A tree configured at INFO - chattier than the sink's floor, which
	// is the whole point.
	tree, controls, closeLogs, err := logging.Setup("test-composition", logging.Config{Level: "info"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeLogs)

	logger, sink := Attach(tree, write, controls)
	t.Cleanup(sink.Close)

	logger.Info("bu ağaca gitmeli, tabloya değil")
	logger.Warn("bu ikisine de gitmeli")
	waitForRows(t, sink, 1)

	rows, err := read.Query(context.Background(),
		`SELECT level, message FROM panel_logs WHERE service = $1`, testdb.Collector)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var got []string
	for rows.Next() {
		var level, message string
		if err := rows.Scan(&level, &message); err != nil {
			t.Fatal(err)
		}
		got = append(got, level)
		if level == "INFO" {
			t.Errorf("an INFO line reached panel_logs: %q. "+
				"The tree keeps INFO on disk where it is cheap; this table shares "+
				"a database with the customer's analytics, and a log table that "+
				"accepts everything becomes the largest one in it", message)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Errorf("%d rows landed (%v), want exactly the WARN", len(got), got)
	}
}
