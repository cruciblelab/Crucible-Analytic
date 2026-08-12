package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// Querier is the read-only data access the handlers need. *Store
// implements it; tests substitute a fake so the HTTP/auth layer can be
// exercised without a live database.
type Querier interface {
	Sites(ctx context.Context) ([]string, error)
	Overview(ctx context.Context, sites []string, from, to time.Time, botScoreMin int) ([]SiteOverview, error)
	Summary(ctx context.Context, siteID string, from, to time.Time, botScoreMin int) (Summary, error)
	Timeseries(ctx context.Context, siteID string, from, to time.Time, interval string, botScoreMin int) ([]Bucket, error)
	TopIPs(ctx context.Context, siteID string, from, to time.Time, limit, offset int) ([]IPStat, int, error)
	Countries(ctx context.Context, siteID string, from, to time.Time, limit, offset, botScoreMin int) ([]GroupStat, int, error)
	ASNs(ctx context.Context, siteID string, from, to time.Time, limit, offset, botScoreMin int) ([]GroupStat, int, error)
	JA4s(ctx context.Context, siteID string, from, to time.Time, limit, offset, botScoreMin int) ([]JA4Stat, int, error)
	ScoreDistribution(ctx context.Context, siteID string, from, to time.Time) ([]ScoreBucket, error)
	IPDetail(ctx context.Context, siteID string, ip netip.Addr, from, to time.Time, limit int) (IPDetail, error)
	Snapshots(ctx context.Context, siteID string, from, to time.Time, limit, offset int) ([]Snapshot, int, error)
}

// Server serves the read-only JSON API.
type Server struct {
	ListenAddr string
	Store      Querier
	Auth       *Authenticator
	Logger     *slog.Logger
	// Now supplies the current time for range defaults; nil means
	// time.Now. Injectable so tests get deterministic default ranges.
	Now func() time.Time
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

// Handler builds the routed, authenticated handler. Exported separately
// from Serve so tests can drive it through httptest without binding a
// port.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Go 1.22+ patterns: the method is part of the pattern, so anything
	// other than GET on these paths gets a 405 from the mux itself rather
	// than needing a check in every handler.
	mux.HandleFunc("GET /api/v1/sites", s.handleSites)
	mux.HandleFunc("GET /api/v1/overview", s.handleOverview)
	mux.HandleFunc("GET /api/v1/sites/{site}/summary", s.siteHandler(s.handleSummary))
	mux.HandleFunc("GET /api/v1/sites/{site}/timeseries", s.siteHandler(s.handleTimeseries))
	mux.HandleFunc("GET /api/v1/sites/{site}/top-ips", s.siteHandler(s.handleTopIPs))
	mux.HandleFunc("GET /api/v1/sites/{site}/countries", s.siteHandler(s.handleCountries))
	mux.HandleFunc("GET /api/v1/sites/{site}/asns", s.siteHandler(s.handleASNs))
	mux.HandleFunc("GET /api/v1/sites/{site}/ja4", s.siteHandler(s.handleJA4s))
	mux.HandleFunc("GET /api/v1/sites/{site}/score-distribution", s.siteHandler(s.handleScoreDistribution))
	mux.HandleFunc("GET /api/v1/sites/{site}/ips/{ip}", s.siteHandler(s.handleIPDetail))
	mux.HandleFunc("GET /api/v1/sites/{site}/snapshots", s.siteHandler(s.handleSnapshots))

	// Unauthenticated on purpose: it reports only that the process is up,
	// no data at all, so a load balancer or uptime check can use it
	// without being handed a credential.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"status":"ok"}`)
	})

	return s.withAuth(mux)
}

// tokenContextKey carries the authenticated Token from the auth
// middleware to the handlers.
type tokenContextKey struct{}

// withAuth rejects any request to /api/ that doesn't carry a valid token,
// before routing reaches a handler. /healthz is deliberately left open -
// see its registration above.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}

		tok, ok := s.Auth.Authenticate(r)
		if !ok {
			// WWW-Authenticate tells a well-behaved client what scheme to
			// use; the body deliberately doesn't distinguish "no token"
			// from "wrong token".
			w.Header().Set("WWW-Authenticate", `Bearer realm="crucible-analytics"`)
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tokenContextKey{}, tok)))
	})
}

// siteHandler wraps a per-site handler with the authorization check that
// the request's token may actually read the site named in the path, so no
// individual handler can forget it.
func (s *Server) siteHandler(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		site := r.PathValue("site")
		tok, ok := r.Context().Value(tokenContextKey{}).(Token)
		if !ok {
			// Unreachable while withAuth wraps every /api/ route, but a
			// future refactor that broke that should fail closed, not open.
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		if !tok.CanRead(site) {
			// 403 rather than 404: site IDs appear in the panel's own URLs
			// and aren't secrets, so a clear "your token doesn't cover this
			// site" beats pretending the site doesn't exist and making a
			// misconfigured token look like a missing site.
			writeError(w, http.StatusForbidden, fmt.Sprintf("token %q is not allowed to read site %q", tok.Name, site))
			return
		}
		next(w, r, site)
	}
}

func (s *Server) handleSites(w http.ResponseWriter, r *http.Request) {
	tok, _ := r.Context().Value(tokenContextKey{}).(Token)

	// A wildcard token has to ask the database what exists; an
	// enumerated token already knows, and shouldn't be told about sites
	// it can't read.
	var sites []string
	if tok.CanRead(WildcardSite) {
		all, err := s.Store.Sites(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		sites = all
	} else {
		sites = tok.Sites
	}

	writeJSON(w, http.StatusOK, map[string]any{"sites": sites})
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request, site string) {
	q := r.URL.Query()
	from, to, err := ParseRange(q, s.now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	botScoreMin, err := ParseBotScoreMin(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	summary, err := s.Store.Summary(r.Context(), site, from, to, botScoreMin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

func (s *Server) handleTimeseries(w http.ResponseWriter, r *http.Request, site string) {
	q := r.URL.Query()
	from, to, err := ParseRange(q, s.now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	interval, err := ParseInterval(q.Get("interval"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	botScoreMin, err := ParseBotScoreMin(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	buckets, err := s.Store.Timeseries(r.Context(), site, from, to, interval, botScoreMin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"site_id":  site,
		"from":     from,
		"to":       to,
		"interval": interval,
		"buckets":  buckets,
	})
}

func (s *Server) handleTopIPs(w http.ResponseWriter, r *http.Request, site string) {
	p, ok := s.parseListParams(w, r)
	if !ok {
		return
	}

	ips, total, err := s.Store.TopIPs(r.Context(), site, p.from, p.to, p.limit, p.offset)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p.envelope(site, "ips", ips, total))
}

func (s *Server) handleCountries(w http.ResponseWriter, r *http.Request, site string) {
	p, ok := s.parseListParams(w, r)
	if !ok {
		return
	}

	stats, total, err := s.Store.Countries(r.Context(), site, p.from, p.to, p.limit, p.offset, p.botScoreMin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p.envelope(site, "countries", stats, total))
}

func (s *Server) handleASNs(w http.ResponseWriter, r *http.Request, site string) {
	p, ok := s.parseListParams(w, r)
	if !ok {
		return
	}

	stats, total, err := s.Store.ASNs(r.Context(), site, p.from, p.to, p.limit, p.offset, p.botScoreMin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p.envelope(site, "asns", stats, total))
}

func (s *Server) handleJA4s(w http.ResponseWriter, r *http.Request, site string) {
	p, ok := s.parseListParams(w, r)
	if !ok {
		return
	}

	stats, total, err := s.Store.JA4s(r.Context(), site, p.from, p.to, p.limit, p.offset, p.botScoreMin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p.envelope(site, "ja4", stats, total))
}

func (s *Server) handleSnapshots(w http.ResponseWriter, r *http.Request, site string) {
	p, ok := s.parseListParams(w, r)
	if !ok {
		return
	}

	snaps, total, err := s.Store.Snapshots(r.Context(), site, p.from, p.to, p.limit, p.offset)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, p.envelope(site, "snapshots", snaps, total))
}

func (s *Server) handleScoreDistribution(w http.ResponseWriter, r *http.Request, site string) {
	from, to, err := ParseRange(r.URL.Query(), s.now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	buckets, err := s.Store.ScoreDistribution(r.Context(), site, from, to)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"site_id": site, "from": from, "to": to, "buckets": buckets})
}

func (s *Server) handleIPDetail(w http.ResponseWriter, r *http.Request, site string) {
	q := r.URL.Query()
	from, to, err := ParseRange(q, s.now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := ParseLimit(q, 500)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Parsed rather than passed through: an unparseable value should be a
	// clear 400, not a database error, and pgx needs a real netip.Addr to
	// bind against an inet column anyway.
	ip, err := netip.ParseAddr(r.PathValue("ip"))
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid ip %q", r.PathValue("ip")))
		return
	}

	detail, err := s.Store.IPDetail(r.Context(), site, ip.Unmap(), from, to, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to, err := ParseRange(q, s.now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	botScoreMin, err := ParseBotScoreMin(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	tok, _ := r.Context().Value(tokenContextKey{}).(Token)
	// nil means "no restriction" to the store; only a wildcard token gets
	// that, and an enumerated token is restricted to exactly its grant so
	// it can never see another customer's row here.
	var sites []string
	if !tok.CanRead(WildcardSite) {
		sites = tok.Sites
	}

	overview, err := s.Store.Overview(r.Context(), sites, from, to, botScoreMin)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "bot_score_min": botScoreMin, "sites": overview})
}

// listParams is the parameter set every paginated list endpoint shares.
type listParams struct {
	from, to    time.Time
	limit       int
	offset      int
	botScoreMin int
}

// envelope wraps a page of results with the paging metadata a UI needs to
// render "showing X-Y of Z", under a key naming what the rows are.
func (p listParams) envelope(site, key string, rows any, total int) map[string]any {
	return map[string]any{
		"site_id": site,
		"from":    p.from,
		"to":      p.to,
		"limit":   p.limit,
		"offset":  p.offset,
		"total":   total,
		key:       rows,
	}
}

// parseListParams reads the shared list parameters, writing the 400
// itself and reporting ok=false if any is invalid.
func (s *Server) parseListParams(w http.ResponseWriter, r *http.Request) (listParams, bool) {
	q := r.URL.Query()
	from, to, err := ParseRange(q, s.now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return listParams{}, false
	}
	limit, err := ParseLimit(q, 50)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return listParams{}, false
	}
	offset, err := ParseOffset(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return listParams{}, false
	}
	botScoreMin, err := ParseBotScoreMin(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return listParams{}, false
	}
	return listParams{from: from, to: to, limit: limit, offset: offset, botScoreMin: botScoreMin}, true
}

// fail logs the real error and returns a generic one, so a database error
// message never reaches a client.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.logger().Error("api: query failed", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status line and likely some body are already written, so
		// there's nothing useful left to say to the client; the connection
		// will simply end up truncated.
		slog.Default().Error("api: encoding response failed", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// ListenAndServe binds ListenAddr and serves until ctx is cancelled,
// returning nil on a clean, ctx-triggered shutdown - the same contract as
// proxy.Server.Serve and fullproxy.Server.Serve.
func (s *Server) ListenAndServe(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.ListenAddr)
	if err != nil {
		return fmt.Errorf("api: listen on %s: %w", s.ListenAddr, err)
	}
	return s.Serve(ctx, ln)
}

// Serve serves on ln until ctx is cancelled. Split out from
// ListenAndServe so tests can serve on an ephemeral (":0") listener and
// still learn the bound address.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	httpServer := &http.Server{
		Handler: s.Handler(),
		// A read-only JSON API has no reason to hold a connection open
		// while a slow client dribbles out a request; these bound the
		// classic slowloris shape without needing a limiter.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	s.logger().Info("analytics api listening", "addr", ln.Addr().String())

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
			return fmt.Errorf("api: shutdown: %w", err)
		}
		<-serveErrCh
		return nil
	}
}
