// Command collector runs the bot-aware analytics collector: a TCP/TLS
// passthrough proxy that fingerprints connections via JA4, tracks per-IP
// request rate in memory, scores it, and periodically flushes summaries to
// TimescaleDB.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cruciblelab/crucible-analytic/internal/config"
	"github.com/cruciblelab/crucible-analytic/internal/proxy"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
	"github.com/cruciblelab/crucible-analytic/internal/scoring"
	"github.com/cruciblelab/crucible-analytic/internal/storage"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("config error", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store := ratestore.NewMemoryRateStore(cfg.WindowSize, cfg.IdleTTL, cfg.CleanupInterval)

	writer, err := storage.NewWriter(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("failed to connect to TimescaleDB", "err", err)
		store.Close()
		os.Exit(1)
	}

	flusher := &storage.Flusher{
		Store:     store,
		Writer:    writer,
		KnownBots: scoring.KnownBotJA4,
		Interval:  cfg.FlushInterval,
		Logger:    logger,
	}
	flusherDone := make(chan struct{})
	go func() {
		defer close(flusherDone)
		flusher.Run(ctx)
	}()

	server := &proxy.Server{
		ListenAddr:       cfg.ListenAddr,
		BackendAddr:      cfg.BackendAddr,
		Store:            store,
		HandshakeTimeout: cfg.HandshakeTimeout,
		DialTimeout:      cfg.DialTimeout,
		Logger:           logger,
	}

	serveErr := server.ListenAndServe(ctx)

	// Wait for the flusher's own ctx-triggered shutdown flush before
	// closing the writer/store under it, so the last partial interval's
	// activity isn't lost or written to an already-closed pool.
	<-flusherDone
	writer.Close()
	store.Close()

	if serveErr != nil {
		logger.Error("proxy server error", "err", serveErr)
		os.Exit(1)
	}
	logger.Info("shutdown complete")
}
