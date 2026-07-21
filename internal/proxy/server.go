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
	"sync"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/limiter"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// Server proxies TCP connections from ListenAddr to BackendAddr, recording
// a JA4-fingerprinted request per connection into Store. It never rejects
// or delays a connection because fingerprinting failed or timed out - the
// whole point is to observe, not gate. Limiter (see internal/limiter) is
// the one thing that *does* gate connections, bounding the collector's
// own total resource usage independent of per-IP behavior.
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

	// HandshakeTimeout bounds how long sniffing waits to see a complete
	// ClientHello before giving up and proxying unfingerprinted. Zero
	// disables the deadline (not recommended outside tests).
	HandshakeTimeout time.Duration
	// DialTimeout bounds connecting to the backend. Defaults to 10s if <= 0.
	DialTimeout time.Duration

	Logger *slog.Logger
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
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	s.logger().Info("proxy listening", "addr", ln.Addr().String(), "backend", s.BackendAddr)

	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				wg.Wait()
				return nil
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()

	decision, release := s.admit()
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
		s.pipeToBackend(conn, conn)
		return
	}

	remoteIP, ok := ipFromAddr(conn.RemoteAddr())
	if !ok {
		s.logger().Warn("proxy: could not parse remote address, dropping connection", "addr", conn.RemoteAddr().String())
		return
	}

	peeked, fingerprint := sniffClientHello(conn, s.HandshakeTimeout)
	s.Store.RecordRequest(remoteIP, fingerprint, time.Now())

	s.pipeToBackend(conn, io.MultiReader(bytes.NewReader(peeked), conn))
}

// admit consults Limiter, treating a nil Limiter (tests, or a config with
// every dimension explicitly unlimited) as always-proceed.
func (s *Server) admit() (limiter.Decision, func()) {
	if s.Limiter == nil {
		return limiter.DecisionProceed, nil
	}
	return s.Limiter.Admit(context.Background())
}

// pipeToBackend dials BackendAddr and splices clientReader (everything the
// client sent, including any bytes already peeked from conn) to it.
func (s *Server) pipeToBackend(conn net.Conn, clientReader io.Reader) {
	dialTimeout := s.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 10 * time.Second
	}
	backendConn, err := net.DialTimeout("tcp", s.BackendAddr, dialTimeout)
	if err != nil {
		s.logger().Warn("proxy: dial backend failed", "backend", s.BackendAddr, "err", err)
		return
	}
	defer backendConn.Close()

	pipeConns(conn, clientReader, backendConn)
}
