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
	"github.com/cruciblelab/crucible-analytic/internal/logging"
)

func main() {
	// Until the config is read, there is nowhere to write but stderr -
	// the file tree is configured by the very file being loaded.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	configPath := flag.String("config", "analytics-api.toml", "path to the TOML config file")
	hashToken := flag.Bool("hash-token", false, "generate a random API token, print it with its SHA-256 hash, and exit")
	flag.Parse()

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
