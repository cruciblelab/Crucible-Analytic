//go:build integration

package beacon

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A write that has started finishes, even if the process was asked to
// stop while it was going out.
//
// # Where this came from
//
// CI run 357 failed on TestTheVisitorSwitchInARealBrowser: 3 rows
// written, 4 expected, with one line above it -
//
//	beacon: write failed, batch dropped err="beacon: copy rows: context canceled" rows=1
//
// and the identical commit passed on main in run 358. Same tree, two
// results: a race, not a wrong answer. But the race is not in the test.
//
// Writer.flush is called from three places. The drain in Run builds a
// fresh context because "ctx is already cancelled, so reusing it would
// abort the very write this drain exists to perform" - that comment is
// right. The other two call sites, the batch-is-full path and the
// ticker, passed ctx itself. The Run goroutine is either in select or
// inside a COPY, so a cancel that lands during the COPY killed it: up
// to batchSize events discarded, on every clean stop.
//
// Run's own doc comment said "so a clean shutdown loses nothing". It was
// true of the rows still in the buffer and false of the batch already
// leaving, and nothing measured the difference. This test is that
// criterion turned into a test.
//
// # Why it is deterministic where the browser test was not
//
// It races nothing. The context is cancelled *first* and flush is called
// after - which is the state the losing interleaving reaches, reached on
// purpose. Measured before the fix: 0 of 2 rows, dropped counter 2.
func TestAWriteAlreadyGoingOutIsNotAbandonedOnShutdown(t *testing.T) {
	const site = "flush-cancel"
	w := newTestWriter(t, site, WriterConfig{})

	rows := []Row{
		{Time: time.Now().UTC(), SiteID: site, VisitorID: "v1", EventType: "pageview", Path: "/bir"},
		{Time: time.Now().UTC(), SiteID: site, VisitorID: "v1", EventType: "pageview", Path: "/iki"},
	}

	// The process is asked to stop, and *then* the batch goes out. This
	// is the interleaving the browser test hit by accident.
	stopping, cancel := context.WithCancel(context.Background())
	cancel()

	w.flush(stopping, rows)

	if got := countRows(t, site); got != len(rows) {
		_, dropped := w.Counters()
		t.Errorf("%d of %d rows reached the database after a flush that began with a "+
			"cancelled context (dropped counter: %d).\n"+
			"Run's contract is that a clean shutdown loses nothing. A cancel that "+
			"lands while a COPY is in flight must not abort it: those rows are "+
			"already out of the buffer, so nothing will send them again.",
			got, len(rows), dropped)
	}
}

// And detached is not unbounded: a database that has stopped answering
// does not hold shutdown open.
//
// # Why this test exists
//
// Because a mutation asked for it. Replacing the flush timeout with a
// plain cancellable context broke nothing: the test above only shows
// that a cancelled context no longer *aborts* the write, and a write
// with no deadline satisfies that just as well. The two halves of the
// rule are separate claims, and only one of them was measured.
//
// The half this one measures is a liveness claim, and it is the reason
// the timeout is in the code: `systemctl stop` gives a unit 90 seconds
// by default and then kills it. A drain that waits forever on a wedged
// database does not save the batch it is holding - it loses the batch
// *and* whatever the kill interrupts.
//
// # The rig
//
// A listener that accepts the connection and then says nothing, which is
// the shape that hangs: a refused connection fails fast and would prove
// nothing. The pool is built directly rather than through NewWriter,
// because NewWriter pings - reasonably - and would never hand back a
// writer pointed at a black hole.
//
// Three assertions, and the last two are what keep it from passing
// vacuously: the listener was actually reached, the wait ended inside
// the bound, and it did *not* end well inside it. A pool that failed
// fast for some unrelated reason would satisfy a ceiling alone while
// measuring nothing - so the floor is the one that says the deadline is
// what ended the wait. It costs the test ten seconds.
func TestAWedgedDatabaseDoesNotHoldShutdownOpen(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan struct{}, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			select {
			case accepted <- struct{}{}:
			default:
			}
			// Held open and never answered. Closed when the test ends.
			defer conn.Close()
		}
	}()

	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody:nothing@"+listener.Addr().String()+"/nothing?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	w := &Writer{pool: pool, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rows := []Row{{Time: time.Now().UTC(), SiteID: "wedged", EventType: "pageview"}}

	stopping, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	w.flush(stopping, rows)
	elapsed := time.Since(start)

	select {
	case <-accepted:
	default:
		t.Fatal("the black-hole listener was never reached, so this test measured " +
			"something other than a hanging database")
	}
	if elapsed > flushTimeout+5*time.Second {
		t.Errorf("flush took %v against a database that never answered; the bound is %v.\n"+
			"A drain with no ceiling turns a wedged database into a unit systemd has "+
			"to kill, which loses more than the batch it was holding.",
			elapsed.Round(time.Millisecond), flushTimeout)
	}
	if elapsed < flushTimeout/2 {
		t.Errorf("flush gave up after %v, well inside the %v bound.\n"+
			"Then something other than the deadline ended it, and this test is not "+
			"measuring the deadline.", elapsed.Round(time.Millisecond), flushTimeout)
	}
}
