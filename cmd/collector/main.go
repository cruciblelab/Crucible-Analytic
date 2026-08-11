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

	// ASN/country lookup is entirely optional and additive - unlike the
	// TimescaleDB connection above, a failure to set it up here doesn't
	// take down the collector's core purpose (proxying, fingerprinting,
	// scoring, storage all work fine without it), so it's logged and
	// skipped rather than fatal. Built before the flusher below so it can
	// be wired in as the flusher's row-enrichment source (see the nil
	// check right before constructing flusher).
	var lookup *asnlookup.Resolver
	if cfg.ASNLookup.Enabled {
		lookup, err = asnlookup.NewResolver(ctx, cfg.Storage.TimescaleDSN, asnlookup.CacheConfig{
			MaxEntries: cfg.ASNLookup.CacheMaxEntries,
			TTL:        cfg.ASNLookup.CacheTTL(),
		}, cfg.ASNLookup.LocalCSVPath, logger)
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

	// nil if both lists are empty (the default) - see NewGeoBlocklist.
	// Only actually wired into a server below when lookup is also
	// non-nil, since a blocklist is meaningless without a resolver to
	// check it against.
	geoBlocklist := limiter.NewGeoBlocklist(cfg.ASNLookup.BlockedCountries, cfg.ASNLookup.BlockedASNs)

	// nil unless apply_to_scoring is explicitly on and at least one ASN
	// is configured - map lookups against a nil map are safe and always
	// miss, so scoring.Score doesn't need its own separate on/off switch
	// for this beyond the map being nil or not.
	var knownBotASNs map[int]struct{}
	if cfg.ASNLookup.ApplyToScoring && len(cfg.ASNLookup.KnownBotASNs) > 0 {
		knownBotASNs = make(map[int]struct{}, len(cfg.ASNLookup.KnownBotASNs))
		for _, asn := range cfg.ASNLookup.KnownBotASNs {
			knownBotASNs[asn] = struct{}{}
		}
	}

	flusher := &storage.Flusher{
		Store:     store,
		SiteID:    cfg.SiteID,
		Writer:    writer,
		KnownBots: scoring.KnownBotJA4,
		Interval:  cfg.Storage.FlushInterval(),
		Logger:    logger,
	}
	if lookup != nil {
		// Deliberately guarded rather than an unconditional
		// `flusher.Resolver = lookup`: assigning a nil *asnlookup.Resolver
		// into the storage.GeoResolver interface field would produce a
		// non-nil interface holding a nil pointer, so BuildRows's own
		// `resolver != nil` check would pass and it would call Resolve on
		// a nil receiver - this guard is what keeps flusher.Resolver a
		// true nil interface when lookup is disabled or failed to start.
		flusher.Resolver = lookup
		// knownBotASNs is only meaningful alongside a real resolver (no
		// resolver means every row's ASN is 0, which scoring.Score never
		// matches anyway - see its own zero-value guard), so it's wired
		// here too rather than unconditionally.
		flusher.KnownBotASNs = knownBotASNs
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

	var server proxyServer
	switch cfg.Mode {
	case config.ModeFull:
		srv := &fullproxy.Server{
			ListenAddr:  cfg.Network.ListenAddr,
			BackendAddr: cfg.Network.BackendAddr,
			CertFile:    cfg.TLS.CertFile,
			KeyFile:     cfg.TLS.KeyFile,
			Store:       store,
			Limiter:     lim,
			DialTimeout: cfg.Network.DialTimeout(),
			Logger:      logger,
		}
		if lookup != nil {
			// Same nil-pointer-in-interface guard as flusher.Resolver
			// above: only assign Resolver when lookup is a real,
			// non-nil *asnlookup.Resolver.
			srv.GeoBlocklist = geoBlocklist
			srv.Resolver = lookup
		}
		server = srv
	default: // config.ModePassthrough, and validated by config.Load otherwise
		srv := &proxy.Server{
			ListenAddr:       cfg.Network.ListenAddr,
			BackendAddr:      cfg.Network.BackendAddr,
			Store:            store,
			Limiter:          lim,
			HandshakeTimeout: cfg.Network.HandshakeTimeout(),
			DialTimeout:      cfg.Network.DialTimeout(),
			Logger:           logger,
		}
		if lookup != nil {
			srv.GeoBlocklist = geoBlocklist
			srv.Resolver = lookup
		}
		server = srv
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
