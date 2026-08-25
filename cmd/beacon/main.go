// Command beacon receives client-side analytics events from the
// JavaScript snippet embedded in the sites being measured, and writes
// them to the beacon_events table.
//
// It is the second of this project's two data sources. The collector
// sees every connection but no URLs; the beacon sees URLs, referrers
// and custom events but only from clients that run JavaScript. Neither
// is complete alone, which is the point of running both against one
// database - see internal/beacon's package documentation.
//
// It runs as its own process rather than inside the collector or the
// read API because it is the only part of the system the whole internet
// may POST to, and because it writes: folding it into the collector
// would put attacker-supplied JSON parsing in the traffic path, and
// folding it into the API would end that process's read-only database
// role.
//
// Printing the snippet to embed:
//
//	beacon -snippet https://example.com mysite
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/asnlookup"
	"github.com/cruciblelab/crucible-analytic/internal/beacon"
	"github.com/cruciblelab/crucible-analytic/internal/limiter"
	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/retention"
	"github.com/cruciblelab/crucible-analytic/internal/settings"
)

func main() {
	// Until the config is read, there is nowhere to write but stderr -
	// the file tree is configured by the very file being loaded.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	configPath := flag.String("config", "beacon.toml", "path to the TOML config file")
	snippet := flag.Bool("snippet", false, "print the <script> tag to embed, given a base URL and a site id, then exit")
	flag.Parse()

	if *snippet {
		if err := printSnippet(flag.Args()); err != nil {
			logger.Error("could not print snippet", "err", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := beacon.LoadConfig(*configPath)
	if err != nil {
		logger.Error("config error", "err", err)
		os.Exit(1)
	}

	// Now that the config is known, swap the bootstrap logger for the
	// structured tree. Everything after this point is filed by
	// category and by day; anything before it went to stderr.
	treeLogger, logControls, closeLogs, err := logging.Setup("beacon", cfg.Logging)
	if err != nil {
		logger.Error("logging setup failed", "err", err)
		os.Exit(1)
	}
	defer closeLogs()
	logger = treeLogger
	slog.SetDefault(logger)

	trustedProxies, err := beacon.ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		// Already validated by LoadConfig; re-checked because silently
		// trusting nothing after a parse failure would look like a
		// working deployment recording every visitor as 127.0.0.1.
		logger.Error("invalid trusted_proxies", "err", err)
		os.Exit(1)
	}

	// Fail fast rather than on the first visitor: without working
	// randomness there is no safe visitor ID to derive (see
	// beacon.VisitorIDs.ID), and discovering that at startup is much
	// better than discovering it under traffic.
	visitors, err := beacon.NewVisitorIDs()
	if err != nil {
		logger.Error("could not initialize visitor ids", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	writer, err := beacon.NewWriter(ctx, cfg.TimescaleDSN, beacon.WriterConfig{
		BufferSize:    cfg.Buffer.Size,
		BatchSize:     cfg.Buffer.BatchSize,
		FlushInterval: cfg.Buffer.FlushInterval(),
		Logger:        logger,
	})
	if err != nil {
		logger.Error("failed to connect to TimescaleDB", "err", err)
		os.Exit(1)
	}

	// Optional and additive, exactly as in the collector: a failure here
	// costs country/ASN columns, not event collection, so it is logged
	// and skipped rather than fatal.
	var lookup *asnlookup.Resolver
	if cfg.ASNLookup.Enabled {
		lookup, err = asnlookup.NewResolver(ctx, cfg.TimescaleDSN, asnlookup.CacheConfig{
			MaxEntries: cfg.ASNLookup.CacheMaxEntries,
			TTL:        cfg.ASNLookup.CacheTTL(),
		}, cfg.ASNLookup.LocalCSVPath, logger)
		if err != nil {
			logger.Error("failed to set up ASN/country lookup, continuing without it", "err", err)
			lookup = nil
		} else {
			// Non-negotiable in this process. The collector on the same
			// database TRUNCATEs and rebuilds ip_country_ranges and
			// ip_asn_ranges on its own refresh schedule; a second writer
			// doing the same would repeatedly destroy the first one's
			// data and leave both seeing an empty table mid-load.
			lookup.SkipRangePersistence = true
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

	// The writer outlives the server on purpose: it must keep accepting
	// rows until the last in-flight request has been handled, so it gets
	// its own context that is cancelled only after the server has fully
	// shut down.
	writerCtx, stopWriter := context.WithCancel(context.Background())
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		writer.Run(writerCtx)
	}()

	// Live settings. Optional: a deployment that never grants
	// SELECT on panel_settings simply keeps running on its config file,
	// which is why the failure here is logged rather than fatal.
	settingsCtx, stopSettings := context.WithCancel(context.Background())
	live := settings.New(settingsCtx, writer.Pool(), settings.Config{
		Interval: cfg.Settings.Interval(),
		Logger:   logger,
	})

	srv := &beacon.Server{
		ListenAddr:     cfg.ListenAddr,
		PathPrefix:     cfg.PathPrefix,
		Sites:          cfg.Sites,
		Sink:           writer,
		Visitors:       visitors,
		ClientIP:       beacon.ClientIPResolver{TrustedProxies: trustedProxies},
		AllowedOrigins: cfg.AllowedOrigins,
		Campaign:       cfg.Campaign.Policy(),
		IPMode:         cfg.Privacy.IPMode(),
		IPHashKey:      cfg.Privacy.HashKey(),
		Logger:         logger,
	}
	if lookup != nil {
		// Guarded, not unconditional: assigning a nil *asnlookup.Resolver
		// into the interface field would leave a non-nil interface
		// holding a nil pointer, and Server's own `Resolver != nil` check
		// would then call Resolve on it. Same trap as in the collector's
		// flusher wiring.
		srv.Resolver = lookup
	}
	// Always constructed, where it used to be built only when the file
	// asked for a limit. A limiter with no limits admits everything, so
	// this costs an atomic load per request - and the alternative is
	// that a deployment which never wrote [limits] into its file cannot
	// be given a limit from the panel during the incident where it turns
	// out to need one.
	srv.Limiter = limiter.New(cfg.Limits.LiveLimits(nil))

	// Seeded from the config so the first applySettings call reports the
	// mode the process is actually starting on, rather than reporting a
	// change from nothing.
	lastIPMode := cfg.Privacy.IPMode()
	logger.Info("ip storage mode", "mode", lastIPMode.String())

	// Apply once before serving, then on the source's own interval. The
	// static config is the fallback for every value, so a settings table
	// that is empty or unreachable changes nothing.
	// Seeded the same way, so the first apply reports what the process
	// is starting on rather than a change from nothing.
	lastProxies := trustedProxies
	lastLimits := cfg.Limits.LiveLimits(nil)

	applySettings := func() {
		srv.SetCampaignPolicy(beacon.CampaignPolicy(cfg.Campaign.Live(live)))
		srv.SetSites(live.Strings(settings.KeyBeaconSites, "", cfg.Sites))

		// Trusted proxies. Logged on change, at Info, because getting
		// this wrong is the single most consequential misconfiguration
		// this service has - and somebody reading the log after the
		// numbers went strange should find the moment it changed.
		if prefixes, err := cfg.LiveTrustedProxies(live); err != nil {
			// The stored list is unusable, so the one in force stays in
			// force. Warned every interval rather than once: this is a
			// state somebody has to fix, and a single line at startup is
			// a line nobody reads.
			logger.Warn("settings: trusted_proxies is not usable, keeping the current list",
				"err", err, "in_force", len(lastProxies))
		} else if !samePrefixes(prefixes, lastProxies) {
			logger.Info("trusted proxies changed",
				"from", len(lastProxies), "to", len(prefixes), "networks", prefixes)
			lastProxies = prefixes
			srv.SetTrustedProxies(prefixes)
		}

		// Admission limits. The policy is the one that can take a site
		// down, so a change to it is logged with both values.
		if limits := cfg.Limits.LiveLimits(live); limits != lastLimits {
			logger.Info("limits changed",
				"policy_from", string(lastLimits.Policy), "policy_to", string(limits.Policy),
				"max_concurrent", limits.MaxConcurrentConnections,
				"max_per_second", limits.MaxRequestsPerSecond,
				"throttle_queue", limits.ThrottleQueueSize)
			lastLimits = limits
			srv.Limiter.SetConfig(limits)
		}

		// Logged on change rather than every minute: this decides what
		// personal data the process writes, so a silent switch would be
		// the one change nobody could account for afterwards.
		if mode := cfg.Privacy.Live(live); mode != lastIPMode {
			logger.Info("ip storage mode changed", "from", lastIPMode.String(), "to", mode.String())
			lastIPMode = mode
		}
		srv.SetIPMode(lastIPMode)

		// The log level, and any temporary raise to debug. The raise
		// expires by itself: "turn on debug, reproduce it, turn it off"
		// is the one log setting a support call actually reaches for, and
		// leaving it on is how a disk fills.
		configured, err := logging.ParseLevel(
			live.String(settings.KeyLogLevel, "", cfg.Logging.Level, []string{"debug", "info", "warn", "error"}))
		if err != nil {
			configured = logControls.Base()
		}
		before := logControls.Level()
		after := logControls.Apply(configured, live.String(settings.KeyLogVerboseUntil, "", "", nil), time.Now())
		if after != before {
			// Logged at the new level's own severity would risk being
			// invisible; Info is always written and this is rare.
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
					logger.Warn("settings: read failed, keeping the last known values",
						"err", err, "consecutive_failures", live.Failures())
					continue
				}
				applySettings()
			}
		}
	}()

	// Retention. Its own goroutine on its own, much slower schedule:
	// applying the policy is idempotent, but a site keeping less than the
	// deployment gets a row-level delete each pass, and running that
	// every minute would scan for nothing sixty times an hour.
	//
	// A failure here is logged, not fatal. Without a policy the table
	// grows, which is a problem measured in weeks; refusing to accept
	// events is a problem measured in seconds.
	retentionDone := make(chan struct{})
	go func() {
		defer close(retentionDone)

		manager, err := retention.NewManager(writer.Pool(), retention.TableBeaconEvents)
		if err != nil {
			logger.Error("retention: not configured", "err", err)
			return
		}
		apply := func() {
			// From the file, and only the file. The ticker below still
			// earns its place: the policy no longer changes under a
			// running process, but the row-level trim for sites keeping
			// less than the deployment has to keep running, because what
			// changes hourly is the data, not the rule.
			policy := cfg.RetentionPolicy()
			report, err := manager.Apply(settingsCtx, policy)
			if err != nil {
				logger.Warn("retention: could not apply", "err", err, logging.In(logging.CategoryError))
				return
			}
			if report.PolicyChanged {
				logger.Info("retention policy changed", "table", string(report.Table),
					"from_days", report.PreviousDays, "to_days", report.PolicyDays)
			}
			if rows := report.Rows(); rows > 0 {
				// Said out loud: this is the path that deletes rows rather
				// than dropping chunks, and how much it removed is the
				// number somebody will ask about.
				logger.Info("retention: trimmed sites keeping less than the deployment",
					"rows", rows, "sites", len(report.SiteRows))
			}
		}
		apply()

		ticker := time.NewTicker(cfg.Retention.Interval())
		defer ticker.Stop()
		for {
			select {
			case <-settingsCtx.Done():
				return
			case <-ticker.C:
				apply()
			}
		}
	}()

	serveErr := srv.ListenAndServe(ctx)
	stopSettings()
	<-retentionDone
	<-settingsDone

	// Only now that no new event can arrive: drain the buffer, then let
	// go of the connection pool the drain needs.
	stopWriter()
	<-writerDone
	<-lookupDone
	writer.Close()
	if lookup != nil {
		lookup.Close()
	}

	accepted, dropped, rejected := srv.Counters()
	written, writerDropped := writer.Counters()
	logger.Info("shutdown complete",
		"accepted", accepted, "dropped_by_server", dropped, "rejected", rejected,
		"written", written, "dropped_total", writerDropped)

	if serveErr != nil {
		logger.Error("beacon server error", "err", serveErr)
		os.Exit(1)
	}
}

// printSnippet emits the exact tag to paste into a site's HTML. The
// endpoint is derived from the script's own URL at runtime (see
// beacon.js), so the base URL is the only thing that has to be right.
func printSnippet(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: beacon -snippet <base-url> <site-id>\n  e.g. beacon -snippet https://example.com mysite")
	}
	base, site := strings.TrimRight(args[0], "/"), args[1]

	fmt.Printf("<script defer src=%q data-site=%q></script>\n", base+beacon.DefaultPathPrefix+"/ca.js", site)
	fmt.Println()
	fmt.Println("Serve that path from the site's own origin - forward")
	fmt.Printf("  %s/\n", beacon.DefaultPathPrefix)
	fmt.Println("to this process from whatever terminates TLS for the site. Same-origin")
	fmt.Println("keeps it out of the URL patterns content blockers match on, and means")
	fmt.Println("no CORS is involved at all.")
	return nil
}

// samePrefixes compares two trusted-proxy lists.
//
// By value and in order, because the settings source hands back a fresh
// slice every interval and comparing the slices themselves would report
// a change every minute - which would fill the log with the one line
// somebody needs to be able to find.
func samePrefixes(a, b []netip.Prefix) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
