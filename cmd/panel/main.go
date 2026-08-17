// Command panel serves the management dashboard: the part of Crucible
// Analytic a customer logs into.
//
// It is its own process, with its own database role, and that role has
// no access at all to the analytics tables. The panel reads traffic
// numbers over HTTP from the read-only API, exactly as an external
// panel would. That keeps the component the whole internet can reach
// from also being the component with broad database rights.
//
// Everything the browser loads - the stylesheet, htmx, every template
// and every string - is compiled into this binary. There is no CDN, no
// npm and no build step: deploying the panel is copying one file and
// one config.
//
//	panel -config panel.toml
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
	"github.com/cruciblelab/crucible-analytic/internal/panel/web"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always)" ./cmd/panel
//
// Empty is honest rather than a made-up default: an unstamped build
// shows no version in the footer instead of claiming one.
var version string

func main() {
	// Until the config is read there is nowhere to write but stderr -
	// the log tree is configured by the very file being loaded.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	configPath := flag.String("config", "panel.toml", "path to the TOML config file")
	devLink := flag.Bool("dev-link", false,
		"mint a one-time developer access link, print it, and exit")
	devReason := flag.String("dev-reason", "kurulum",
		"why this developer link is being requested; recorded in the audit log")
	baseURL := flag.String("base-url", "",
		"the address the panel is reached at, for the printed link (default http://<listen_addr>)")
	flag.Parse()

	cfg, err := web.LoadConfig(*configPath)
	if err != nil {
		fatal(nil, "config error", err)
	}

	treeLogger, _, closeLogs, err := logging.Setup("panel", cfg.Logging)
	if err != nil {
		fatal(nil, "logging setup failed", err)
	}
	defer closeLogs()
	logger = treeLogger
	slog.SetDefault(logger)

	// The rendering layer is built before anything else that can fail
	// slowly. A missing catalog key, an unparsable template or a
	// stylesheet that did not get embedded all stop the process here,
	// with a message naming the file - rather than at the first request
	// for a page nobody opens for a week.
	catalogs, err := ui.LoadCatalogs()
	if err != nil {
		fatal(logger, "language packs", err)
	}
	assets, err := ui.LoadAssets()
	if err != nil {
		fatal(logger, "static assets", err)
	}
	renderer, err := ui.New(catalogs, assets, logger)
	if err != nil {
		fatal(logger, "templates", err)
	}
	renderer.Version = version

	zone, err := cfg.Location()
	if err != nil {
		fatal(logger, "timezone", err)
	}
	renderer.SetZone(zone)

	// The configured language must actually exist, or the deployment
	// would silently run in Turkish while the config file named another.
	if cfg.Language != "" && catalogs.ByCode(cfg.Language) == nil {
		fatal(logger, "language", fmt.Errorf("panel: language = %q is not one of the packs in this build (%s)",
			cfg.Language, strings.Join(languageCodes(catalogs), ", ")))
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Connect now rather than on the first page that needs a row. A
	// panel that starts against an unreachable database looks healthy
	// until somebody logs in, and then reports a failure that has
	// nothing to do with what they were doing. NewStore pings; it never
	// runs DDL - the schema is applied once, separately, by whoever
	// installed the deployment.
	store, err := panel.NewStore(ctx, cfg.PanelDSN)
	if err != nil {
		fatal(logger, "panel database", err)
	}
	defer store.Close()

	// Minting a link is a one-shot command rather than a running
	// service, and it happens here rather than in a separate binary
	// because it needs this config's database and nothing else. The
	// person running it is at a shell on the server, which is precisely
	// the authority the link stands for.
	if *devLink {
		if err := printDevLink(ctx, store, cfg, *baseURL, *devReason); err != nil {
			fatal(logger, "developer link", err)
		}
		return
	}

	gate, err := devgate.New(cfg.DeveloperGate, devgate.Options{
		Logger: logger,
		Audit:  store.GateAudit(),
	})
	if err != nil {
		fatal(logger, "developer password", err)
	}

	sessions := panel.NewSessions(store, cfg.SessionLifetime(), cfg.CookiesAreSecure())

	srv := &web.Server{
		ListenAddr:       cfg.ListenAddr,
		Renderer:         renderer,
		Logger:           logger,
		HSTS:             cfg.HSTS,
		Zone:             zone,
		Language:         cfg.Language,
		Store:            store,
		Sessions:         sessions,
		Gate:             gate,
		ConfigPath:       *configPath,
		ConfigFileValues: configFileValues(cfg),
		Preflight: panel.PreflightConfig{
			LogDir:      cfg.Logging.Dir,
			DataDir:     cfg.Logging.Dir,
			BotDataPath: cfg.BotDataPath,
		},
	}
	if err := srv.ListenAndServe(ctx); err != nil {
		fatal(logger, "panel server error", err)
	}
	logger.Info("shutdown complete")
}

// fatal reports a startup failure to both the log tree and stderr, then
// exits.
//
// Both, because the two readers are different people at different
// times. The log tree is for whoever looks afterwards; stderr is for the
// operator who just typed the command and is watching the terminal. Once
// logging.Setup has swapped the logger, everything goes to a file - so a
// panel that cannot reach its database would exit 1 having printed
// nothing at all, which is the single most confusing way for a service
// to refuse to start.
//
// A nil logger means the tree does not exist yet, which is the window
// before logging.Setup has run. In that window the bootstrap logger is
// stderr, so logging as well would print the same failure twice to the
// same terminal.
func fatal(logger *slog.Logger, what string, err error) {
	if logger != nil {
		logger.Error(what, "err", err)
	}
	fmt.Fprintf(os.Stderr, "panel: %s: %v\n", what, err)
	os.Exit(1)
}

// languageCodes lists what this build carries, for the error message
// that says a configured language is not one of them.
func languageCodes(cats *ui.Catalogs) []string {
	var codes []string
	for _, lang := range cats.Languages() {
		codes = append(codes, lang.Code)
	}
	return codes
}

// printDevLink mints a one-time developer access link and writes it to
// stdout.
//
// Stdout and not the log tree: this is the one output of this program a
// person copies with their mouse, and burying it in a JSON line beside
// the day's requests would be hostile. The token is not recoverable
// afterwards - only its hash is stored - so it is printed once and
// never again.
func printDevLink(ctx context.Context, store *panel.Store, cfg web.Config, baseURL, reason string) error {
	token, req, err := store.RequestDevAccess(ctx, reason, 0, 0)
	if err != nil {
		return err
	}
	if baseURL == "" {
		baseURL = "http://" + cfg.ListenAddr
	}
	baseURL = strings.TrimSuffix(baseURL, "/")

	fmt.Println(baseURL + web.DevAccessPathPrefix + token)
	fmt.Println()
	fmt.Printf("Tek kullanımlık. %s tarihine kadar geçerli.\n", req.RequestExpiresAt.Format("2006-01-02 15:04:05 MST"))
	if req.AutoApproved {
		// Said out loud because it is the security property that
		// matters most here, and because it stops being true silently.
		fmt.Println("Bu kurulumda henüz hesap yok, bu yüzden bağlantı kendiliğinden onaylandı.")
		fmt.Println("İlk hesap oluşturulduğu anda bu otomatik onay biter; sonrasında sahibin onayı gerekir.")
	} else {
		fmt.Println("Bu kurulumun bir sahibi var, bu yüzden bağlantı onaylanana kadar çalışmaz.")
	}
	return nil
}

// configFileValues is what the wizard may display from the config
// files.
//
// Only the panel's own values, because the panel reads only its own
// file. The collector's and the beacon's entries in the registry stay
// unknown, and the page says "not reported to the panel" rather than
// leaving a blank - which is the same distinction between "we looked"
// and "we did not look" that the preflight checks keep.
//
// Nothing secret is listed. panel.ConfigFileSettings drops secrets on
// the entry itself, so this cannot leak one by passing the wrong key,
// but the DSN is left out here too rather than relying on that.
func configFileValues(cfg web.Config) map[string]string {
	values := map[string]string{
		"panel.listen_addr":            cfg.ListenAddr,
		"panel.timezone":               cfg.Timezone,
		"panel.language":               cfg.Language,
		"panel.analytics_api_url":      cfg.AnalyticsAPIURL,
		"panel.logging.dir":            cfg.Logging.Dir,
		"panel.session_lifetime_hours": strconv.Itoa(cfg.SessionLifetimeHours),
		"panel.hsts":                   strconv.FormatBool(cfg.HSTS),
		"panel.secure_cookies":         strconv.FormatBool(cfg.CookiesAreSecure()),
	}
	for key, value := range values {
		if value == "" {
			delete(values, key)
		}
	}
	return values
}
