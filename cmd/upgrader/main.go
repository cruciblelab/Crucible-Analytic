// Command upgrader applies schema migrations the panel has asked for.
//
// # Why this is a sixth binary
//
// The other five each hold exactly the database rights their job needs,
// and none of them may run DDL. That is not an omission - B6 and H5
// established it, and release/sql/grants.sql contains no ALTER, no
// CREATE and no OWNER for any service role. An upgrade button that
// migrated the database from inside the panel would have to undo it.
//
// So the panel writes a row saying what it wants, and this reads the
// row, applies the schema, and writes back what happened. It is the only
// component in the deployment with the authority, it runs as its own
// user against its own configuration file, and the file it reads is the
// only one carrying a DSN that can change the shape of the database.
//
// # Why a timer and not a daemon
//
// Nothing to serve and nothing to keep warm. systemd runs it, it looks
// once, and it exits - so a machine at rest has no process holding a
// connection with DDL rights open, and a crash mid-migration is a unit
// that failed rather than a service in an unknown state. The stale-claim
// release in internal/applier is what recovers the row afterwards.
//
// Run with -once to do a single pass, which is what the timer uses, or
// with no flag to poll on the configured interval, which is what the
// Docker entry point uses where there is no timer.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/applier"
	"github.com/cruciblelab/crucible-analytic/internal/buildinfo"
	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/logsink"
	"github.com/cruciblelab/crucible-analytic/internal/relupdate"
	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
	"github.com/cruciblelab/crucible-analytic/internal/upgrade"
)

// version is set at build time; see release/build.sh.
var version = ""

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	configPath := flag.String("config", "upgrader.toml", "path to the TOML config file")
	once := flag.Bool("once", false, "check for a request, act on it, and exit")
	showVersion := flag.Bool("version", false, "print the build version and exit")
	schemaVersion := flag.Bool("schema-version", false,
		"print the schema version and fingerprint this build carries, then exit")
	flag.Parse()

	if *showVersion {
		// buildinfo.Print, like the other five. This printed a bare
		// version string until it was measured against them: support's
		// first question is which process, and a line that answers only
		// "which build" is the defect G2 fixed for four binaries and
		// this one reintroduced as a sixth.
		buildinfo.Print(os.Stdout, "upgrader", version)
		return
	}
	if *schemaVersion {
		// The same two lines the panel prints, so an operator comparing a
		// panel and an upgrader that disagree can see it in one glance -
		// which is exactly the situation that makes the applier refuse.
		fmt.Printf("version %d\nfingerprint %s\n", schemaver.Version, schemaver.Fingerprint)
		return
	}

	cfg, err := applier.Load(*configPath)
	if err != nil {
		logger.Error("upgrader: config", "err", err)
		os.Exit(1)
	}

	treeLogger, logControls, closeLogs, err := logging.Setup("upgrader", cfg.Logging)
	if err != nil {
		logger.Error("upgrader: logging setup failed", "err", err)
		os.Exit(1)
	}
	defer closeLogs()
	logger = treeLogger
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.SchemaAdminDSN)
	if err != nil {
		logger.Error("upgrader: database", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		logger.Error("upgrader: database unreachable", "err", err,
			"hint", "schema_admin_dsn must name the fifth role; install.sh creates it")
		os.Exit(1)
	}

	// The panel's copy of this component's log lines. An upgrade is the
	// one operation a customer watches a page for, so the lines it
	// produces have to reach the page rather than only a file the
	// customer cannot read.
	logger, panelLog := logsink.Attach(logger, pool, logControls)
	defer panelLog.Close()
	slog.SetDefault(logger)

	name, err := os.Hostname()
	if err != nil || name == "" {
		name = "upgrader"
	}
	a := &applier.Applier{Pool: pool, Logger: logger, Name: name}

	// The "is there a newer version" check rides on the same loop.
	//
	// Same process rather than a second timer unit, because it is the
	// same question of authority: this is the only component holding the
	// public key, so it is the only one that can turn "the server said
	// v0.21.0" into something a panel may show. See
	// internal/relupdate/manifest.go.
	//
	// Its own cadence, though. The loop below runs every thirty seconds
	// so a queued request is picked up while somebody is still looking
	// at the page; the release check is due every six hours, and Due
	// reads that from the recorded row rather than from a timer here -
	// so restarting the service does not re-check, and a machine that
	// restarts twenty times during a deployment still asks once.
	checker := relupdate.Checker{
		Pool:   pool,
		Logger: logger,
		Source: relupdate.Source{
			BaseURL:   cfg.Release.BaseURL,
			PublicKey: cfg.Release.Key(),
		},
	}

	if *once {
		if err := runOnce(ctx, a, checker, logger); err != nil {
			os.Exit(1)
		}
		return
	}

	logger.Info("upgrader: watching for requests", "interval", cfg.Interval())
	ticker := time.NewTicker(cfg.Interval())
	defer ticker.Stop()
	_ = runOnce(ctx, a, checker, logger)
	for {
		select {
		case <-ctx.Done():
			logger.Info("upgrader: stopping")
			return
		case <-ticker.C:
			_ = runOnce(ctx, a, checker, logger)
		}
	}
}

// runOnce does one pass and reports whether it failed.
//
// "Nothing waiting" is not a failure and says nothing: it is what almost
// every run reports, and a line each time would bury the runs that
// matter under a log of runs that did not.
func runOnce(ctx context.Context, a *applier.Applier, checker relupdate.Checker,
	logger *slog.Logger) error {

	// The check first, and its failure does not stop the pass.
	//
	// They are independent jobs sharing a tick: a release host that is
	// down must not stop a queued schema upgrade from being applied,
	// and the upgrade is the one somebody is waiting on. RunOnce has
	// already recorded and logged whatever went wrong.
	_ = checker.RunOnce(ctx)

	req, err := a.RunOnce(ctx)
	switch {
	case errors.Is(err, upgrade.ErrNothingToDo):
		return nil
	case err != nil:
		// Already recorded against the request row by RunOnce, and
		// already logged there with the request id. This line is for the
		// operator reading journalctl, who has no row in front of them.
		logger.Error("upgrader: the upgrade did not finish", "err", err)
		return err
	}
	logger.Info("upgrader: done", "request", req.ID, "state", req.State)
	return nil
}
