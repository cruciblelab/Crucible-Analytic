//go:build linux

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// The shutdown's idle test asks the kernel, so these run where the
// kernel can be asked. drain_test.go holds the half that holds anywhere.

// Short, so the suite does not spend DrainIdle's real two seconds; the
// defaults are pinned separately.
const (
	testQuiet   = 300 * time.Millisecond
	testTimeout = 4 * time.Second
)

// serving is a Server on a loopback listener and the channel its Serve
// returns on.
type serving struct {
	addr   string
	cancel context.CancelFunc
	done   chan error
}

// serveForDrain starts a proxy in front of backend with short drain
// times.
func serveForDrain(t *testing.T, backend string) *serving {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	store := ratestore.NewMemoryRateStore(time.Minute, time.Minute, time.Minute)
	t.Cleanup(store.Close)
	srv := &Server{
		BackendAddr:      backend,
		Store:            store,
		HandshakeTimeout: 200 * time.Millisecond,
		DialTimeout:      2 * time.Second,
		drainIdle:        testQuiet,
		drainTimeout:     testTimeout,
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &serving{addr: ln.Addr().String(), cancel: cancel, done: make(chan error, 1)}
	go func() { s.done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-s.done:
		case <-time.After(testTimeout + 5*time.Second):
			t.Error("Serve did not return after the test")
		}
	})
	return s
}

// stop cancels and returns how long Serve took to return, failing the
// test if it did not return within limit.
func (s *serving) stop(t *testing.T, limit time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	s.cancel()
	select {
	case err := <-s.done:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
		// Put it back for the cleanup above, which waits on it too.
		s.done <- nil
		return time.Since(start)
	case <-time.After(limit):
		t.Fatalf("Serve had not returned %s after shutdown began", limit)
		return 0
	}
}

// keepAliveBackend answers every line it reads after a delay, and never
// closes a connection first - Go's http.Server with no IdleTimeout, which
// is what held the collector open for as long as a client did.
func keepAliveBackend(t *testing.T, delay time.Duration) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				// A whole line, then the answer: the proxy may deliver a
				// line in two pieces (what it peeked, then the rest), and a
				// backend answering each piece after its delay would be a
				// response with a pause in the middle - the one thing the
				// drain is documented to misread.
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadBytes('\n')
					if err != nil {
						return
					}
					time.Sleep(delay)
					if _, err := c.Write(bytes.ToUpper(line)); err != nil {
						return
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// ask sends one line through the proxy and waits for the answer.
func ask(t *testing.T, c net.Conn, line string) {
	t.Helper()
	if _, err := c.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(line))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("reading the answer to %q: %v", line, err)
	}
	if string(got) != string(bytes.ToUpper([]byte(line))) {
		t.Fatalf("answer %q to %q", got, line)
	}
}

// closedByPeer reports whether the proxy has closed this client
// connection: a read returns EOF (or a reset) rather than timing out.
func closedByPeer(c net.Conn) bool {
	c.SetReadDeadline(time.Now().Add(time.Second))
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	if err == nil {
		return false
	}
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	return true
}

// A connection between requests is closed when the server stops, after
// DrainIdle of quiet - not held open until the client or the backend
// ends it.
//
// The measured case: one keep-alive connection, a backend that never
// closes an idle one. Before Z7 Serve did not return at all; the real
// binary was still waiting after 40 seconds.
func TestAnIdleConnectionIsClosedWhenTheServerStops(t *testing.T) {
	s := serveForDrain(t, keepAliveBackend(t, 0))
	c, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ask(t, c, "merhaba\n")

	took := s.stop(t, testTimeout)
	// Closed for being idle, which is quiet after the last answer - not
	// at the deadline, which is what a busy connection gets.
	if took >= testTimeout-500*time.Millisecond {
		t.Errorf("Serve took %s, the deadline; an idle connection should close after %s of quiet",
			took, testQuiet)
	}
	if !closedByPeer(c) {
		t.Error("the idle connection is still open after Serve returned")
	}
}

// A request the backend has not answered yet is waited for, and its
// answer reaches the client.
//
// The client spoke last, so the connection is busy however long the
// backend is quiet: the kernel's view that tells an idle connection from
// a busy one is who sent data last, not only how long ago.
func TestARequestInFlightIsAnsweredBeforeTheServerStops(t *testing.T) {
	const think = 1200 * time.Millisecond // longer than testQuiet, well under testTimeout
	s := serveForDrain(t, keepAliveBackend(t, think))
	c, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("siparis\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // the request is at the backend

	answered := make(chan error, 1)
	go func() {
		got := make([]byte, len("SIPARIS\n"))
		_, err := io.ReadFull(c, got)
		answered <- err
	}()
	took := s.stop(t, testTimeout+time.Second)
	if err := <-answered; err != nil {
		t.Fatalf("the answer did not arrive: %v (Serve returned after %s)", err, took)
	}
	if took < think-200*time.Millisecond {
		t.Errorf("Serve returned after %s, before the backend's %s answer could have been delivered",
			took, think)
	}
	if took >= testTimeout {
		t.Errorf("Serve waited %s, the whole deadline, for a connection that went idle after its answer", took)
	}
}

// A response still streaming is not idle, even though the backend is the
// one talking: data moved within DrainIdle.
func TestAStreamingResponseIsNotCutAsIdle(t *testing.T) {
	const chunks, every = 12, 100 * time.Millisecond // 1.2 s of streaming
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if _, err := c.Read(make([]byte, 16)); err != nil {
			return
		}
		for i := 0; i < chunks; i++ {
			if _, err := c.Write([]byte("x")); err != nil {
				return
			}
			time.Sleep(every)
		}
		// Then idle, and it never closes first: the drain has to judge it.
		io.Copy(io.Discard, c)
	}()

	s := serveForDrain(t, ln.Addr().String())
	c, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("indir\n")); err != nil {
		t.Fatal(err)
	}
	var got atomic.Int64
	go func() {
		buf := make([]byte, 16)
		for {
			n, err := c.Read(buf)
			got.Add(int64(n))
			if err != nil {
				return
			}
		}
	}()
	time.Sleep(250 * time.Millisecond) // mid-stream

	took := s.stop(t, testTimeout+time.Second)
	if n := got.Load(); n != chunks {
		t.Errorf("the client received %d of %d chunks; the stream was cut (Serve returned after %s)",
			n, chunks, took)
	}
}

// A connection that never sent a byte - a speculative connect, or a
// client that went away without closing - is idle: nothing went either
// way, for longer than DrainIdle.
func TestAConnectionThatNeverSpokeIsClosed(t *testing.T) {
	s := serveForDrain(t, keepAliveBackend(t, 0))
	c, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(500 * time.Millisecond) // past the handshake timeout; spliced, silent

	took := s.stop(t, testTimeout)
	if took >= testTimeout-500*time.Millisecond {
		t.Errorf("Serve took %s, the deadline, for a connection that never carried a byte", took)
	}
}

// The shape drain.go says it cannot see, held as a test so the comment
// cannot drift from the code: two requests on one connection, the first
// answered at once and the second slow. The quick answer is the last
// data out, the backend is silent while it works on the second, and after
// DrainIdle of silence the connection looks idle - so the slow answer is
// cut. HTTP/2 puts several requests on a connection all the time.
//
// If this starts failing because the second answer arrives, the drain
// has learned to see requests; say so in drain.go and turn this around.
func TestASlowRequestBehindAQuickAnswerIsCutAsDocumented(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	const slow = 2 * time.Second // well past testQuiet
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		if _, err := r.ReadBytes('\n'); err != nil { // "hizli"
			return
		}
		if _, err := r.ReadBytes('\n'); err != nil { // "yavas"
			return
		}
		c.Write([]byte("HIZLI\n"))
		time.Sleep(slow)
		c.Write([]byte("YAVAS\n"))
		io.Copy(io.Discard, c)
	}()

	s := serveForDrain(t, ln.Addr().String())
	c, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Both requests before either answer - pipelined, the way HTTP/2
	// multiplexes them.
	if _, err := c.Write([]byte("hizli\nyavas\n")); err != nil {
		t.Fatal(err)
	}
	quick := make([]byte, len("HIZLI\n"))
	if _, err := io.ReadFull(c, quick); err != nil {
		t.Fatal(err)
	}

	took := s.stop(t, testTimeout+time.Second)
	if took >= slow {
		t.Fatalf("Serve waited %s, past the slow answer at %s: the drain saw the second "+
			"request after all - update drain.go's 'What it cannot see' and this test", took, slow)
	}
	c.SetReadDeadline(time.Now().Add(slow + time.Second))
	rest, _ := io.ReadAll(c)
	if string(rest) == "YAVAS\n" {
		t.Fatal("the slow answer arrived; the documented limitation no longer holds")
	}
}

// fullBacklogListener is a backend whose accept queue is already full, so
// a new connection to it waits for a SYN-ACK that never comes: a dial in
// flight, on loopback, with no network behaviour to depend on. listen(2)
// with a backlog of zero lets one connection wait; this function puts one
// there.
func fullBacklogListener(t *testing.T) string {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fd) })
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Listen(fd, 0); err != nil {
		t.Fatal(err)
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		t.Fatal(err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(sa.(*syscall.SockaddrInet4).Port))
	first, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatalf("filling the accept queue: %v", err)
	}
	t.Cleanup(func() { first.Close() })
	// The premise, asked rather than assumed: the next connection waits.
	if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("a second connection to a full accept queue was accepted; this test's dial would not hang")
	}
	return addr
}

// A backend dial still in flight when the deadline comes is cancelled, so
// the shutdown ends without leaving a goroutine behind - and without the
// ERROR that says one was.
//
// The queue's cancellation has its own test; this one isolates the dial:
// the connection is past the limiter (there is none) and past the record,
// waiting only for the backend to answer a SYN.
func TestADialInFlightIsCancelledAtTheDeadline(t *testing.T) {
	backend := fullBacklogListener(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	store := &countingStore{}
	logs := &records{}
	const deadline = 300 * time.Millisecond
	srv := &Server{BackendAddr: backend, Store: store, Logger: slog.New(logs),
		HandshakeTimeout: 50 * time.Millisecond, DialTimeout: 30 * time.Second,
		drainIdle: time.Hour, drainTimeout: deadline}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("ara\n"))
	waitFor(t, func() bool { return store.recorded.Load() == 1 }, "the connection to reach the dial")
	time.Sleep(100 * time.Millisecond) // dialling

	cancel()
	select {
	case <-done:
	case <-time.After(deadline + drainGrace + 3*time.Second):
		t.Fatal("Serve did not return")
	}
	if logs.has("ERROR ") {
		t.Errorf("the shutdown left a goroutine behind; logged: %v", logs.msgs)
	}
}
