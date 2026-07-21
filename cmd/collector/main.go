// Command collector runs the bot-aware analytics collector. It supports
// two operating modes (see internal/config): "passthrough" (default), a
// content-blind TCP/TLS proxy that never terminates TLS, and "full", a
// TLS-terminating HTTP reverse proxy with real per-request visibility.
// Both fingerprint connections via JA4, track per-IP request rate in
// memory, score it, and periodically flush summaries to TimescaleDB.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/cruciblelab/crucible-analytic/internal/asnlookup"
	"github.com/cruciblelab/crucible-analytic/internal/config"
	"github.com/cruciblelab/crucible-analytic/internal/fullproxy"
	"github.com/cruciblelab/crucible-analytic/internal/limiter"
	"github.com/cruciblelab/crucible-analytic/internal/proxy"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
	"github.com/cruciblelab/crucible-analytic/internal/scoring"
	"github.com/cruciblelab/crucible-analytic/internal/storage"
)

// proxyServer is implemented by both proxy.Server (passthrough) and
// fullproxy.Server (full mode), letting main dispatch on cfg.Mode without
// either package knowing about the other.
type proxyServer interface {
	ListenAndServe(ctx context.Context) error
}

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	configPath := flag.String("config", "config.toml", "path to the TOML config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("config error", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store := ratestore.NewMemoryRateStore(cfg.Cache.WindowSize(), cfg.Cache.TTL(), cfg.Cache.CleanupInterval())

	writer, err := storage.NewWriter(ctx, cfg.Storage.TimescaleDSN)
	if err != nil {
		logger.Error("failed to connect to TimescaleDB", "err", err)
		store.Close()
		os.Exit(1)
	}

	flusher := &storage.Flusher{
		Store:     store,
		Writer:    writer,
		KnownBots: scoring.KnownBotJA4,
		Interval:  cfg.Storage.FlushInterval(),
		Logger:    logger,
	}
	flusherDone := make(chan struct{})
	go func() {
		defer close(flusherDone)
		flusher.Run(ctx)
	}()

	lim := limiter.New(limiter.Config{
		MaxConcurrentConnections: cfg.Limits.MaxConcurrentConnections,
		MaxRequestsPerSecond:     cfg.Limits.MaxRequestsPerSecond,
		Policy:                   limiter.Policy(cfg.Limits.OverloadPolicy),
		ThrottleQueueSize:        cfg.Limits.ThrottleQueueSize,
	})

	// ASN/country lookup is entirely optional and additive - unlike the
	// TimescaleDB connection above, a failure to set it up here doesn't
	// take down the collector's core purpose (proxying, fingerprinting,
	// scoring, storage all work fine without it), so it's logged and
	// skipped rather than fatal.
	var lookup *asnlookup.Resolver
	if cfg.ASNLookup.Enabled {
		lookup, err = asnlookup.NewResolver(ctx, cfg.Storage.TimescaleDSN, asnlookup.CacheConfig{
			MaxEntries: cfg.ASNLookup.CacheMaxEntries,
			TTL:        cfg.ASNLookup.CacheTTL(),
		}, logger)
		if err != nil {
			logger.Error("failed to set up ASN/country lookup, continuing without it", "err", err)
			lookup = nil
		}
	}
	lookupDone := make(chan struct{})
	if lookup != nil {
		go func() {
			defer close(lookupDone)
			lookup.Run(ctx, cfg.ASNLookup.RefreshInterval())
		}()
	} else {
		close(lookupDone)
	}

	var server proxyServer
	switch cfg.Mode {
	case config.ModeFull:
		server = &fullproxy.Server{
			ListenAddr:  cfg.Network.ListenAddr,
			BackendAddr: cfg.Network.BackendAddr,
			CertFile:    cfg.TLS.CertFile,
			KeyFile:     cfg.TLS.KeyFile,
			Store:       store,
			Limiter:     lim,
			DialTimeout: cfg.Network.DialTimeout(),
			Logger:      logger,
		}
	default: // config.ModePassthrough, and validated by config.Load otherwise
		server = &proxy.Server{
			ListenAddr:       cfg.Network.ListenAddr,
			BackendAddr:      cfg.Network.BackendAddr,
			Store:            store,
			Limiter:          lim,
			HandshakeTimeout: cfg.Network.HandshakeTimeout(),
			DialTimeout:      cfg.Network.DialTimeout(),
			Logger:           logger,
		}
	}
	logger.Info("starting collector", "mode", cfg.Mode)

	serveErr := server.ListenAndServe(ctx)

	// Wait for the flusher's own ctx-triggered shutdown flush before
	// closing the writer/store under it, so the last partial interval's
	// activity isn't lost or written to an already-closed pool.
	<-flusherDone
	<-lookupDone
	if lookup != nil {
		lookup.Close()
	}
	writer.Close()
	store.Close()

	if serveErr != nil {
		logger.Error("proxy server error", "err", serveErr)
		os.Exit(1)
	}
	logger.Info("shutdown complete")
}
