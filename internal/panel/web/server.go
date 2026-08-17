package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// Server is the panel's HTTP surface.
type Server struct {
	ListenAddr string
	Renderer   *ui.Renderer
	Logger     *slog.Logger
	// HSTS is passed to the header middleware; see Config.HSTS.
	HSTS bool
	// Zone is the time zone every page renders in.
	Zone *time.Location
}

// Timeouts. A panel is not a streaming service: every response it
// produces is a page or a small fragment, so anything taking longer
// than these is a stuck request holding a connection, not slow work.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 60 * time.Second
	idleTimeout       = 2 * time.Minute
	shutdownGrace     = 15 * time.Second
)

// Handler builds the routing tree.
//
// Go 1.22's method-and-wildcard patterns are used throughout, which is
// what makes a wrong method a 405 rather than a 404 without a hand
// written check on every route - and 405 is the honest answer, because
// "this page does not exist" and "you called it wrong" send whoever is
// debugging in different directions.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	assets := s.Renderer.Assets()
	mux.Handle("GET "+ui.AssetPrefix+"{path...}", assets.Handler())
	mux.Handle("HEAD "+ui.AssetPrefix+"{path...}", assets.Handler())

	// "/" in a ServeMux matches everything nothing else claims, so this
	// is both the home route and the catch-all. It has to tell the two
	// apart itself.
	mux.HandleFunc("/", s.home)

	return SecurityHeaders(s.HSTS, s.requestLog(mux))
}

// SecurityHeaders is re-exported so the binary and the tests apply the
// same headers the renderer's tests assert.
func SecurityHeaders(hsts bool, next http.Handler) http.Handler {
	return ui.SecurityHeaders(hsts, next)
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.Renderer.Error(w, r, http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	default:
		w.Header().Set("Allow", "GET, HEAD")
		s.Renderer.Error(w, r, http.StatusMethodNotAllowed)
		return
	}
	// Everything behind the login form arrives with C2 onwards. Until
	// then this is the honest page: the panel is running and is asking
	// who you are.
	cat := s.Renderer.Catalog()
	s.Renderer.Render(w, r, http.StatusOK, "giris", &ui.Page{
		Title:   cat.T("giris.baslik"),
		Heading: cat.T("giris.baslik"),
		F:       ui.NewFormatter(s.Zone),
	})
}

// requestLog records one line per request, in the access category.
//
// Method, path and status only. No query string: it is where a
// password-reset token or a search term would be, and an access log is
// the file most likely to be copied into a support ticket.
func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.logger().Info("panel request",
			logging.In(logging.CategoryAccess),
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"ms", time.Since(started).Milliseconds(),
		)
	})
}

func (s *Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// statusRecorder remembers the status so the access log can report what
// was actually sent rather than what the handler intended.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written bool
}

func (w *statusRecorder) WriteHeader(status int) {
	if !w.written {
		w.status = status
		w.written = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(p []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(p)
}

// ListenAndServe runs until ctx is cancelled, then drains.
func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.ListenAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(s.logger().Handler(), slog.LevelWarn),
	}

	errs := make(chan error, 1)
	go func() {
		s.logger().Info("panel listening", "addr", s.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	s.logger().Info("panel shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	return <-errs
}
