package beacon

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/asnlookup"
	"github.com/cruciblelab/crucible-analytic/internal/limiter"
)

//go:embed beacon.js
var scriptSource []byte

// scriptETag is the script's content hash, computed once at startup.
// Serving it lets browsers revalidate with a conditional request
// instead of re-downloading the script on every cache expiry, and it
// changes automatically when the file does - no version string to
// remember to bump.
var scriptETag = func() string {
	sum := sha256.Sum256(scriptSource)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}()

// maxBodyBytes caps an event payload. Generous next to a real event
// (a few hundred bytes) and small enough that flooding the endpoint
// costs the sender far more bandwidth than it costs this process.
const maxBodyBytes = 8 << 10

// DefaultPathPrefix is where the beacon mounts by default.
//
// Short and unbranded on purpose. Filter lists block analytics by URL
// pattern, and a path containing "analytics", "track" or a recognizable
// vendor name is the single easiest thing for them to match - which is
// also why the recommended deployment serves this from the site's own
// origin rather than a shared one.
const DefaultPathPrefix = "/_ca"

// Sink accepts finished rows for persistence. *Writer implements it;
// tests substitute a fake so the HTTP layer can be exercised without a
// database. Enqueue reports false when the row was dropped because the
// buffer is full - see Writer for why dropping is the correct behavior
// there rather than blocking.
type Sink interface {
	Enqueue(Row) bool
}

// GeoResolver enriches an event with country/ASN. *asnlookup.Resolver
// implements it; declared here as a narrow interface for the same
// reason storage.GeoResolver is - so tests need no loaded range tables.
type GeoResolver interface {
	Resolve(ip netip.Addr) asnlookup.Result
}

// Server is the public beacon ingest endpoint.
//
// It runs as its own process, apart from both the collector and the
// read API, and the reasons are worth being explicit about because they
// are what the split buys:
//
//   - This is the only component of the system the entire internet can
//     POST to. The read API is token-gated and the collector accepts no
//     structured input at all; putting attacker-supplied JSON parsing in
//     either would hand both of those a threat surface they currently
//     do not have.
//   - This writes. The read API's database role is read-only, and that
//     guarantee is only real while no writer shares its process.
//   - The collector sits in the traffic path, where its own failure
//     takes the site down with it. The beacon failing loses analytics
//     events and nothing else, and it should stay that way.
type Server struct {
	ListenAddr string
	// PathPrefix is where the routes mount; "" means DefaultPathPrefix.
	PathPrefix string
	// Sites is the allowlist of site_ids this server accepts events
	// for. The snippet is public and its data-site attribute is a claim
	// anyone can copy, so this is the only thing stopping an arbitrary
	// caller from writing rows under someone else's site.
	Sites []string
	Sink  Sink
	// Visitors derives cookieless visitor IDs. A nil value is lazily
	// replaced with a working zero-value VisitorIDs; main.go still
	// constructs one eagerly so a broken randomness source fails at
	// startup rather than on first visitor.
	Visitors *VisitorIDs
	// Resolver is optional; nil leaves country/ASN empty, the same
	// "not resolved" convention storage.BuildRows uses.
	Resolver GeoResolver
	// Limiter is optional; nil means no admission control.
	Limiter  *limiter.Limiter
	ClientIP ClientIPResolver
	// AllowedOrigins narrows CORS. Empty means every origin is allowed,
	// which is safe here and not the usual laxness it looks like: the
	// endpoint is write-only, its success response is an empty 204, and
	// it is called with credentials omitted - so there is no response
	// content for another origin to steal and no ambient authority to
	// abuse. CORS would also not be a defense worth having: it
	// constrains browsers, and anything determined to post junk here
	// would not be using one.
	AllowedOrigins []string
	Logger         *slog.Logger
	// Now supplies event timestamps; nil means time.Now.
	Now func() time.Time

	visitorsOnce sync.Once
	dropped      atomic.Uint64
	rejected     atomic.Uint64
	accepted     atomic.Uint64
}

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) pathPrefix() string {
	prefix := s.PathPrefix
	if prefix == "" {
		return DefaultPathPrefix
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return strings.TrimRight(prefix, "/")
}

func (s *Server) visitors() *VisitorIDs {
	s.visitorsOnce.Do(func() {
		if s.Visitors == nil {
			// The zero value is fully functional: the first ID request
			// draws a salt lazily. See VisitorIDs.currentSalt.
			s.Visitors = &VisitorIDs{}
		}
	})
	return s.Visitors
}

// Counters reports what this process has done with the events it was
// offered, for logging and for tests. Accepted counts rows handed to
// the sink; Dropped counts rows the sink had no room for; Rejected
// counts requests refused before a row was ever built (bad payload,
// unknown site, over limit).
func (s *Server) Counters() (accepted, dropped, rejected uint64) {
	return s.accepted.Load(), s.dropped.Load(), s.rejected.Load()
}

// Handler builds the routed handler. Exported separately from Serve so
// tests can drive it through httptest without binding a port, matching
// api.Server.
func (s *Server) Handler() http.Handler {
	prefix := s.pathPrefix()
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+prefix+"/ca.js", s.handleScript)
	mux.HandleFunc("POST "+prefix+"/event", s.handleEvent)
	// Only reached when the snippet is served from a different origin
	// than the endpoint. The default same-origin deployment never
	// preflights, and even cross-origin the snippet sends a
	// CORS-"simple" request that skips preflight - this exists so a
	// caller that does send application/json still works.
	mux.HandleFunc("OPTIONS "+prefix+"/event", s.handlePreflight)

	// Unauthenticated and dataless, exactly as in the read API.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"status":"ok"}`)
	})

	return mux
}

func (s *Server) handleScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("ETag", scriptETag)
	// An hour: long enough that repeat visitors rarely re-fetch, short
	// enough that a change to the snippet reaches the world the same
	// day. The ETag makes the revalidation itself nearly free.
	w.Header().Set("Cache-Control", "public, max-age=3600")

	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, scriptETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(scriptSource)
}

func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	s.setCORS(w, r)
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Max-Age", "86400")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleEvent(w http.ResponseWriter, r *http.Request) {
	s.setCORS(w, r)

	if !s.admit(w, r) {
		return
	}

	ip, ok := s.ClientIP.ClientIP(r)
	if !ok {
		// Without an address there is no visitor ID and no join key, so
		// the row would be worth very little and would still cost a
		// write. This is not reachable over TCP; it exists so a future
		// listener that isn't TCP fails closed.
		s.reject(w, http.StatusBadRequest, "unresolvable client address")
		return
	}

	// The declared Content-Type is deliberately not checked: the
	// snippet sends text/plain to stay a CORS-simple request and avoid
	// a preflight per event (see beacon.js), so the body's type header
	// says nothing useful about its contents either way.
	// Unknown fields are ignored rather than rejected. The snippet is
	// cached in browsers for an hour, so during any rollout that adds a
	// field there is a window where new scripts talk to old servers;
	// strict decoding would turn that window into total event loss for
	// everyone whose cache had already refreshed.
	var event Event
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := decoder.Decode(&event); err != nil {
		s.reject(w, http.StatusBadRequest, "malformed event")
		return
	}

	if err := event.Validate(); err != nil {
		s.reject(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.allowsSite(event.Site) {
		s.reject(w, http.StatusForbidden, fmt.Sprintf("unknown site %q", sanitizeText(event.Site, 64)))
		return
	}

	userAgent := r.Header.Get("User-Agent")
	visitorID, err := s.visitors().ID(event.Site, ip, userAgent)
	if err != nil {
		// Only reachable if the system randomness source failed. Never
		// fall back to an unsalted hash - see VisitorIDs.ID.
		s.logger().Error("beacon: visitor id unavailable", "err", err)
		s.reject(w, http.StatusServiceUnavailable, "temporarily unavailable")
		return
	}

	var geo asnlookup.Result
	if s.Resolver != nil {
		geo = s.Resolver.Resolve(ip)
	}

	row := BuildRow(event, Enrichment{
		Time:      s.now(),
		IP:        ip,
		VisitorID: visitorID,
		UserAgent: ParseUserAgent(userAgent),
		Country:   geo.Country,
		ASN:       geo.ASN,
		ASNOrg:    geo.ASNName,
	})

	if s.Sink.Enqueue(row) {
		s.accepted.Add(1)
	} else {
		s.dropped.Add(1)
	}

	// 204 either way. A browser cannot usefully act on "your pageview
	// was dropped" - sendBeacon has already discarded the response, and
	// a retry would only add load to a process that just said it has
	// none to spare.
	w.WriteHeader(http.StatusNoContent)
}

// admit applies the limiter, translating its decisions into what they
// mean for an endpoint with no backend to protect.
func (s *Server) admit(w http.ResponseWriter, r *http.Request) bool {
	if s.Limiter == nil {
		return true
	}

	decision, release := s.Limiter.Admit(r.Context())
	if release != nil {
		defer release()
	}

	switch decision {
	case limiter.DecisionProceed:
		return true
	case limiter.DecisionDegrade:
		// fail_open means "never be the reason the site breaks" for the
		// collector, which sits in front of one. The beacon sits in
		// front of nothing, so the equivalent is to accept the request
		// and drop the event: the visitor's page is unaffected, and the
		// client is told nothing that would make it retry.
		s.dropped.Add(1)
		w.WriteHeader(http.StatusNoContent)
		return false
	default:
		s.rejected.Add(1)
		// Retry-After keeps a well-behaved client from hot-looping;
		// the snippet does not retry at all.
		w.Header().Set("Retry-After", "60")
		writeError(w, http.StatusTooManyRequests, "over capacity")
		return false
	}
}

func (s *Server) reject(w http.ResponseWriter, status int, message string) {
	s.rejected.Add(1)
	writeError(w, status, message)
}

func (s *Server) allowsSite(site string) bool {
	for _, allowed := range s.Sites {
		if allowed == site {
			return true
		}
	}
	return false
}

func (s *Server) setCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	if len(s.AllowedOrigins) == 0 {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		return
	}
	for _, allowed := range s.AllowedOrigins {
		if allowed == "*" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			return
		}
		if strings.EqualFold(allowed, origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			// Without Vary, a shared cache could serve one origin's
			// allow header to another origin's request.
			w.Header().Add("Vary", "Origin")
			return
		}
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// ListenAndServe binds ListenAddr and serves until ctx is cancelled,
// returning nil on a clean shutdown - the same contract as
// api.Server.Serve, proxy.Server.Serve and fullproxy.Server.Serve.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.ListenAddr)
	if err != nil {
		return fmt.Errorf("beacon: listen on %s: %w", s.ListenAddr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve serves on ln until ctx is cancelled. Split out from
// ListenAndServe so tests can serve on an ephemeral (":0") listener and
// still learn the bound address.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	httpServer := &http.Server{
		Handler: s.Handler(),
		// Tighter than the read API's: every legitimate request here is
		// one small POST from a browser, so there is no reason to hold
		// a connection open for a slow one.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	s.logger().Info("beacon listening", "addr", ln.Addr().String(), "path_prefix", s.pathPrefix(), "sites", len(s.Sites))

	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- httpServer.Serve(ln) }()

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
			return fmt.Errorf("beacon: shutdown: %w", err)
		}
		<-serveErrCh
		return nil
	}
}
