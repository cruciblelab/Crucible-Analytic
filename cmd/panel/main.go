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
	"strings"
	"syscall"

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

	srv := &web.Server{
		ListenAddr: cfg.ListenAddr,
		Renderer:   renderer,
		Logger:     logger,
		HSTS:       cfg.HSTS,
		Zone:       zone,
		Language:   cfg.Language,
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
