package api

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/scoring"
)

// The crossover queries are the ones no single-source analytics tool can
// answer, and the reason both data sources write to one database.
//
// A conventional analytics tool sees only clients that ran its script,
// so it cannot report on the ones that didn't - the traffic is simply
// absent from its numbers, which is why such tools systematically
// under-report automation. A WAF or log analyser sees every connection
// but has no idea which of them rendered a page. Joining
// traffic_snapshots to beacon_events on ip gives both halves at once.
//
// None of these endpoints takes a `bots` filter. The question they ask
// is "did anything from this address execute JavaScript", and filtering
// beacon events by their user agent first would answer a different
// question - a headless browser that ran the snippet really did run it,
// whatever its User-Agent header claims.

// CoverageBand is one bot-score band's JavaScript coverage.
type CoverageBand struct {
	Min int `json:"min"`
	Max int `json:"max"`
	// IPsSeen is how many distinct IPs in this band the collector
	// observed; IPsRanJS is how many of those also sent a beacon event.
	IPsSeen  int `json:"ips_seen"`
	IPsRanJS int `json:"ips_ran_js"`
	// JSCoverage is IPsRanJS/IPsSeen, 0 for an empty band.
	JSCoverage float64 `json:"js_coverage"`
}

// CrossoverSummary reports how much of the traffic that reached the site
// actually executed JavaScript.
type CrossoverSummary struct {
	SiteID string    `json:"site_id"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`

	// IPsSeen counts distinct addresses the collector observed.
	IPsSeen int `json:"ips_seen"`
	// IPsRanJS counts how many of those also sent at least one beacon
	// event, and IPsSilent is the remainder - the population a
	// conventional analytics tool cannot see at all.
	IPsRanJS  int `json:"ips_ran_js"`
	IPsSilent int `json:"ips_silent"`
	// JSCoverage is IPsRanJS/IPsSeen, 0 when nothing was seen.
	JSCoverage float64 `json:"js_coverage"`

	// BeaconOnlyIPs counts addresses that sent beacon events but which
	// the collector never saw. In a correct deployment this is 0, since
	// every browser that loaded the page necessarily connected through
	// the collector first. A non-zero value is a configuration signal
	// worth surfacing rather than hiding: usually the collector is not
	// actually in the path for that traffic, or the beacon's
	// trusted_proxies is wrong and it is recording the proxy's address
	// instead of the visitor's.
	BeaconOnlyIPs int `json:"beacon_only_ips"`

	// Bands breaks coverage down by the collector's bot score. All ten
	// are always present, including empty ones. The expected shape is a
	// downward slope - the more bot-like the score, the less likely the
	// client ran JavaScript - and a high-score band with high coverage is
	// the interesting anomaly: automation sophisticated enough to render
	// pages.
	Bands []CoverageBand `json:"bands"`
}

// CrossoverSummary computes JavaScript coverage for one site over
// [from, to).
func (s *Store) CrossoverSummary(ctx context.Context, siteID string, from, to time.Time) (CrossoverSummary, error) {
	out := CrossoverSummary{SiteID: siteID, From: from, To: to}

	// Shared by both queries below: one row per IP the collector saw,
	// tagged with whether the beacon also heard from it.
	const joinedCTE = `
		WITH collector_ips AS (
		    SELECT ip, max(bot_score) AS peak_score
		    FROM traffic_snapshots
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY ip
		),
		beacon_ips AS (
		    SELECT DISTINCT ip
		    FROM beacon_events
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		),
		joined AS (
		    SELECT c.ip, c.peak_score, (b.ip IS NOT NULL) AS ran_js
		    FROM collector_ips c
		    LEFT JOIN beacon_ips b ON b.ip = c.ip
		)`

	err := s.pool.QueryRow(ctx, joinedCTE+`
		SELECT
		    (SELECT count(*) FROM joined),
		    (SELECT count(*) FROM joined WHERE ran_js),
		    (SELECT count(*) FROM beacon_ips b
		       WHERE NOT EXISTS (SELECT 1 FROM collector_ips c WHERE c.ip = b.ip))`,
		siteID, from, to,
	).Scan(&out.IPsSeen, &out.IPsRanJS, &out.BeaconOnlyIPs)
	if err != nil {
		return CrossoverSummary{}, fmt.Errorf("api: crossover summary: %w", err)
	}
	out.IPsSilent = out.IPsSeen - out.IPsRanJS
	if out.IPsSeen > 0 {
		out.JSCoverage = float64(out.IPsRanJS) / float64(out.IPsSeen)
	}

	rows, err := s.pool.Query(ctx, joinedCTE+`
		-- least(x/10, 9) folds a perfect 100 into the top band rather than
		-- creating an eleventh one holding a single score.
		SELECT least(peak_score / 10, 9) AS band, count(*), count(*) FILTER (WHERE ran_js)
		FROM joined
		GROUP BY band
		ORDER BY band`,
		siteID, from, to,
	)
	if err != nil {
		return CrossoverSummary{}, fmt.Errorf("api: crossover bands: %w", err)
	}
	defer rows.Close()

	type counts struct{ seen, ranJS int }
	found := map[int]counts{}
	for rows.Next() {
		var band, seen, ranJS int
		if err := rows.Scan(&band, &seen, &ranJS); err != nil {
			return CrossoverSummary{}, fmt.Errorf("api: scan crossover band: %w", err)
		}
		found[band] = counts{seen, ranJS}
	}
	if err := rows.Err(); err != nil {
		return CrossoverSummary{}, err
	}

	// Every band emitted, including empty ones, so a chart needn't
	// synthesise gaps - the same contract ScoreDistribution offers.
	out.Bands = make([]CoverageBand, 0, 10)
	for band := range 10 {
		c := found[band]
		bucket := CoverageBand{Min: band * 10, IPsSeen: c.seen, IPsRanJS: c.ranJS}
		bucket.Max = bucket.Min + 9
		if band == 9 {
			bucket.Max = 100
		}
		if c.seen > 0 {
			bucket.JSCoverage = float64(c.ranJS) / float64(c.seen)
		}
		out.Bands = append(out.Bands, bucket)
	}
	return out, nil
}

// SilentIPs lists the addresses the collector saw that never sent a
// beacon event, most suspicious first.
//
// This is the population a conventional analytics tool reports as not
// existing. Most of it is ordinary - feed readers, uptime checks, search
// crawlers, and any visitor with JavaScript disabled - but a scraper
// working through a site is here too, and nowhere else.
func (s *Store) SilentIPs(ctx context.Context, siteID string, from, to time.Time, limit, offset int) ([]IPStat, int, error) {
	const silentCTE = `
		WITH beacon_ips AS (
		    SELECT DISTINCT ip
		    FROM beacon_events
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		),
		silent AS (
		    SELECT *
		    FROM traffic_snapshots t
		    WHERE t.site_id = $1 AND t.time >= $2 AND t.time < $3
		      AND NOT EXISTS (SELECT 1 FROM beacon_ips b WHERE b.ip = t.ip)
		)`

	var total int
	if err := s.pool.QueryRow(ctx, silentCTE+`SELECT count(DISTINCT ip) FROM silent`,
		siteID, from, to,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("api: silent ips total: %w", err)
	}

	rows, err := s.pool.Query(ctx, silentCTE+`
		SELECT ip, max(bot_score), max(request_rate),
		       COALESCE(max(country), ''), COALESCE(max(asn), 0), COALESCE(max(asn_org), ''),
		       bool_or(is_known_bot_ja4), bool_or(is_known_bot_asn),
		       COALESCE(max(ja4), ''), max(time), count(*)
		FROM silent
		GROUP BY ip
		ORDER BY max(bot_score) DESC, max(request_rate) DESC, ip
		LIMIT $4 OFFSET $5`,
		siteID, from, to, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("api: silent ips: %w", err)
	}
	defer rows.Close()

	stats := []IPStat{}
	for rows.Next() {
		var (
			stat IPStat
			ip   netip.Addr
		)
		if err := rows.Scan(&ip, &stat.PeakScore, &stat.PeakRequestRate, &stat.Country, &stat.ASN,
			&stat.ASNName, &stat.IsKnownBotJA4, &stat.IsKnownBotASN, &stat.JA4, &stat.LastSeen, &stat.Snapshots); err != nil {
			return nil, 0, fmt.Errorf("api: scan silent ip: %w", err)
		}
		stat.IP = ip.String()
		stat.JA4Label = scoring.KnownBotJA4[stat.JA4]
		stats = append(stats, stat)
	}
	return stats, total, rows.Err()
}

// JSBot is one address that both executed JavaScript and looks
// automated.
type JSBot struct {
	IP string `json:"ip"`
	// PeakScore is the collector's behavioral score for this IP, 0 if
	// the collector never scored it.
	PeakScore int `json:"peak_score"`
	// IsBotUA reports that its User-Agent self-identified as automation.
	IsBotUA bool `json:"is_bot_ua"`

	Browser string `json:"browser,omitempty"`
	OS      string `json:"os,omitempty"`

	JA4      string `json:"ja4,omitempty"`
	JA4Label string `json:"ja4_label,omitempty"`

	Country string `json:"country,omitempty"`
	ASN     int    `json:"asn,omitempty"`
	ASNName string `json:"asn_name,omitempty"`

	IsKnownBotJA4 bool `json:"is_known_bot_ja4"`
	IsKnownBotASN bool `json:"is_known_bot_asn"`

	Pageviews int `json:"pageviews"`
	Visitors  int `json:"visitors"`

	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

// JSBots lists addresses that ran the beacon snippet *and* either
// self-identified as a bot or were scored at or above botScoreMin by the
// collector.
//
// This is the list that justifies running both sources. A headless
// browser renders pages, executes the snippet and appears in a
// conventional analytics tool as an ordinary visitor - there is nothing
// in the client-side data to distinguish it. What gives it away is the
// other source: a JA4 fingerprint that doesn't match the browser it
// claims to be, a request rate no human produces, or a datacentre ASN.
func (s *Store) JSBots(ctx context.Context, siteID string, from, to time.Time, limit, offset, botScoreMin int) ([]JSBot, int, error) {
	const jsBotsCTE = `
		WITH beacon_agg AS (
		    SELECT ip,
		           count(*) FILTER (WHERE event_type = 'pageview') AS pageviews,
		           count(DISTINCT visitor_id) AS visitors,
		           bool_or(is_bot_ua) AS bot_ua,
		           -- max() picks a stable representative for columns that
		           -- are effectively constant per IP, the same reasoning
		           -- TopIPs documents.
		           max(browser) AS browser,
		           max(os) AS os,
		           min(time) AS first_seen,
		           max(time) AS last_seen
		    FROM beacon_events
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY ip
		),
		collector_agg AS (
		    SELECT ip, max(bot_score) AS peak_score,
		           COALESCE(max(ja4), '') AS ja4,
		           COALESCE(max(country), '') AS country,
		           COALESCE(max(asn), 0) AS asn,
		           COALESCE(max(asn_org), '') AS asn_org,
		           bool_or(is_known_bot_ja4) AS known_ja4,
		           bool_or(is_known_bot_asn) AS known_asn
		    FROM traffic_snapshots
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY ip
		),
		suspects AS (
		    SELECT b.ip, COALESCE(c.peak_score, 0) AS peak_score, b.bot_ua,
		           b.browser, b.os,
		           COALESCE(c.ja4, '') AS ja4, COALESCE(c.country, '') AS country,
		           COALESCE(c.asn, 0) AS asn, COALESCE(c.asn_org, '') AS asn_org,
		           COALESCE(c.known_ja4, false) AS known_ja4,
		           COALESCE(c.known_asn, false) AS known_asn,
		           b.pageviews, b.visitors, b.first_seen, b.last_seen
		    FROM beacon_agg b
		    LEFT JOIN collector_agg c ON c.ip = b.ip
		    WHERE b.bot_ua OR COALESCE(c.peak_score, 0) >= $4
		)`

	var total int
	if err := s.pool.QueryRow(ctx, jsBotsCTE+`SELECT count(*) FROM suspects`,
		siteID, from, to, botScoreMin,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("api: js bots total: %w", err)
	}

	rows, err := s.pool.Query(ctx, jsBotsCTE+`
		SELECT ip, peak_score, bot_ua, browser, os, ja4, country, asn, asn_org,
		       known_ja4, known_asn, pageviews, visitors, first_seen, last_seen
		FROM suspects
		ORDER BY peak_score DESC, pageviews DESC, ip
		LIMIT $5 OFFSET $6`,
		siteID, from, to, botScoreMin, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("api: js bots: %w", err)
	}
	defer rows.Close()

	bots := []JSBot{}
	for rows.Next() {
		var (
			bot JSBot
			ip  netip.Addr
		)
		if err := rows.Scan(&ip, &bot.PeakScore, &bot.IsBotUA, &bot.Browser, &bot.OS, &bot.JA4,
			&bot.Country, &bot.ASN, &bot.ASNName, &bot.IsKnownBotJA4, &bot.IsKnownBotASN,
			&bot.Pageviews, &bot.Visitors, &bot.FirstSeen, &bot.LastSeen); err != nil {
			return nil, 0, fmt.Errorf("api: scan js bot: %w", err)
		}
		bot.IP = ip.String()
		bot.JA4Label = scoring.KnownBotJA4[bot.JA4]
		bots = append(bots, bot)
	}
	return bots, total, rows.Err()
}
