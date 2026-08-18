// Command collector runs the bot-aware analytics collector. It supports
// two operating modes (see internal/collector): "passthrough" (default), a
// content-blind TCP/TLS proxy that never terminates TLS, and "full", a
// TLS-terminating HTTP reverse proxy with real per-request visibility.
// Both fingerprint connections via JA4, track per-IP request rate in
// memory, score it, and periodically flush summaries to TimescaleDB.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/asnlookup"
	"github.com/cruciblelab/crucible-analytic/internal/botdata"
	"github.com/cruciblelab/crucible-analytic/internal/collector"
	"github.com/cruciblelab/crucible-analytic/internal/fullproxy"
	"github.com/cruciblelab/crucible-analytic/internal/limiter"
	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/proxy"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
	"github.com/cruciblelab/crucible-analytic/internal/retention"
	"github.com/cruciblelab/crucible-analytic/internal/scoring"
	"github.com/cruciblelab/crucible-analytic/internal/settings"
	"github.com/cruciblelab/crucible-analytic/internal/storage"
)

// proxyServer is implemented by both proxy.Server (passthrough) and
// fullproxy.Server (full mode), letting main dispatch on cfg.Mode without
// either package knowing about the other.
type proxyServer interface {
	ListenAndServe(ctx context.Context) error
}

func main() {
	// Until the config is read, there is nowhere to write but stderr -
	// the file tree is configured by the very file being loaded.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	configPath := flag.String("config", "collector.toml", "path to the TOML config file")
	updateBotData := flag.Bool("update-bot-data", false,
		"fetch the known-bot fingerprint set into bot_data.path and exit (put this in cron)")
	flag.Parse()

	cfg, err := collector.Load(*configPath)
	if err != nil {
		logger.Error("config error", "err", err)
		os.Exit(1)
	}

	// Now that the config is known, swap the bootstrap logger for the
	// structured tree. Everything after this point is filed by
	// category and by day; anything before it went to stderr.
	treeLogger, logControls, closeLogs, err := logging.Setup("collector", cfg.Logging)
	if err != nil {
		logger.Error("logging setup failed", "err", err)
		os.Exit(1)
	}
	defer closeLogs()
	logger = treeLogger
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Updating the fingerprint set is a one-shot command, not part of
	// starting the collector. It is here rather than in a separate
	// binary because it needs this config's paths and nothing else, and
	// because the deployment schedules it however it likes - cron, by
	// hand, or from somewhere else entirely.
	if *updateBotData {
		if err := runBotDataUpdate(ctx, cfg, logger); err != nil {
			logger.Error("bot data update failed", "err", err)
			fmt.Fprintf(os.Stderr, "collector: bot data update failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// Loaded before anything starts listening. A deployment that has
	// never fetched gets the empty set and a line saying so: the
	// known-bot signal is absent, every other signal still works, and
	// nobody has to discover that from a dashboard six weeks later.
	knownBots, botMeta := loadBotData(cfg, logger)
	_ = botMeta

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

	// Always a real blocklist, empty or not, because the lists can be
	// replaced from the panel while this process runs (A5.2). Only wired
	// into a server below when lookup is also non-nil, since a blocklist
	// is meaningless without a resolver to check it against - and the
	// servers ask Active() per connection, so an empty one costs an
	// atomic load rather than a geography lookup.
	geoBlocklist := limiter.NewGeoBlocklist(cfg.ASNLookup.BlockedCountries, cfg.ASNLookup.BlockedASNs)

	// nil unless apply_to_scoring is explicitly on and at least one ASN
	// is configured - map lookups against a nil map are safe and always
	// miss, so scoring.Score doesn't need its own separate on/off switch
	// for this beyond the map being nil or not.
	knownBotASNs := cfg.ASNLookup.LiveKnownBotASNs(nil)

	flusher := &storage.Flusher{
		Store:     store,
		SiteID:    cfg.SiteID,
		Writer:    writer,
		KnownBots: knownBots,
		Interval:  cfg.Storage.FlushInterval(),
		Logger:    logger,
		IPMode:    cfg.Privacy.IPMode(),
		IPHashKey: cfg.Privacy.HashKey(),
	}
	// Said out loud at startup, because it decides what personal data
	// this process writes and it is the one setting nobody will
	// remember choosing. An operator reading the first ten lines of the
	// log should be able to answer "does this store whole addresses".
	logger.Info("ip storage mode", "mode", flusher.IPMode.String())
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
		//
		// The live setter below (applySettings) does call
		// SetKnownBotASNs unconditionally, which looks like a
		// disagreement and is not: 0 cannot be a key by either route -
		// collector.Load rejects a non-positive entry in the file and
		// the settings validator rejects one in the panel - so a list
		// applied without a resolver matches nothing, exactly as this
		// guard arranges by omission.
		flusher.KnownBotASNs = knownBotASNs
	}
	// Retention, on its own slow schedule. A failure is logged rather
	// than fatal: without a policy the table grows, which is a problem
	// measured in weeks, while refusing to start is a problem measured
	// in seconds - and this process is in the traffic path.
	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)

		manager, err := retention.NewManager(writer.Pool(), retention.TableTrafficSnapshots)
		if err != nil {
			logger.Error("retention: not configured", "err", err)
			return
		}
		apply := func() {
			report, err := manager.Apply(ctx, retention.Policy{Days: cfg.Retention.Resolved()})
			if err != nil {
				logger.Warn("retention: could not apply", "err", err, logging.In(logging.CategoryError))
				return
			}
			if report.PolicyChanged {
				logger.Info("retention policy changed", "table", string(report.Table),
					"from_days", report.PreviousDays, "to_days", report.PolicyDays)
			}
		}
		apply()

		ticker := time.NewTicker(cfg.Retention.Interval())
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				apply()
			}
		}
	}()

	flusherDone := make(chan struct{})
	go func() {
		defer close(flusherDone)
		flusher.Run(ctx)
	}()

	// Live settings. New in A5.1: until then this process read its config
	// file and nothing else, so the panel could offer a setting the
	// collector would never see - and the two tables this system writes
	// were configured from two different places.
	//
	// Optional, exactly as in the beacon: a deployment that never runs
	// GRANT SELECT ON panel_settings keeps working from its file, which
	// is why a failure here is logged rather than fatal.
	settingsCtx, stopSettings := context.WithCancel(context.Background())
	live := settings.New(settingsCtx, writer.Pool(), settings.Config{
		Interval: cfg.Settings.Interval(),
		Logger:   logger,
	})

	lim := limiter.New(cfg.Limits.LiveLimits(nil))

	// Seeded from the file so the first apply reports what the process is
	// starting on rather than a change from nothing.
	lastLimits := cfg.Limits.LiveLimits(nil)
	lastCountries, lastASNs := cfg.ASNLookup.LiveBlocklist(nil)
	lastBotASNs := knownBotASNs

	// # Every setting below is applied on every poll, and compared only
	// to decide whether to say so
	//
	// The obvious shape is the other way round - compare, and apply only
	// on a change - and that is how this was written. It put the
	// correctness of a live setting behind the accuracy of its own
	// change detector, and the known-bot ASN list got that detector
	// wrong: it compared lengths, on the reasoning that diffing a map
	// every poll was not worth a log line. True about the log line, and
	// wrong about everything else, because swapping one ASN for another
	// keeps the length. The new list would have sat in the database,
	// shown in the panel as the current value, and never reached
	// scoring.
	//
	// Applying unconditionally costs one atomic store per setting per
	// poll (SetConfig, Set and SetKnownBotASNs are all a single store
	// over freshly built values - none of them touches a counter, a
	// semaphore or a queue, so re-applying an unchanged value is not
	// observable to traffic in flight). A comparison that is now only
	// ever wrong about whether to log is a comparison that cannot break
	// a security setting.
	applySettings := func() {
		limits := cfg.Limits.LiveLimits(live)
		lim.SetConfig(limits)
		if limits != lastLimits {
			// Logged at Info with both policies. This is the one setting
			// here that can stop traffic reaching the customer's site,
			// and somebody reading the log afterwards should find the
			// moment it changed without having to infer it.
			logger.Info("limits changed",
				"policy_from", string(lastLimits.Policy), "policy_to", string(limits.Policy),
				"max_concurrent", limits.MaxConcurrentConnections,
				"max_per_second", limits.MaxRequestsPerSecond,
				"throttle_queue", limits.ThrottleQueueSize)
			lastLimits = limits
		}

		// The blocklist. Logged at Info for the same reason as the
		// limits and more so: this one refuses traffic outright, and
		// "when did we start blocking that country" is a question
		// somebody asks days later.
		countries, blockedASNs := cfg.ASNLookup.LiveBlocklist(live)
		geoBlocklist.Set(countries, blockedASNs)
		if !slices.Equal(countries, lastCountries) || !slices.Equal(blockedASNs, lastASNs) {
			logger.Info("geo blocklist changed",
				"countries", len(countries), "asns", len(blockedASNs),
				"was_countries", len(lastCountries), "was_asns", len(lastASNs))
			lastCountries, lastASNs = countries, blockedASNs
		}

		// The scoring signal.
		botASNs := cfg.ASNLookup.LiveKnownBotASNs(live)
		flusher.SetKnownBotASNs(botASNs)
		if !maps.Equal(botASNs, lastBotASNs) {
			logger.Info("known-bot ASN list changed", "asns", len(botASNs), "was", len(lastBotASNs))
			lastBotASNs = botASNs
		}

		// The log level, and any temporary raise to debug. Live in the
		// beacon since A6 and discarded here until A5.2 - the collector
		// built its Controls and threw them away, so the one process a
		// support call most often needs verbose was the one that could
		// not be turned up without a restart.
		configured, err := logging.ParseLevel(
			live.String(settings.KeyLogLevel, "", cfg.Logging.Level,
				[]string{"debug", "info", "warn", "error"}))
		if err != nil {
			configured = logControls.Base()
		}
		before := logControls.Level()
		after := logControls.Apply(configured, live.String(settings.KeyLogVerboseUntil, "", "", nil), time.Now())
		if after != before {
			logger.Info("logging level changed", "from", before.String(), "to", after.String())
		}
	}
	applySettings()

	settingsDone := make(chan struct{})
	go func() {
		defer close(settingsDone)
		ticker := time.NewTicker(cfg.Settings.Interval())
		defer ticker.Stop()
		for {
			select {
			case <-settingsCtx.Done():
				return
			case <-ticker.C:
				if err := live.Refresh(settingsCtx); err != nil {
					// The last known values stay in force. Resetting to
					// defaults during a database blip would silently undo
					// a customer's tuning, which is worse than running on
					// a stale value and much harder to notice.
					logger.Warn("settings: read failed, keeping the last known values",
						"err", err, "consecutive_failures", live.Failures())
					continue
				}
				applySettings()
			}
		}
	}()

	var server proxyServer
	switch cfg.Mode {
	case collector.ModeFull:
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
	default: // collector.ModePassthrough, and validated by collector.Load otherwise
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
	stopSettings()
	<-settingsDone
	<-flusherDone
	<-retentionDone
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

// runBotDataUpdate fetches the known-bot fingerprint set and writes it
// to the configured path.
//
// This project ships no copy of that dataset: it belongs to somebody
// else, and a permissively licensed repository carrying third-party data
// under unstated terms hands that uncertainty to everyone who clones it.
// The deployment retrieves it here, onto its own machine, under the
// source's own terms.
func runBotDataUpdate(ctx context.Context, cfg *collector.Config, logger *slog.Logger) error {
	if cfg.BotData.Path == "" {
		return fmt.Errorf("bot_data.path is not set; there is nowhere to write the file")
	}
	fetchCtx, cancel := context.WithTimeout(ctx, botdata.FetchTimeout)
	defer cancel()

	set, err := botdata.Update(fetchCtx, nil, cfg.BotData.SourceURL, cfg.BotData.Path)
	if err != nil {
		return err
	}
	logger.Info("bot data updated",
		"path", cfg.BotData.Path, "source", set.Source,
		"fingerprints", set.Len(), "dropped", set.Dropped)
	// Also to stdout: whoever ran this is at a shell or reading cron
	// mail, and a line in the log tree is not where they are looking.
	fmt.Printf("%d fingerprints written to %s (source: %s, %d entries filtered out)\n",
		set.Len(), cfg.BotData.Path, set.Source, set.Dropped)
	return nil
}

// loadBotData reads the fingerprint set at startup.
//
// A missing file is not a failure - it is a deployment that has not run
// the update yet, which is an ordinary state. What would be a failure is
// letting that pass unremarked, so the absence is logged as plainly as
// the presence.
func loadBotData(cfg *collector.Config, logger *slog.Logger) (scoring.KnownBots, botdata.Set) {
	set, err := botdata.Load(cfg.BotData.Path)
	if err != nil {
		// A file that exists and cannot be read is different from no
		// file, and is worth a warning rather than a silent empty set.
		logger.Warn("bot data could not be read; continuing without the known-bot signal",
			"path", cfg.BotData.Path, "err", err)
		return nil, botdata.Empty()
	}
	switch {
	case cfg.BotData.Path == "":
		logger.Info("bot data not configured; the known-bot signal is off",
			"how", "set bot_data.path and run: collector -update-bot-data")
	case !set.Fetched():
		logger.Info("bot data has never been fetched; the known-bot signal is off",
			"path", cfg.BotData.Path,
			"how", "run: collector -config <file> -update-bot-data")
	default:
		logger.Info("bot data loaded",
			"path", cfg.BotData.Path, "fingerprints", set.Len(),
			"fetched_at", set.FetchedAt.Format(time.RFC3339), "source", set.Source)
	}
	return scoring.KnownBots(set.Labels), set
}
