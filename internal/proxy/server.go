// Package proxy implements a minimal TCP/TLS passthrough proxy: it
// listens for connections, best-effort extracts a JA4 fingerprint from the
// TLS ClientHello without terminating TLS, records the request against a
// RateStore, and forwards every byte to the backend unmodified. Keeping
// setup as simple as "point it at your existing site" means it never
// decrypts, buffers, or rewrites application data.
package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/asnlookup"
	"github.com/cruciblelab/crucible-analytic/internal/limiter"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// Resolver resolves an IP to country/ASN info for GeoBlocklist checks.
// *asnlookup.Resolver implements this; a narrow interface (rather than a
// direct dependency on *asnlookup.Resolver) so tests can substitute a
// fake instead of needing real loaded range tables - the same reasoning
// behind storage.GeoResolver.
type Resolver interface {
	Resolve(ip netip.Addr) asnlookup.Result
}

// Server proxies TCP connections from ListenAddr to BackendAddr, recording
// a JA4-fingerprinted request per connection into Store. It never rejects
// or delays a connection because fingerprinting failed or timed out - the
// whole point is to observe, not gate. Two things *do* gate connections:
// Limiter (see internal/limiter), bounding the collector's own total
// resource usage independent of per-IP behavior, and GeoBlocklist,
// rejecting specific countries/ASNs independent of load.
type Server struct {
	ListenAddr  string
	BackendAddr string
	Store       ratestore.RateStore
	// Limiter bounds total concurrent connections/requests-per-second
	// across all IPs. Nil means unlimited (mainly for tests that don't
	// care about this dimension) - production wiring in cmd/collector
	// always sets a real one, even if its own limits are effectively
	// unbounded.
	Limiter *limiter.Limiter
	// GeoBlocklist and Resolver together gate connections by country/ASN,
	// checked before Limiter and independent of it - see geoBlocked. Both
	// nil (the default - asn_lookup disabled, or enabled with no
	// blocklist entries) skips the check entirely, including the
	// Resolve() call, so this costs nothing extra on the request path
	// unless blocking is actually configured.
	GeoBlocklist *limiter.GeoBlocklist
	Resolver     Resolver

	// HandshakeTimeout bounds how long sniffing waits to see a complete
	// ClientHello before giving up and proxying unfingerprinted. Zero
	// disables the deadline (not recommended outside tests).
	HandshakeTimeout time.Duration
	// DialTimeout bounds connecting to the backend. Defaults to 10s if <= 0.
	DialTimeout time.Duration

	Logger *slog.Logger

	// drainIdle and drainTimeout are the shutdown's two numbers (Z7,
	// drain.go); zero means DrainIdle and DrainTimeout. Unexported: the
	// only reason to change them is a test that cannot spend ten real
	// seconds per case, and a public field nothing in production sets
	// would be a setting nobody could reason about.
	drainIdle    time.Duration
	drainTimeout time.Duration
}

// ListenAndServe binds ListenAddr and serves until ctx is cancelled or a
// fatal listener error occurs.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.ListenAddr)
	if err != nil {
		return fmt.Errorf("proxy: listen on %s: %w", s.ListenAddr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve accepts connections on ln until ctx is cancelled or a fatal
// listener error occurs, returning nil on a clean, ctx-triggered shutdown.
// Split out from ListenAndServe so tests can serve on an ephemeral
// (":0") listener and still learn the bound address.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	// no-recover: this goroutine waits on a channel and closes a
	// listener. It never holds a connection and never touches a byte an
	// attacker sent, so there is nothing here for a recover to contain -
	// and one that pretended otherwise would be the sort of reassurance
	// this package's own recover.go warns about. Every other goroutine
	// in this package does recover; internal/invariants checks that, and
	// this line is the exemption it requires a reason for.
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	s.logger().Info("proxy listening", "addr", ln.Addr().String(), "backend", s.BackendAddr)

	// stopping ends the two waits a closed socket does not: a connection
	// queued for a limiter slot, and a backend dial in progress. Cancelled
	// at the drain's deadline and not before - a connection accepted
	// before the shutdown began is served if it can be within it.
	stopping, stop := context.WithCancel(context.Background())
	defer stop()
	live := newConnSet()

	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				done := make(chan struct{})
				// no-recover: this goroutine waits on a WaitGroup and
				// closes a channel. It holds no connection and touches no
				// byte an attacker sent, which is the same reason the
				// listener-closing goroutine above gives.
				go func() {
					wg.Wait()
					close(done)
				}()
				s.logDrain(drain(live, done, stop, orDefault(s.drainIdle, DrainIdle),
					orDefault(s.drainTimeout, DrainTimeout)))
				return nil
			}
			return err
		}
		t := live.add(conn)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer live.remove(t)
			s.handleConn(stopping, t)
		}()
	}
}

// logDrain says what a shutdown did with the connections it found open.
//
// Nothing when there were none: a restart of an idle collector is not
// news. Info when every connection ended by itself or was closed as idle,
// Warn when the deadline cut some - those were requests in flight, and
// "why did my upload fail at 03:12" is answered here.
func (s *Server) logDrain(r drainReport) {
	if r.open == 0 {
		return
	}
	level, msg := slog.LevelInfo, "proxy: drained the open connections"
	switch {
	case r.stuck:
		level, msg = slog.LevelError, "proxy: connections were closed at the shutdown deadline "+
			"and some did not finish; returning anyway"
	case r.cut > 0:
		level, msg = slog.LevelWarn, "proxy: the shutdown deadline closed connections still in use"
	}
	// One call with the keys written out, not three sharing a slice: the
	// export rules for the diagnostic file are held against the keys the
	// tree writes, and a key inside a variable is one that check cannot
	// read (internal/invariants/logkeys_test.go).
	s.logger().Log(context.Background(), level, msg, "open", r.open, "closed_idle", r.idle,
		"cut_at_deadline", r.cut, "took", r.took.Round(time.Millisecond))
}

// orDefault is d, or def when d is not positive.
func orDefault(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Server) handleConn(stopping context.Context, t *tracked) {
	conn := t.client
	// Ordered so conn.Close runs even when a panic unwinds through here:
	// deferred calls run last-in-first-out, so recoverConn stops the
	// unwinding first and Close then still fires. See recover.go for why
	// this exists at all.
	defer conn.Close()
	defer recoverConn(s.logger(), "handleConn")

	remoteIP, ok := ipFromAddr(conn.RemoteAddr())
	if !ok {
		s.logger().Warn("proxy: could not parse remote address, dropping connection", "addr", conn.RemoteAddr().String())
		return
	}

	if s.geoBlocked(remoteIP) {
		// Rejected before ever consuming a Limiter concurrency slot or
		// dialing the backend - a geo-block is unconditional, checked
		// ahead of and independent of Limiter's own decision (see
		// GeoBlocklist's doc comment for why).
		return
	}

	decision, release := s.admit(stopping)
	if release != nil {
		defer release()
	}
	switch decision {
	case limiter.DecisionReject:
		// fail_closed, over limit: refuse the connection outright. No
		// backend dial, no fingerprinting - just close (via the defer
		// above), as cheaply as a rejection can be.
		return
	case limiter.DecisionDegrade:
		// fail_open, over limit: skip the ClientHello peek and RecordRequest
		// entirely (the whole point is minimizing collector overhead while
		// it can't otherwise keep up) and splice bytes through unread.
		s.pipeToBackend(stopping, t, conn)
		return
	}

	peeked, fingerprint := sniffClientHello(conn, s.HandshakeTimeout)
	s.Store.RecordRequest(remoteIP, fingerprint, time.Now())

	s.pipeToBackend(stopping, t, io.MultiReader(bytes.NewReader(peeked), conn))
}

// admit consults Limiter, treating a nil Limiter (tests, or a config with
// every dimension explicitly unlimited) as always-proceed.
//
// The wait for a throttled slot ends with stopping, which a shutdown
// cancels at its deadline (Z7). It used to be context.Background: a
// connection still queued when the other connections were closed at the
// deadline would then take a freed slot and be spliced to the backend by
// a process on its way out.
func (s *Server) admit(stopping context.Context) (limiter.Decision, func()) {
	if s.Limiter == nil {
		return limiter.DecisionProceed, nil
	}
	return s.Limiter.Admit(stopping)
}

// geoBlocked reports whether remoteIP's country/ASN matches GeoBlocklist.
// Always false when there is no Resolver or nothing is blocked, so
// callers never need to check either themselves.
//
// Active rather than a nil check, since A5.2: the lists can be replaced
// while this server is running, so "is anything blocked" is a question
// per connection rather than one answered at startup. It stays one
// atomic load, which is what keeps a deployment that blocks nothing -
// the default - from paying for a geography lookup on every connection.
func (s *Server) geoBlocked(remoteIP netip.Addr) bool {
	if s.Resolver == nil || !s.GeoBlocklist.Active() {
		return false
	}
	geo := s.Resolver.Resolve(remoteIP)
	return s.GeoBlocklist.Blocked(geo.Country, geo.ASN)
}

// pipeToBackend dials BackendAddr and splices clientReader (everything the
// client sent, including any bytes already peeked from conn) to it.
//
// The dial ends with stopping as well as with DialTimeout, and the backend
// connection is registered with t before any byte is spliced, so a drain
// can close it: a backend that ignores the half-close would otherwise
// keep the copy towards the client open for as long as it liked.
func (s *Server) pipeToBackend(stopping context.Context, t *tracked, clientReader io.Reader) {
	dialTimeout := s.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 10 * time.Second
	}
	backendConn, err := (&net.Dialer{Timeout: dialTimeout}).DialContext(stopping, "tcp", s.BackendAddr)
	if err != nil {
		if stopping.Err() == nil {
			s.logger().Warn("proxy: dial backend failed", "backend", s.BackendAddr, "err", err)
		}
		return
	}
	if !t.attach(backendConn) {
		// A drain closed this connection while the dial was in flight;
		// attach has closed the backend too.
		return
	}
	defer backendConn.Close()

	pipeConns(s.logger(), t.client, clientReader, backendConn)
}
