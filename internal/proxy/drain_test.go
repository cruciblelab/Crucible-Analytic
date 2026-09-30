package proxy

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/limiter"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// The half of the shutdown that holds on every platform: whatever the
// kernel can or cannot say about idleness, nothing is waited for past
// the deadline.

// silentBackend accepts connections, reads what arrives and never
// answers - a request that never finishes - and counts the connections.
func silentBackend(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var accepted atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() {
				defer c.Close()
				buf := make([]byte, 64)
				for {
					if _, err := c.Read(buf); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String(), &accepted
}

// A request that never finishes does not hold the shutdown: at the
// deadline its connection is closed and Serve returns.
func TestABusyConnectionIsCutAtTheDeadline(t *testing.T) {
	backend, _ := silentBackend(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	store := ratestore.NewMemoryRateStore(time.Minute, time.Minute, time.Minute)
	defer store.Close()
	logs := &records{}
	const deadline = 800 * time.Millisecond
	srv := &Server{BackendAddr: backend, Store: store, Logger: slog.New(logs),
		HandshakeTimeout: 100 * time.Millisecond, drainIdle: time.Hour, drainTimeout: deadline}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("bekle\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // spliced, and the backend is not answering

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(deadline + 3*time.Second):
		t.Fatalf("Serve had not returned %s after a shutdown whose deadline was %s",
			deadline+3*time.Second, deadline)
	}
	if took := time.Since(start); took < deadline-50*time.Millisecond {
		// DrainIdle is an hour here, so nothing may count as idle: a
		// return before the deadline cut a busy connection early.
		t.Errorf("Serve returned after %s, before the %s deadline, with a busy connection open", took, deadline)
	}
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Error("the busy connection is still open after Serve returned")
	}
	// A request cut is said at WARN: "why did my upload fail at 03:12"
	// is answered in the log or nowhere.
	if !logs.has("WARN proxy: the shutdown deadline closed connections still in use") {
		t.Errorf("the deadline cut a connection in use and nothing said so at WARN; logged: %v", logs.msgs)
	}
}

// A connection still waiting for a limiter slot when the deadline comes
// is refused, not served by a process that is leaving: it never reaches
// the backend.
func TestAConnectionQueuedForASlotIsNotServedAfterTheDeadline(t *testing.T) {
	backend, accepted := silentBackend(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	store := &countingStore{}
	lim := limiter.New(limiter.Config{MaxConcurrentConnections: 1, MaxRequestsPerSecond: 1000,
		Policy: limiter.PolicyThrottle, ThrottleQueueSize: 4})
	const deadline = 600 * time.Millisecond
	srv := &Server{BackendAddr: backend, Store: store, Limiter: lim,
		HandshakeTimeout: 100 * time.Millisecond, drainIdle: time.Hour, drainTimeout: deadline}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	// The first takes the one slot and holds it with a request the
	// backend never answers; the second queues behind it.
	first, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	first.Write([]byte("bir\n"))
	waitFor(t, func() bool { return accepted.Load() == 1 }, "the first connection to reach the backend")
	second, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.Write([]byte("iki\n"))
	time.Sleep(200 * time.Millisecond) // queued

	cancel()
	select {
	case <-done:
	case <-time.After(deadline + 3*time.Second):
		t.Fatal("Serve did not return")
	}
	// Given the chance, a released queue would have dialled for the
	// second connection once the first was closed at the deadline.
	time.Sleep(300 * time.Millisecond)
	if n := accepted.Load(); n != 1 {
		t.Errorf("the backend accepted %d connections; the one still queued at the deadline was "+
			"served by a process that was shutting down", n)
	}
	// And the queue itself let go - not only the dial after it. A
	// connection admitted after the deadline is recorded as a visit before
	// it dials, so the count separates the two mechanisms: a queue that
	// kept waiting would admit the second connection once the first
	// freed its slot, and the cancelled dial would hide that from the
	// backend's count above.
	if n := store.recorded.Load(); n != 1 {
		t.Errorf("%d connections were admitted and recorded; the one queued when the deadline "+
			"came should have been refused, not admitted", n)
	}
}

// countingStore is a rate store that counts what it is given.
type countingStore struct {
	recorded atomic.Int64
	block    chan struct{} // nil: never blocks
}

func (c *countingStore) RecordRequest(ip netip.Addr, ja4 string, now time.Time) ratestore.WindowStats {
	c.recorded.Add(1)
	if c.block != nil {
		<-c.block
	}
	return ratestore.WindowStats{}
}

func (c *countingStore) Snapshot(since, now time.Time) []ratestore.Snapshot { return nil }

// records is a slog handler that keeps what it is given.
type records struct {
	mu   sync.Mutex
	msgs []string
}

func (r *records) Enabled(context.Context, slog.Level) bool { return true }
func (r *records) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, rec.Level.String()+" "+rec.Message)
	return nil
}
func (r *records) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *records) WithGroup(string) slog.Handler      { return r }
func (r *records) has(prefix string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.msgs {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return false
}

// A connection whose goroutine will not return even after everything is
// closed - here a rate store that never answers - does not hold the
// shutdown forever: Serve gives it drainGrace, says so at ERROR, and
// returns. The backstop for an edit that adds a wait nothing reaches.
func TestAStuckConnectionDoesNotHoldTheShutdownForever(t *testing.T) {
	backend, _ := silentBackend(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	store := &countingStore{block: make(chan struct{})}
	defer close(store.block)
	logs := &records{}
	const deadline = 300 * time.Millisecond
	srv := &Server{BackendAddr: backend, Store: store, Logger: slog.New(logs),
		HandshakeTimeout: 50 * time.Millisecond, drainIdle: time.Hour, drainTimeout: deadline}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("takil\n"))
	waitFor(t, func() bool { return store.recorded.Load() == 1 }, "the connection to reach the store")

	cancel()
	select {
	case <-done:
	case <-time.After(deadline + drainGrace + 3*time.Second):
		t.Fatal("Serve did not return: a stuck connection held the shutdown")
	}
	if !logs.has("ERROR proxy: connections were closed at the shutdown deadline and some did not finish") {
		t.Errorf("Serve returned without saying a connection was left behind; logged: %v", logs.msgs)
	}
}

// The decision, apart from any socket: every case the kernel can
// describe, and the side each one falls on.
func TestTheIdleDecision(t *testing.T) {
	const quiet = 2 * time.Second
	long, short := 5*time.Second, 500*time.Millisecond
	cases := []struct {
		name string
		v    view
		want bool
	}{
		{"never spoke, long enough", view{toClient: long, fromClient: long}, true},
		{"never spoke, not yet", view{toClient: short, fromClient: short}, false},
		// The measured defect: a client that writes as it connects has
		// both timers at one value, and nothing has gone back.
		{"the client wrote as it connected, nothing back yet",
			view{toClient: long, fromClient: long, received: true}, false},
		{"the client spoke, nothing back, a long wait",
			view{toClient: 10 * long, fromClient: long, received: true}, false},
		{"answered, quiet since", view{toClient: long, fromClient: 2 * long, sent: true, received: true}, true},
		{"answered, not quiet yet", view{toClient: short, fromClient: long, sent: true, received: true}, false},
		{"the client spoke after the answer",
			view{toClient: 2 * long, fromClient: long, sent: true, received: true}, false},
		// A request and its answer inside one millisecond - a backend on
		// the same machine - is the common reading of a tie.
		{"answered inside the millisecond, quiet since", view{toClient: long, fromClient: long, sent: true, received: true}, true},
		{"answered inside the millisecond, not quiet yet", view{toClient: short, fromClient: short, sent: true, received: true}, false},
		{"answered and quiet, but the answer is still leaving",
			view{toClient: long, fromClient: 2 * long, sent: true, received: true, inFlight: true}, false},
		{"exactly the quiet", view{toClient: quiet, fromClient: 2 * quiet, sent: true, received: true}, true},
		{"a millisecond short of it",
			view{toClient: quiet - time.Millisecond, fromClient: 2 * quiet, sent: true, received: true}, false},
	}
	for _, c := range cases {
		if got := c.v.idle(quiet); got != c.want {
			t.Errorf("%s: idle = %v, want %v", c.name, got, c.want)
		}
	}
}

// The numbers themselves, written out: a test that took them from the
// constants would move with them.
func TestTheDrainPromiseIsTwoSecondsOfQuietAndTenOfPatience(t *testing.T) {
	if DrainIdle != 2*time.Second {
		t.Errorf("DrainIdle is %s; the design says 2s - long enough that a backend pausing "+
			"mid-response is rarely taken for idle, short enough that a restart refuses new "+
			"visitors for about that long", DrainIdle)
	}
	if DrainTimeout != 10*time.Second {
		t.Errorf("DrainTimeout is %s; the design says 10s, fullproxy's", DrainTimeout)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
