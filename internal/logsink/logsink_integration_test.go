//go:build integration

// A log line is text somebody else chose.
//
// The done criterion this file exists for, in the plan's words: *düşmanca
// dize (NUL, geçersiz UTF-8, 1 MB tek satır) yazıcıyı bozmuyor.* Every
// case below is something a visitor can put in a URL, a header or a user
// agent, which is how it reaches a log line in the first place.
package logsink

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func sinkPool(t *testing.T, role string) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CA_LOGSINK_DSN_" + strings.ToUpper(role))
	if dsn == "" {
		dsn = os.Getenv("CA_SUPERUSER_DSN")
	}
	if dsn == "" {
		t.Skip("set CA_SUPERUSER_DSN")
	}
	p, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// newTestSink returns a sink writing as `service`, and cleans up after.
func newTestSink(t *testing.T, service string) (*Sink, *pgxpool.Pool) {
	t.Helper()
	pool := sinkPool(t, service)

	level := &slog.LevelVar{}
	level.Set(slog.LevelDebug) // so the tests choose what is filtered, not the default
	s := New(pool, Config{Service: service, Level: level, Buffer: 64})
	t.Cleanup(func() {
		s.Close()
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM panel_logs WHERE service = $1`, service)
	})
	return s, pool
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
	s, pool := newTestSink(t, "postgres")
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
	rows, err := pool.Query(context.Background(),
		`SELECT message, attrs::text FROM panel_logs WHERE service = 'postgres'`)
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
	pool := sinkPool(t, "postgres")
	level := &slog.LevelVar{}
	level.Set(slog.LevelDebug)

	// A buffer of one, and nothing draining it fast enough.
	s := New(pool, Config{Service: "postgres", Level: level, Buffer: 1})
	t.Cleanup(func() {
		s.Close()
		_, _ = pool.Exec(context.Background(), `DELETE FROM panel_logs WHERE service = 'postgres'`)
	})
	logger := slog.New(s.Handler())

	// Far more than the buffer, as fast as the caller can go. If Handle
	// ever waited, this would take as long as 5000 database inserts.
	start := time.Now()
	for i := 0; i < 5000; i++ {
		logger.Warn(fmt.Sprintf("satır %d", i))
	}
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
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
	pool := sinkPool(t, "postgres")
	level := &slog.LevelVar{}
	level.Set(slog.LevelWarn) // the default this deployment ships with

	s := New(pool, Config{Service: "postgres", Level: level, Buffer: 64})
	t.Cleanup(func() {
		s.Close()
		_, _ = pool.Exec(context.Background(), `DELETE FROM panel_logs WHERE service = 'postgres'`)
	})
	logger := slog.New(s.Handler())

	logger.Debug("bu görünmemeli")
	logger.Info("bu da görünmemeli")
	logger.Warn("bu görünmeli")
	waitForRows(t, s, 1)

	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM panel_logs WHERE service = 'postgres'`).Scan(&count); err != nil {
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
	s, pool := newTestSink(t, "postgres")
	logger := slog.New(s.Handler())

	logger.Warn("bir operasyonun satırı",
		OperationKey, "op-1234",
		SiteKey, "bir-site",
		"baska", "deger")
	waitForRows(t, s, 1)

	var op, site, attrs string
	if err := pool.QueryRow(context.Background(),
		`SELECT operation_id, site_id, attrs::text FROM panel_logs WHERE service = 'postgres'`).
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
