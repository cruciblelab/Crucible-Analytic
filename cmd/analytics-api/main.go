// Command analytics-api serves a read-only JSON view of the collector's
// traffic_snapshots table, so an external management panel can pull each
// site's statistics over HTTP rather than connecting to the database
// itself.
//
// It runs as its own process, separate from the collector, for three
// reasons: one server may run several collectors (one per site) but needs
// only one API; the API should be reachable on its own port without the
// collector's traffic-path process also being a web service; and giving
// it a read-only database role is only meaningful if it isn't sharing a
// process with the writer.
//
// Generating a token:
//
//	analytics-api -hash-token
//
// prints a fresh random token and its SHA-256 hash. Put the hash in the
// config, give the token to the caller, and keep no copy of the token
// itself - the config only ever needs the hash.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cruciblelab/crucible-analytic/internal/api"
	"github.com/cruciblelab/crucible-analytic/internal/botdata"
	"github.com/cruciblelab/crucible-analytic/internal/buildinfo"
	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/scoring"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always)" ./cmd/analytics-api
//
// Left empty when it was not: internal/buildinfo falls back to the commit
// Go embeds into every build made from a working tree, so an unstamped
// binary still answers "which build is this" with something true.
var version string

func main() {
	// Until the config is read, there is nowhere to write but stderr -
	// the file tree is configured by the very file being loaded.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	configPath := flag.String("config", "analytics-api.toml", "path to the TOML config file")
	showVersion := flag.Bool("version", false, "print the build version and exit")
	hashToken := flag.Bool("hash-token", false, "generate a random API token, print it with its SHA-256 hash, and exit")
	flag.Parse()

	// Before the config is read, and before anything can fail: this is the
	// question asked when a process will not start, so it must not need a
	// working installation to answer.
	if *showVersion {
		buildinfo.Print(os.Stdout, "analytics-api", version)
		return
	}

	if *hashToken {
		if err := printNewToken(); err != nil {
			logger.Error("failed to generate token", "err", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := api.LoadConfig(*configPath)
	if err != nil {
		logger.Error("config error", "err", err)
		os.Exit(1)
	}

	// Now that the config is known, swap the bootstrap logger for the
	// structured tree. Everything after this point is filed by
	// category and by day; anything before it went to stderr.
	treeLogger, logControls, closeLogs, err := logging.Setup("analytics-api", cfg.Logging)
	if err != nil {
		logger.Error("logging setup failed", "err", err)
		os.Exit(1)
	}
	defer closeLogs()
	logger = treeLogger
	slog.SetDefault(logger)
	_ = logControls

	auth, err := api.NewAuthenticator(cfg.TokenList())
	if err != nil {
		logger.Error("invalid tokens", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := api.NewStore(ctx, cfg.TimescaleDSN)
	if err != nil {
		logger.Error("failed to connect to TimescaleDB", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	// The API labels JA4 fingerprints in its responses from the same
	// file the collector reads. Optional: without it the fingerprints
	// are still reported, just unlabelled - which is honest, and far
	// better than shipping somebody else's dataset inside a repository
	// that says MIT. See internal/botdata.
	botSet, err := botdata.Load(cfg.BotDataPath)
	if err != nil {
		logger.Warn("bot data could not be read; fingerprints will be unlabelled",
			"path", cfg.BotDataPath, "err", err)
	}
	store.SetKnownBots(scoring.KnownBots(botSet.Labels))
	if botSet.Fetched() {
		logger.Info("bot data loaded", "fingerprints", botSet.Len(), "source", botSet.Source)
	} else {
		logger.Info("bot data not present; JA4 fingerprints will be reported without labels",
			"how", "run: collector -config <file> -update-bot-data")
	}

	srv := &api.Server{
		ListenAddr: cfg.ListenAddr,
		Store:      store,
		Auth:       auth,
		Logger:     logger,
	}
	if err := srv.ListenAndServe(ctx); err != nil {
		logger.Error("api server error", "err", err)
		os.Exit(1)
	}
	logger.Info("shutdown complete")
}

// printNewToken generates a 32-byte random token and prints it alongside
// its hash. Writing both to stdout is deliberate: the operator needs the
// token exactly once, to hand to the caller, and the hash to paste into
// the config - nothing persists it.
func printNewToken() error {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return fmt.Errorf("reading random bytes: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	fmt.Println("token (give this to the caller, it is not recoverable later):")
	fmt.Println("  " + token)
	fmt.Println()
	fmt.Println("sha256 (put this in the config's [[tokens]] entry):")
	fmt.Println("  " + api.HashToken(token))
	return nil
}
