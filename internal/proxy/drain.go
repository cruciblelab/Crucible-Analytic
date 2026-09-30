package proxy

import (
	"context"
	"net"
	"sync"
	"time"
)

// Z7: how a shutdown ends the connections it finds open.
//
// # What this replaced
//
// Serve closed the listener and then waited for every open connection to
// finish by itself, with no deadline. A connection a browser keeps open
// after loading a page finishes when the backend's keep-alive timer says
// so - 75 seconds for nginx's default, never for a Go http.Server that
// sets no IdleTimeout - and for all of that time the listener was closed:
// the customer's site refused every new visitor. Under systemd the wait
// ended at the unit's stop timeout, 90 seconds, with a SIGKILL. Measured
// on the real binary: NOTES, Z7.
//
// # The promise, and why it is fullproxy's
//
// Full mode hands the same job to http.Server.Shutdown with ten seconds:
// idle connections are closed at once, busy ones are waited for up to the
// deadline, and whatever is left is left to the process exit. Passthrough
// now makes the same promise, so the two modes of one product restart the
// same way - and a test holds DrainTimeout to fullproxy.ShutdownGrace.
//
// # Idle, without reading a byte
//
// http.Server knows a connection is idle because it parses HTTP. This
// proxy never decrypts, so it asks the kernel instead (kernelView):
// TCP_INFO says, for the client's socket, how long ago data last arrived
// from the client and how long ago data last went to it, whether any data
// has gone each way at all, and whether a response is still leaving. A
// connection whose last data went *to* the client, and which has carried
// nothing for DrainIdle, is an answer delivered and a client that has not
// asked for anything since - the shape of a keep-alive connection between
// requests. view.idle holds the rule and its cases.
//
// Asked of the kernel rather than stamped by wrapping the connections,
// and that is a measured choice rather than a shortcut: the backend-to-
// client copy is io.Copy between two *net.TCPConn, which Go turns into
// splice(2) - response bytes never enter this process. A wrapper that
// recorded each Read would put every one of them back through userspace
// to learn something the kernel already keeps.
//
// # What it cannot see
//
// Requests, because they are inside TLS. It sees who sent data last, and
// that stands in for "is a request waiting for its answer" except in two
// shapes, both written down so nobody has to rediscover them:
//
//   - Several requests on one connection - HTTP/2 does this all the time -
//     where a quick answer is the last data out and a slow request is
//     still with the backend. After DrainIdle of silence it looks idle,
//     and is closed. drain_linux_test.go holds the shape as a test.
//   - A new request that arrives inside the kernel's millisecond with the
//     previous answer, and then takes longer than DrainIdle: the tie is
//     read as idle (view.idle says why).
//
// The cost of either is one request cut during a restart. The cost of
// the rule it replaced was every new visitor refused for as long as any
// connection stayed open.
//
// Where the kernel cannot be asked (a platform without TCP_INFO, or a
// connection that is not TCP), a connection counts as busy and the
// deadline decides. The product runs on Linux; the others build.

// DrainIdle is how long a connection must have carried nothing, after
// data last went to the client, for a shutdown to close it at once.
const DrainIdle = 2 * time.Second

// DrainTimeout is the longest a shutdown waits for busy connections
// before closing them: fullproxy's ShutdownGrace.
const DrainTimeout = 10 * time.Second

// drainPoll is how often a draining server looks again for connections
// that have become idle.
const drainPoll = 100 * time.Millisecond

// drainGrace bounds the wait after everything has been closed, for the
// goroutines to notice. Closing both sockets and cancelling the dial and
// the admission wait ends every one of them; this is the backstop for a
// future edit that adds a wait none of those reaches, so that edit shows
// up as a log line rather than as a shutdown that never finishes.
const drainGrace = time.Second

// tracked is one accepted connection and, once it is dialled, the
// backend connection it is spliced to.
type tracked struct {
	client net.Conn

	mu      sync.Mutex
	backend net.Conn
	closed  bool
}

// attach records the backend connection. It reports false when a drain
// has already closed this connection, and then the backend connection
// is closed here: a dial that finished after the drain gave up on it
// must not start a splice nobody will end.
func (t *tracked) attach(backend net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		backend.Close()
		return false
	}
	t.backend = backend
	return true
}

// close ends both sides. Closing the client socket stops the copy that
// reads it; closing the backend stops the other, which would otherwise
// wait for a backend that ignores the half-close.
func (t *tracked) close() {
	t.mu.Lock()
	t.closed = true
	backend := t.backend
	t.mu.Unlock()
	t.client.Close()
	if backend != nil {
		backend.Close()
	}
}

// idle reports whether the kernel says this connection is between
// requests; see the comment at the top of this file.
func (t *tracked) idle(quiet time.Duration) bool {
	v, ok := kernelView(t.client)
	return ok && v.idle(quiet)
}

// view is what the kernel says about one client socket.
type view struct {
	// toClient and fromClient are how long ago data last went to the
	// client and last came from it. Before any data, both count from the
	// connection's start.
	toClient, fromClient time.Duration
	// sent and received are whether any data has ever gone each way.
	sent, received bool
	// inFlight is data to the client that has not been sent yet or not
	// been acknowledged: a response still on its way.
	inFlight bool
}

// idle is the decision, apart from the socket it is read from.
//
// # Why the byte counts and not the two timers alone
//
// The first version compared the timers and nothing else, and the
// kernel's millisecond cannot order two events inside one: a client that
// writes as it connects - which is what a TLS client does with its
// ClientHello - has both timers at the same value, and "equal" was read as
// "the backend spoke last". A request waiting for its answer was cut as
// idle. The byte counts say what the timers cannot: whether anything has
// gone to the client at all.
func (v view) idle(quiet time.Duration) bool {
	switch {
	case v.inFlight:
		// Still delivering a response.
		return false
	case !v.sent && !v.received:
		// Never spoke: a speculative connect, or a client that went
		// away without closing.
		return v.toClient >= quiet && v.fromClient >= quiet
	case !v.sent:
		// The client spoke and nothing has come back yet.
		return false
	case v.fromClient < v.toClient:
		// The client spoke last: a request not answered yet.
		return false
	default:
		// Data went to the client after the client last sent any - an
		// answer delivered - or the two fell inside one of the kernel's
		// milliseconds. Idle once it has been quiet long enough.
		//
		// The tie counts as idle, and that was measured rather than
		// chosen: the second version counted it as busy, and a backend
		// on the same machine answers inside the millisecond its request
		// arrived - a cached file, a 304 - so the connection stayed tied,
		// and busy, until the deadline. The reading a tie gets wrong is a
		// new request arriving inside the millisecond the previous answer
		// left, and then taking longer than DrainIdle.
		return v.toClient >= quiet
	}
}

// connSet is every connection a Serve call has accepted and not finished.
type connSet struct {
	mu    sync.Mutex
	conns map[*tracked]struct{}
}

func newConnSet() *connSet { return &connSet{conns: map[*tracked]struct{}{}} }

func (s *connSet) add(c net.Conn) *tracked {
	t := &tracked{client: c}
	s.mu.Lock()
	s.conns[t] = struct{}{}
	s.mu.Unlock()
	return t
}

func (s *connSet) remove(t *tracked) {
	s.mu.Lock()
	delete(s.conns, t)
	s.mu.Unlock()
}

// snapshot copies the set, so closing happens outside the lock: a close
// that blocked would otherwise hold up every connection's remove.
func (s *connSet) snapshot() []*tracked {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*tracked, 0, len(s.conns))
	for t := range s.conns {
		out = append(out, t)
	}
	return out
}

// drainReport is what a shutdown did, for its log line.
type drainReport struct {
	open, idle, cut int
	took            time.Duration
	stuck           bool
}

// drain closes idle connections as they become idle, waits for the busy
// ones up to timeout, then closes whatever is left.
//
// done is closed when every connection's goroutine has returned; force
// cancels the admission wait and the backend dial of connections that
// have not got that far.
func drain(live *connSet, done <-chan struct{}, force context.CancelFunc,
	quiet, timeout time.Duration) drainReport {

	start := time.Now()
	r := drainReport{open: len(live.snapshot())}
	closedIdle := map[*tracked]bool{}

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	poll := time.NewTicker(drainPoll)
	defer poll.Stop()
	for {
		for _, t := range live.snapshot() {
			if !closedIdle[t] && t.idle(quiet) {
				closedIdle[t] = true
				t.close()
			}
		}
		select {
		case <-done:
			r.idle, r.took = len(closedIdle), time.Since(start)
			return r
		case <-deadline.C:
			for _, t := range live.snapshot() {
				if !closedIdle[t] {
					r.cut++
				}
				t.close()
			}
			force()
			select {
			case <-done:
			case <-time.After(drainGrace):
				r.stuck = true
			}
			r.idle, r.took = len(closedIdle), time.Since(start)
			return r
		case <-poll.C:
		}
	}
}
