// Package fullproxy implements the TLS-terminating "full mode" operating
// mode: unlike internal/proxy's content-blind passthrough, it decrypts
// client TLS connections with a real certificate/key and reverse-proxies
// HTTP to the backend via net/http, in exchange for real per-request
// visibility (HTTP/1.1 keep-alive and HTTP/2 multiplexing are both
// resolved into individual requests by net/http itself - no custom
// request-boundary detection needed) and a foundation later phases (e.g.
// path-scanning detection) can build on.
//
// It still fingerprints every connection via JA4, reusing
// ja4.ParseClientHelloFromRecords/Fingerprint unchanged: the ClientHello
// bytes are captured by snooping the raw connection (see snoop.go) before
// crypto/tls consumes them, since tls.ClientHelloInfo itself only exposes
// a parsed subset of fields, not the raw bytes JA4 needs. This guarantees
// full mode's JA4 output is computed by the exact same, already-validated
// code path as passthrough mode's, not a second parallel implementation.
package fullproxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/asnlookup"
	"github.com/cruciblelab/crucible-analytic/internal/ja4"
	"github.com/cruciblelab/crucible-analytic/internal/limiter"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// Resolver resolves an IP to country/ASN info for GeoBlocklist checks.
// *asnlookup.Resolver implements this; a narrow interface (rather than a
// direct dependency on *asnlookup.Resolver) so tests can substitute a
// fake instead of needing real loaded range tables - the same reasoning
// behind storage.GeoResolver and proxy.Resolver.
type Resolver interface {
	Resolve(ip netip.Addr) asnlookup.Result
}

// Server is a TLS-terminating HTTP reverse proxy fronting a single
// plaintext HTTP backend.
type Server struct {
	ListenAddr string
	// BackendAddr is a plain host:port; requests are proxied to it over
	// plaintext HTTP (the standard "TLS terminates at the edge" setup -
	// an HTTPS backend isn't supported yet).
	BackendAddr string
	// CertFile and KeyFile are the backend's real TLS certificate/key,
	// used to terminate client connections.
	CertFile string
	KeyFile  string
	Store    ratestore.RateStore
	// Limiter bounds total concurrent requests/requests-per-second across
	// all IPs, admitted once per HTTP request (not per connection - see
	// limiter.Limiter.Admit's doc comment for why full mode differs from
	// passthrough here). Nil means unlimited (mainly for tests that don't
	// care about this dimension) - production wiring in cmd/collector
	// always sets a real one, even if its own limits are effectively
	// unbounded.
	Limiter *limiter.Limiter
	// GeoBlocklist and Resolver together gate requests by country/ASN,
	// checked before Limiter and independent of it - see geoBlocked. Both
	// nil (the default - asn_lookup disabled, or enabled with no
	// blocklist entries) skips the check entirely, including the
	// Resolve() call, so this costs nothing extra on the request path
	// unless blocking is actually configured.
	GeoBlocklist *limiter.GeoBlocklist
	Resolver     Resolver
	// DialTimeout bounds connecting to the backend. Defaults to 10s if <= 0.
	DialTimeout time.Duration
	Logger      *slog.Logger
}

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// connStateKey is the context key recordingHandler uses to retrieve the
// connection's snoopConn (and thus its JA4 fingerprint) for each request.
type connStateKey struct{}

// ListenAndServe binds ListenAddr and serves until ctx is cancelled or a
// fatal error occurs.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.ListenAddr)
	if err != nil {
		return fmt.Errorf("fullproxy: listen on %s: %w", s.ListenAddr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve terminates TLS on ln (a plain TCP listener - Serve wraps it with
// TLS itself) and reverse-proxies to BackendAddr until ctx is cancelled,
// returning nil on a clean, ctx-triggered shutdown - the same contract as
// proxy.Server.Serve, so main.go can dispatch to either mode uniformly.
// Split out from ListenAndServe so tests can serve on an ephemeral (":0")
// listener and still learn the bound address.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	cert, err := tls.LoadX509KeyPair(s.CertFile, s.KeyFile)
	if err != nil {
		return fmt.Errorf("fullproxy: load TLS cert/key: %w", err)
	}

	dialTimeout := s.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 10 * time.Second
	}
	backendURL := &url.URL{Scheme: "http", Host: s.BackendAddr}
	reverseProxy := httputil.NewSingleHostReverseProxy(backendURL)
	reverseProxy.Transport = &http.Transport{
		DialContext: (&net.Dialer{Timeout: dialTimeout}).DialContext,
		// The zero-value/default Transport caps at 2 idle connections per
		// host (http.DefaultMaxIdleConnsPerHost) - fronting a single
		// backend host, that's nowhere near enough under concurrent load
		// and is a well-known cause of high CPU/low throughput in Go
		// reverse proxies (constant redial instead of connection reuse).
		MaxIdleConns:        200,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,
	}
	reverseProxy.ErrorLog = slog.NewLogLogger(s.logger().Handler(), slog.LevelWarn)

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		// http.Server's own auto-HTTP/2 setup (triggered from Serve, since
		// we never set httpServer.TLSConfig below) only prepares it to
		// *route* an already-negotiated "h2" connection - it does not add
		// "h2" to this Config's own ALPN offer, since this Config is ours,
		// built and passed to tls.NewListener independently. Without
		// listing it here explicitly, the real TLS handshake would never
		// offer h2 to clients at all, regardless of that other mechanism.
		NextProtos: []string{"h2", "http/1.1"},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			s.captureFingerprint(hello)
			return nil, nil // fall back to the base Config (Certificates above) unmodified
		},
	}
	tlsLn := tls.NewListener(&snoopListener{Listener: ln}, tlsConfig)

	httpServer := &http.Server{
		Handler:     s.recordingHandler(reverseProxy),
		ConnContext: connectFingerprintContext,
	}

	s.logger().Info("fullproxy listening", "addr", ln.Addr().String(), "backend", backendURL.String())

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- httpServer.Serve(tlsLn) }()

	select {
	case err := <-serveErrCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("fullproxy: shutdown: %w", err)
		}
		<-serveErrCh
		return nil
	}
}

// connectFingerprintContext stashes the connection's snoopConn into the
// request context, so recordingHandler can retrieve it for every request
// handled on that connection. It runs once per connection, right after
// Accept and before the TLS handshake - by the time any request is
// actually handled, the handshake (and thus captureFingerprint) has
// necessarily already completed.
func connectFingerprintContext(ctx context.Context, c net.Conn) context.Context {
	tlsConn, ok := c.(*tls.Conn)
	if !ok {
		return ctx
	}
	sc, ok := tlsConn.NetConn().(*snoopConn)
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, connStateKey{}, sc)
}

// captureFingerprint runs inside the TLS handshake, once per connection,
// after crypto/tls has read and parsed the ClientHello. It's best-effort
// like the passthrough proxy's sniffing: a failure here never affects the
// handshake or the connection - hello.Conn.(*snoopConn) not matching, or
// parsing failing, just leaves that connection's fingerprint empty.
func (s *Server) captureFingerprint(hello *tls.ClientHelloInfo) {
	sc, ok := hello.Conn.(*snoopConn)
	if !ok {
		return
	}
	raw := sc.stopCapturing()

	ch, err := ja4.ParseClientHelloFromRecords(raw)
	if err != nil {
		s.logger().Debug("fullproxy: could not fingerprint ClientHello", "err", err)
		return
	}
	sc.setFingerprint(ja4.Fingerprint(ch))
}

// recordingHandler admits and records one RateStore request per actual
// HTTP request - not per connection - before forwarding to next. Because
// net/http itself resolves both HTTP/1.1 keep-alive and HTTP/2
// multiplexing into separate handler invocations, this "just works"
// without any custom request framing logic.
//
// The geo-block check runs first, ahead of and independent of Limiter's
// own decision (same reasoning as proxy.Server.handleConn - see
// GeoBlocklist's doc comment): a match is rejected with 403 before ever
// consuming a Limiter concurrency slot, distinct from the 503 the
// existing overload rejection below returns, so the two reasons a
// request was refused stay distinguishable in logs/responses. Limiter
// admission runs next: a rejected request never reaches next (or Store)
// at all, and a degraded one reaches next but skips RecordRequest - the
// same Proceed/Degrade/Reject split proxy.Server applies per connection,
// just at request granularity here since that's the unit HTTP/2
// multiplexing forces on full mode anyway.
func (s *Server) recordingHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc, _ := r.Context().Value(connStateKey{}).(*snoopConn)
		var remoteIP netip.Addr
		var haveIP bool
		if sc != nil {
			remoteIP, haveIP = ipFromAddr(sc.RemoteAddr())
		}

		if haveIP && s.geoBlocked(remoteIP) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		decision, release := s.admit(r.Context())
		if release != nil {
			defer release()
		}
		if decision == limiter.DecisionReject {
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			return
		}

		if decision == limiter.DecisionProceed && haveIP {
			s.Store.RecordRequest(remoteIP, sc.fingerprint(), time.Now())
		}
		next.ServeHTTP(w, r)
	})
}

// admit consults Limiter, treating a nil Limiter (tests, or a config with
// every dimension explicitly unlimited) as always-proceed. Admitting on
// the request's own context (rather than context.Background()) means a
// client that disconnects while queued under the throttle policy stops
// waiting immediately instead of holding a queue slot for a response
// nobody will read.
func (s *Server) admit(ctx context.Context) (limiter.Decision, func()) {
	if s.Limiter == nil {
		return limiter.DecisionProceed, nil
	}
	return s.Limiter.Admit(ctx)
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
