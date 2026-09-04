package web

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
	"github.com/cruciblelab/crucible-analytic/internal/panel/preflight"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
	"github.com/cruciblelab/crucible-analytic/internal/sealed"
)

// Server is the panel's HTTP surface.
type Server struct {
	ListenAddr string
	Renderer   *ui.Renderer
	Logger     *slog.Logger
	// Store is the panel's own database.
	Store *panel.Store
	// Sessions carries logins and CSRF tokens.
	Sessions *panel.Sessions
	// Analytics reads traffic numbers from the read-only API.
	//
	// Nil is a supported state and renders as "the numbers are not
	// available", which is what a deployment configured before group D
	// existed has - and what one mid-installation has too. A panel that
	// refused to start over an unset API address would make an upgrade
	// an outage.
	Analytics *analytics.Client
	// Gate guards the settings with legal weight. Nil means no
	// developer password is configured, which freezes those settings at
	// their defaults - the safe direction, and the wizard says so.
	Gate *devgate.Gate
	// Preflight runs the setup checks. Nil is survivable rather than
	// fatal - preflight.Run reports every database check as a skip and
	// handover stays blocked - because a panic on the last step of the
	// wizard, one button from handover, is the worst outcome available.
	Preflight *preflight.Checker
	// PreflightConfig tells those checks where to look.
	PreflightConfig preflight.Config
	// HealthCheckBudget bounds the check run behind the health page's
	// setup-check section. Zero means defaultHealthCheckBudget.
	//
	// A field rather than a constant because the behaviour it produces -
	// a page that renders every other section while one of them reports
	// a database it could not reach - cannot be shown at all without
	// waiting out whatever the budget is. Production never sets it.
	HealthCheckBudget time.Duration
	// ConfigPath is the panel's own config file, quoted back in the
	// command the first-run page tells the installer to run.
	ConfigPath string
	// ConfigFileValues are the config-file settings the panel may
	// display, keyed "service.toml.path". Secrets are never included -
	// see panel.ConfigFileSettings, which drops them on the entry
	// itself so a call site cannot pass one by accident.
	ConfigFileValues map[string]string
	// StoragePaths are the directories this deployment was configured
	// with, whose filesystems the health page measures. Derived from the
	// config by Config.StoragePaths rather than listed at the call site,
	// so a directory added to the config cannot quietly go unmeasured.
	//
	// Empty is survivable and renders as "nothing was configured to
	// measure", which is true of a panel started by hand with no log
	// directory.
	StoragePaths []StoragePath
	// DatabaseIsLocal is whether the DSN points at this machine.
	//
	// It changes nothing that is measured and only which of two honest
	// sentences the page prints: the panel cannot see the database's
	// disk in either case, because finding a data directory needs a
	// privilege its role does not have. What it can say is whether that
	// disk is one of the ones above or somewhere else entirely.
	DatabaseIsLocal bool
	// SecretKey encrypts the stored SMTP password. See internal/sealed.
	//
	// A zero Key is a supported state and means no mail password can be
	// stored - the mail page renders and says so. Parsed once at
	// startup rather than on each request: a key that does not parse is
	// a startup error (see Config.validate), so by the time a request
	// arrives the only two possibilities are "set" and "not
	// configured".
	SecretKey sealed.Key
	// HSTS is passed to the header middleware; see Config.HSTS.
	HSTS bool
	// Zone is the fallback time zone, from the config file.
	//
	// A fallback rather than the answer: the panel.timezone setting wins
	// when it is set, because the customer knows their own timezone
	// better than whoever installed the deployment. See Server.zone.
	Zone *time.Location
	// ConfiguredTimezone is that config-file value as written, shown to
	// the customer so they can see what they are overriding rather than
	// replacing a value they never knew existed.
	ConfiguredTimezone string
	// BeaconURL is the public address the beacon is reached at, used to
	// build the snippet the customer embeds.
	//
	// Empty is a supported state: the panel reads only its own config
	// file, so a deployment that did not tell it produces a step saying
	// where to find the snippet instead of one printing a tag that
	// points nowhere.
	BeaconURL string
	// Language is the deployment's preferred language code. A reader
	// whose browser asks for another language the panel carries gets
	// that one instead; this is the answer when the browser expresses no
	// preference the panel can serve.
	Language string
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

	mux.HandleFunc(DevAccessPathPrefix+"{token...}", s.devAccessHandler)
	mux.HandleFunc(SetupPathPrefix+"{step...}", s.setupHandler)

	// The customer's side. Registered with method-and-wildcard patterns
	// so that a wrong method is a 405 rather than falling through to the
	// catch-all and being reported as a page that does not exist.
	mux.HandleFunc(LoginPath, s.loginHandler)
	// Registered before SecondFactorPath would matter: both live under
	// /giris/, and ServeMux matches the longer pattern regardless of
	// order, so this is placement for a reader rather than for the mux.
	mux.HandleFunc(RecoveryPath, s.recoveryHandler)
	mux.HandleFunc(SecondFactorPath, s.secondFactorHandler)
	mux.HandleFunc(LogoutPath, s.logoutHandler)
	mux.HandleFunc(AccountPath, s.accountHandler)
	mux.HandleFunc(TOTPQRPath, s.totpQRHandler)
	mux.HandleFunc(MembersPathPrefix+"{site}"+membersPathSuffix, s.membersHandler)
	mux.HandleFunc(MembersPathPrefix+"{site}"+settingsPathSuffix, s.settingsHandler)
	mux.HandleFunc(MembersPathPrefix+"{site}"+dashboardPathSuffix, s.dashboardHandler)
	mux.HandleFunc(MembersPathPrefix+"{site}"+breakdownPathSegment+"{kirilim}", s.detailHandler)
	mux.HandleFunc(MembersPathPrefix+"{site}"+addressListPathSegment+"{liste}", s.addressListHandler)
	mux.HandleFunc(DevAccessRequestsPath, s.devAccessRequestsHandler)
	mux.HandleFunc(MailPath, s.mailHandler)
	mux.HandleFunc(HealthPath, s.healthHandler)
	mux.HandleFunc(ClaimPathPrefix+"{token...}", s.claimHandler)
	mux.HandleFunc(WelcomePathPrefix+"{step...}", s.welcomeHandler)
	mux.HandleFunc(TechnicalDoorPath, s.technicalDoorHandler)

	// "/" in a ServeMux matches everything nothing else claims, so this
	// is both the home route and the catch-all. It has to tell the two
	// apart itself.
	mux.HandleFunc("/", s.home)

	// Language is resolved once, outermost but inside the logging and
	// header middleware, so every handler and every error page below can
	// read it off the request rather than negotiating again. The session
	// middleware is innermost so that only handlers touch the cookie -
	// the asset routes above never load or save a session.
	var handler http.Handler = mux
	if s.Sessions != nil {
		handler = s.Sessions.Middleware(handler)
	}
	// LimitRequestBodies is outermost of the three so that nothing below
	// it - not the session middleware, not a handler, not a future
	// addition - can be handed a body with no ceiling.
	return LimitRequestBodies(SecurityHeaders(s.HSTS, s.requestLog(
		ui.LanguageMiddleware(s.Renderer.Catalogs(), s.Language, handler))))
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
	lang := s.language(r)

	// A deployment nobody owns yet has nowhere to log in to. Sending
	// somebody to a sign-in form when no account exists is a loop, so
	// the front page becomes the first-run page instead and names the
	// command that produces a developer link.
	//
	// Checked before the session, not after: a developer who is signed
	// in through a one-time link is precisely the person who needs this
	// page, and resolving their principal first would send them to a
	// site list that is empty for a reason they cannot see.
	if s.Store != nil {
		users, err := s.Store.CountUsers(r.Context())
		if err != nil {
			s.logger().Error("panel: count users", "err", err)
			s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
			return
		}
		if users == 0 {
			s.renderSetupNeeded(w, r, lang, http.StatusOK)
			return
		}
	}

	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	s.sitesHandler(w, r, p)
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

// language is the resolved language for a request, falling back to
// negotiating one for handlers reached without the middleware (tests,
// and any future mount that bypasses Handler).
func (s *Server) language(r *http.Request) *ui.Language {
	if lang := ui.LanguageFrom(r); lang != nil {
		return lang
	}
	return s.Renderer.Catalogs().Negotiate(r, s.Language)
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

// ErrNoSessions means the server was built without a session manager.
//
// Refused at startup rather than tolerated. Every method on
// panel.Sessions answers safely for a nil receiver, so a panel in this
// state would run - and would refuse every login, forever, while
// reporting itself healthy. That is precisely the shape of failure this
// project spends its startup checks on: a service that looks fine and
// is not.
var ErrNoSessions = errors.New("panel: no session manager configured")

// ErrNoStore means the server was built without a database.
//
// Refused for the same reason as ErrNoSessions: without one the panel
// can neither authenticate anybody nor read a single setting, and every
// page that tries either panics or renders an error - while the process
// itself reports perfect health. A handler guarding every dereference
// would be a lot of code protecting a state no deployment should reach.
var ErrNoStore = errors.New("panel: no database configured")

// ListenAndServe runs until ctx is cancelled, then drains.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if s.Sessions == nil {
		return ErrNoSessions
	}
	if s.Store == nil {
		return ErrNoStore
	}
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
