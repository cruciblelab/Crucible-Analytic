package proxy

import (
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// panicStore panics on the Nth RecordRequest call. RecordRequest runs on
// the per-connection goroutine (server.go, in handleConn), so this
// reproduces the realistic shape of the danger: an ordinary bug - a nil
// map write, a slice bounds slip - in code the connection goroutine calls.
// It does not need to be an exotic bug to matter; it needs only to be a
// panic.
type panicStore struct {
	calls   atomic.Int64
	panicOn int64
}

func (p *panicStore) RecordRequest(ip netip.Addr, ja4 string, now time.Time) ratestore.WindowStats {
	if p.calls.Add(1) == p.panicOn {
		panic("simulated bug in the rate store")
	}
	return ratestore.WindowStats{}
}

func (p *panicStore) Snapshot(since, now time.Time) []ratestore.Snapshot { return nil }

// TestPanicInConnectionHandlerDoesNotKillTheProcess is the security
// property, not a unit test of a function.
//
// The collector is a proxy in front of the customer's website. Go tears
// down the entire process on an unrecovered panic in any goroutine, and
// the accept loop hands each connection to a bare goroutine. Without a
// recover on that path, one panicking connection does not lose one
// visitor - it stops the collector, and the collector stopping takes the
// customer's site down with it. Every other visitor's connection dies
// with it, and an attacker who found the input that triggers it can
// simply send it again after each restart.
//
// This is also an internal-consistency requirement. The limiter already
// has a fail_open mode whose entire purpose is that the collector must
// never become the reason the customer's site is unreachable. A
// process-killing panic contradicts that commitment, so the commitment is
// what this test holds the code to.
//
// net/http made the same call for the same reason: conn.serve recovers
// per connection rather than letting one handler take down every other
// in-flight request. The servers in internal/api, internal/beacon,
// internal/fullproxy and internal/panel/web inherit that protection by
// running on http.Server. internal/proxy does not run on http.Server, so
// it has to say so itself - that asymmetry is what this test closes.
//
// Before the recover existed, this test did not fail: it killed the test
// binary outright, and the whole package reported a panic trace. That is
// the exposure, demonstrated.
func TestPanicInConnectionHandlerDoesNotKillTheProcess(t *testing.T) {
	backend := newEchoBackend(t)

	store := &panicStore{panicOn: 1}
	srv := &Server{
		BackendAddr:      backend,
		Store:            store,
		HandshakeTimeout: 200 * time.Millisecond,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go srv.Serve(t.Context(), ln)

	// First connection: the store panics on it. If nothing recovers, the
	// process is gone here and nothing below ever runs.
	first, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial (first): %v", err)
	}
	first.Write([]byte("hello"))
	first.Close()

	// Second connection: the point of the test. The server must still be
	// accepting, and this connection must still reach the backend - the
	// blast radius of the panic is one connection, not the listener.
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := roundTrip(ln.Addr().String()); err == nil {
			return // served after the panic: the process survived and kept working
		} else if time.Now().After(deadline) {
			t.Fatalf("server stopped serving after a panic in a connection handler: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// panicOnCloseWriteConn panics from CloseWrite, which pipeConns calls on
// each splice goroutine once its direction finishes.
type panicOnCloseWriteConn struct {
	net.Conn
	panics atomic.Int64
}

func (c *panicOnCloseWriteConn) CloseWrite() error {
	c.panics.Add(1)
	panic("simulated bug on the splice path")
}

// TestPanicInSpliceGoroutineIsContainedAndDoesNotDeadlock covers the half
// of the danger that a recover in handleConn does not: pipeConns starts
// two goroutines of its own, and recover() only sees panics raised on the
// goroutine it runs on.
//
// It also holds pipeConns to the ordering claim in its comment. wg.Done is
// deferred before recoverConn, so it runs after it - a panic still
// decrements the counter. Get that backwards and the panic is contained
// but wg.Wait never returns, which trades a crash for a permanently stuck
// connection and a leaked goroutine. Hence the timeout: "returns at all"
// is as much the assertion as "did not crash".
func TestPanicInSpliceGoroutineIsContainedAndDoesNotDeadlock(t *testing.T) {
	backend := newEchoBackend(t)

	backendConn, err := net.Dial("tcp", backend)
	if err != nil {
		t.Fatalf("dial backend: %v", err)
	}
	defer backendConn.Close()

	clientSide, proxySide := net.Pipe()
	defer clientSide.Close()

	go func() {
		clientSide.Write([]byte("payload"))
		clientSide.Close()
	}()

	client := &panicOnCloseWriteConn{Conn: proxySide}
	done := make(chan struct{})
	go func() {
		defer close(done)
		pipeConns(slog.New(slog.DiscardHandler), client, proxySide, backendConn)
	}()

	select {
	case <-done:
		// Returned rather than crashing or hanging: both goroutines
		// recovered and both still called wg.Done.
	case <-time.After(5 * time.Second):
		t.Fatal("pipeConns never returned after a panic in a splice goroutine: wg.Done did not run, so wg.Wait is stuck forever")
	}

	// Without this the test passes for the wrong reason: if io.Copy never
	// finished, CloseWrite is never reached, nothing ever panics, and
	// "pipeConns returned" proves nothing about recovery at all.
	if n := client.panics.Load(); n == 0 {
		t.Fatal("CloseWrite was never called, so nothing panicked; this test would pass even with no recover at all")
	}
}

// roundTrip opens a connection, sends a payload and requires the echo
// backend to send it back, proving the proxy still splices end to end
// rather than merely still accepting sockets.
func roundTrip(addr string) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	const payload = "still-alive"
	if _, err := conn.Write([]byte(payload)); err != nil {
		return err
	}
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	got, err := io.ReadAll(conn)
	if err != nil {
		return err
	}
	if string(got) != payload {
		return &roundTripError{got: string(got)}
	}
	return nil
}

type roundTripError struct{ got string }

func (e *roundTripError) Error() string { return "backend echoed " + e.got }

// newEchoBackend starts a TCP server that echoes whatever it is sent, and
// returns its address.
func newEchoBackend(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("backend listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
				if cw, ok := conn.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
			}()
		}
	}()
	return ln.Addr().String()
}
