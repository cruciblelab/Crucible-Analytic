package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"time"
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

	// KeySpaces says how this window's addresses are keyed, which is
	// what decides whether the numbers above can be compared at all.
	KeySpaces KeySpaces `json:"key_spaces"`
}

// KeySpaces counts each source's join keys by which kind they are.
//
// # Why a crossover summary has to report this
//
// The join keys on COALESCE(ip_hash, inet_send(ip)): the token when the
// row has one, the /24 otherwise. A token is 16 bytes and a network is
// not, so the two never compare equal - a row written in masked mode
// never joins a row written in full mode. That is correct, because
// nothing can tell whether they are the same visitor, and it is not
// free: a window containing both kinds is a window whose coverage is
// lower than the site's really was, by an amount this endpoint cannot
// compute.
//
// It cannot compute it because the missing matches are missing: the
// question "would this token have matched that network" has no answer
// once the address is gone, which is the entire point of storing
// neither. So the honest thing is to report the shape of the keys and
// let the page say what it means - PLAN.md §P5.
//
// # Two different situations, and the same four numbers tell them apart
//
//   - One source holding both kinds: privacy.ip_storage changed inside
//     this window. Expected, temporary, and it ends by itself when
//     retention drops the older side.
//   - Each source holding one kind and not the same kind: the two
//     writers are in different modes *right now*. Not a seam in time but
//     a live misconfiguration - and its symptom is that coverage reads
//     0% and every beacon address lands in BeaconOnlyIPs, which the
//     panel used to explain as "the collector is not in the path". A
//     diagnosis pointing at the network for a defect in a setting.
//
// The verdict is drawn in internal/panel/analytics rather than here:
// there is one consumer, and a rule with two definitions is a rule that
// gets to disagree with itself.
type KeySpaces struct {
	// CollectorTokenised and CollectorNetworkOnly count the collector's
	// distinct join keys; the Beacon pair counts the beacon's. Keys
	// rather than rows, because a key is what the join compares - a
	// thousand rows from one address are one key either way.
	CollectorTokenised   int `json:"collector_tokenised"`
	CollectorNetworkOnly int `json:"collector_network_only"`
	BeaconTokenised      int `json:"beacon_tokenised"`
	BeaconNetworkOnly    int `json:"beacon_network_only"`
}

// CrossoverSummary computes JavaScript coverage for one site over
// [from, to).

// joinKey is the expression every crossover query joins on.
//
// # What the two columns actually hold
//
// This comment described a design that no longer exists until
// 2026-09-02: an either/or, `ip` or `ip_hash`, "never both", with `ip`
// left NULL in the other mode. What is true, per internal/privacy:
//
//   - `ip` always holds the masked network, in both modes. It is never
//     NULL, because no mode stores a raw address and every mode stores
//     that one.
//   - `ip_hash` additionally holds a token of the *whole* address, and
//     only in full mode.
//
// So COALESCE does not pick between two spellings of one thing. It picks
// the sharpest key the row has: the token when there is one, the /24
// otherwise. That is what full mode is for, and inet_send renders the
// fallback as bytea so both branches yield one comparable type.
//
// # The property that survives, and it is the one that matters
//
// A row written in masked mode never joins a row written in full mode.
// The first keys on inet_send(/24), the second on a 16-byte token; the
// encodings differ, so they compare unequal.
//
// That is correct - nothing can tell whether they are the same
// visitor - but it is not free, and it is not visible from here: a
// crossover view over a window that spans a privacy.ip_storage change
// is reading two disjoint key spaces and will report coverage lower than
// it was. Changing the mode is therefore not only a decision about what
// is stored from now on; it puts a seam in this join for as long as
// retention keeps rows from both sides of it. PLAN.md §P5 is where that
// gets said to the customer rather than only here.
const joinKey = `COALESCE(ip_hash, inet_send(ip))`

// # Why every query here groups twice (O4b)
//
// This expression costs a function call and a fresh bytea per row, and
// the rows are the whole window: on the 12M-row set over 30 days it is
// 2,4 s of a 3,4 s grouping - two thirds of the work to produce a key
// there are only 47.500 distinct values of. Grouping by the two columns
// first and forming the key per group instead costs 1,06 s.
//
// The rewrite is an identity rather than an approximation, and the
// reason is what the columns are:
//
//   - Two rows with the same (ip_hash, ip) obviously have the same key.
//   - Two rows with *different* pairs can only share a key if one has a
//     token equal to the other's encoded network, and a token is 16
//     bytes where inet_send gives 8 for an IPv4 /24 and 20 for an IPv6
//     /64. So different pairs are different keys.
//
// Which leaves one direction: one key can, in principle, arrive as two
// pairs - the same token with two masked networks. The product cannot
// write that (privacy.TokenIP hashes the whole address and MaskIP
// derives the network from that same address, so the token determines
// the network), but "cannot happen" is not a thing a query should rely
// on when it need not: the second level re-groups by the key, so such a
// pair would be merged exactly as the one-level form merged it.
//
// That is also the condition on where this rewrite is allowed at all -
// every aggregate in the first level has to survive being merged in the
// second. max, min and bool_or do; count(*) does as sum(); count of a
// DISTINCT column does *not*, which is why JSBots' beacon side still
// groups by the key in one level and says so there.

// collectorPairs is the first level over traffic_snapshots: one row per
// (ip_hash, ip), carrying whatever the caller can merge afterwards.
//
// $1/$2/$3 are the site and the window, in every query that uses it.
func collectorPairs(aggregates string) string {
	return `
		SELECT ip_hash, ip, ` + aggregates + `
		FROM traffic_snapshots
		WHERE site_id = $1 AND time >= $2 AND time < $3
		GROUP BY ip_hash, ip`
}

// pageDetails is what an address list shows *about* the addresses it is
// showing, for those addresses only. It requires a CTE named page with
// join_key and ips columns.
//
// # Why this is a second look at the same window
//
// Because it is the ordered aggregate that costs, not the reading. The
// representative fingerprint sorts each address's rows by flag and
// time, and asking for it while grouping the window made PostgreSQL
// sort *the window* - 3,7M rows spilling 300 MB to disk on the 12M-row
// set over 30 days - to produce a column for 47.500 addresses of which
// a page shows 25.
//
// So the grouping pass now computes only what the page is ordered and
// filtered by, and the columns that are merely displayed are fetched
// afterwards, for the 25 addresses that survived. The window is read
// twice and each read is cheap: measured over 90 days, 16,2 s -> 6,0 s
// against the one-pass-with-everything form, and 35 s before the phase.
//
// # Why it filters on ip rather than on the key
//
// An index exists for the first (idx_traffic_snapshots_ip_time) and no
// expression index exists for the second, but that is the smaller half.
// The larger half is that filtering on the key would evaluate the key
// for every row in the window - which is the cost this phase exists to
// remove. ips carries every network the key was seen with, so the
// filter is a superset and the GROUP BY below decides membership
// exactly.
const pageDetails = `
		SELECT ` + joinKey + ` AS join_key,
		       ` + representativeJA4 + ` AS ja4,
		       ` + distinctJA4s + ` AS ja4_count,
		       COALESCE(max(country), '') AS country,
		       COALESCE(max(asn), 0) AS asn,
		       COALESCE(max(asn_org), '') AS asn_org,
		       bool_or(is_known_bot_ja4) AS known_ja4,
		       bool_or(is_known_bot_asn) AS known_asn
		FROM traffic_snapshots
		WHERE site_id = $1 AND time >= $2 AND time < $3
		  AND ip = ANY (SELECT unnest(ips) FROM page)
		GROUP BY ` + joinKey

func (s *Store) CrossoverSummary(ctx context.Context, siteID string, from, to time.Time) (CrossoverSummary, error) {
	out := CrossoverSummary{SiteID: siteID, From: from, To: to}

	// One row per IP each source saw, tagged with whether the other one
	// saw it too. tokenised rides along on the grouping that is already
	// happening.
	//
	// bool_or rather than a column, because ip_hash is not in the outer
	// GROUP BY - and it need not be: the key already decides the answer,
	// since a group keyed by a token contains only tokenised rows and
	// one keyed by a network only untokenised ones. bool_and would give
	// the same result here, and saying bool_or keeps it true if that
	// ever stops holding.
	joinedCTE := `
		WITH collector_pairs AS (` + collectorPairs(`max(bot_score) AS peak_score`) + `
		),
		collector_ips AS (
		    SELECT ` + joinKey + ` AS join_key, max(peak_score) AS peak_score,
		           bool_or(ip_hash IS NOT NULL) AS tokenised
		    FROM collector_pairs
		    GROUP BY ` + joinKey + `
		),
		beacon_pairs AS (
		    SELECT ip_hash, ip
		    FROM beacon_events
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY ip_hash, ip
		),
		beacon_ips AS (
		    SELECT ` + joinKey + ` AS join_key,
		           bool_or(ip_hash IS NOT NULL) AS tokenised
		    FROM beacon_pairs
		    GROUP BY ` + joinKey + `
		),
		joined AS (
		    SELECT c.join_key, c.peak_score, c.tokenised, (b.join_key IS NOT NULL) AS ran_js
		    FROM collector_ips c
		    LEFT JOIN beacon_ips b ON b.join_key = c.join_key
		),
		-- least(x/10, 9) folds a perfect 100 into the top band rather than
		-- creating an eleventh one holding a single score.
		bands AS (
		    SELECT least(peak_score / 10, 9) AS band, count(*) AS seen,
		           count(*) FILTER (WHERE ran_js) AS ran_js
		    FROM joined
		    GROUP BY 1
		)`

	// # Why the bands come back inside this row rather than from a
	// second query
	//
	// Because the second query rebuilt every CTE above it: two passes
	// over both tables to answer two questions about one population.
	// Measured on the 12M-row set over 90 days, the pass is 3,48 s of
	// this endpoint's time, so asking twice was the larger half of it.
	//
	// And the two passes were two snapshots. A row written between them
	// put a page in front of a reader whose bands did not add up to its
	// own total - rare, unreproducible, and impossible to explain. One
	// statement cannot disagree with itself.
	//
	// json_agg of triples rather than three parallel arrays: an array
	// per column would have to stay aligned by convention, and a
	// convention is what a decoder gets wrong.
	var bandsJSON []byte
	err := s.pool.QueryRow(ctx, joinedCTE+`
		SELECT
		    (SELECT count(*) FROM joined),
		    (SELECT count(*) FROM joined WHERE ran_js),
		    (SELECT count(*) FROM beacon_ips b
		       WHERE NOT EXISTS (SELECT 1 FROM collector_ips c WHERE c.join_key = b.join_key)),
		    -- The four key-space counts, off the same two CTEs: no extra
		    -- pass over either table, which matters because this
		    -- endpoint is already the slowest one here (PLAN.md §O4).
		    (SELECT count(*) FROM joined WHERE tokenised),
		    (SELECT count(*) FROM joined WHERE NOT tokenised),
		    (SELECT count(*) FROM beacon_ips WHERE tokenised),
		    (SELECT count(*) FROM beacon_ips WHERE NOT tokenised),
		    (SELECT COALESCE(json_agg(json_build_array(band, seen, ran_js) ORDER BY band), '[]'::json)
		       FROM bands)`,
		siteID, from, to,
	).Scan(&out.IPsSeen, &out.IPsRanJS, &out.BeaconOnlyIPs,
		&out.KeySpaces.CollectorTokenised, &out.KeySpaces.CollectorNetworkOnly,
		&out.KeySpaces.BeaconTokenised, &out.KeySpaces.BeaconNetworkOnly, &bandsJSON)
	if err != nil {
		return CrossoverSummary{}, fmt.Errorf("api: crossover summary: %w", err)
	}
	out.IPsSilent = out.IPsSeen - out.IPsRanJS
	if out.IPsSeen > 0 {
		out.JSCoverage = float64(out.IPsRanJS) / float64(out.IPsSeen)
	}

	var banded [][3]int
	if err := json.Unmarshal(bandsJSON, &banded); err != nil {
		return CrossoverSummary{}, fmt.Errorf("api: crossover bands: %w", err)
	}
	type counts struct{ seen, ranJS int }
	found := map[int]counts{}
	for _, b := range banded {
		found[b[0]] = counts{b[1], b[2]}
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
	rows, err := s.pool.Query(ctx, `
		WITH beacon_pairs AS (
		    SELECT ip_hash, ip
		    FROM beacon_events
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY ip_hash, ip
		),
		beacon_ips AS (
		    SELECT `+joinKey+` AS join_key FROM beacon_pairs GROUP BY `+joinKey+`
		),
		collector_pairs AS (`+collectorPairs(`max(bot_score) AS peak_score,
		           max(request_rate) AS peak_rate, max(time) AS last_seen,
		           count(*) AS snapshots`)+`
		),
		-- The merge, and the whole reason the first level was allowed:
		-- max of maxima is the max, and a count of counts is their sum.
		collector_ips AS (
		    SELECT `+joinKey+` AS join_key, max(ip) AS ip, max(peak_score) AS peak_score,
		           max(peak_rate) AS peak_rate, max(last_seen) AS last_seen,
		           sum(snapshots)::bigint AS snapshots, array_agg(ip) AS ips
		    FROM collector_pairs
		    GROUP BY `+joinKey+`
		),
		silent AS (
		    SELECT c.* FROM collector_ips c
		    WHERE NOT EXISTS (SELECT 1 FROM beacon_ips b WHERE b.join_key = c.join_key)
		),
		page AS (
		    SELECT *, `+pageTotal+`
		    FROM silent
		    ORDER BY peak_score DESC, peak_rate DESC, ip
		    LIMIT $4 OFFSET $5
		),
		details AS (`+pageDetails+`
		)
		SELECT p.ip, p.peak_score, p.peak_rate,
		       COALESCE(d.country, ''), COALESCE(d.asn, 0), COALESCE(d.asn_org, ''),
		       COALESCE(d.known_ja4, false), COALESCE(d.known_asn, false),
		       COALESCE(d.ja4, ''), COALESCE(d.ja4_count, 0),
		       p.last_seen, p.snapshots, p.total
		FROM page p
		LEFT JOIN details d ON d.join_key = p.join_key
		ORDER BY p.peak_score DESC, p.peak_rate DESC, p.ip`,
		siteID, from, to, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("api: silent ips: %w", err)
	}
	defer rows.Close()

	stats := []IPStat{}
	var total int
	for rows.Next() {
		var (
			stat IPStat
			ip   netip.Addr
		)
		if err := rows.Scan(&ip, &stat.PeakScore, &stat.PeakRequestRate, &stat.Country, &stat.ASN,
			&stat.ASNName, &stat.IsKnownBotJA4, &stat.IsKnownBotASN, &stat.JA4, &stat.JA4Count,
			&stat.LastSeen, &stat.Snapshots, &total); err != nil {
			return nil, 0, fmt.Errorf("api: scan silent ip: %w", err)
		}
		stat.IP = ip.String()
		stat.JA4Label, _ = s.knownBots.Label(stat.JA4)
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
	// JA4Count is how many distinct fingerprints the collector saw from
	// this address in range - see representativeJA4 for why it is not
	// always one, and why JA4 above is chosen rather than arbitrary.
	JA4Count int `json:"ja4_count"`

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
	rows, err := s.pool.Query(ctx, `
		WITH beacon_agg AS (
		    -- The one side that still forms the key per row, because
		    -- count(DISTINCT visitor_id) is the one aggregate here that
		    -- a second level cannot merge: two groups' distinct visitors
		    -- overlap, and summing them counts a visitor twice. The
		    -- reading is cheap by comparison - one row per pageview on
		    -- an uncompressed table, 0,36 s of this query's 6,0 s over
		    -- 90 days on the 12M-row set.
		    SELECT `+joinKey+` AS join_key, max(ip) AS ip,
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
		    GROUP BY `+joinKey+`
		),
		collector_pairs AS (`+collectorPairs(`max(bot_score) AS peak_score`)+`
		),
		collector_scores AS (
		    SELECT `+joinKey+` AS join_key, max(peak_score) AS peak_score,
		           array_agg(ip) AS ips
		    FROM collector_pairs
		    GROUP BY `+joinKey+`
		),
		suspects AS (
		    SELECT b.join_key, b.ip, COALESCE(c.peak_score, 0) AS peak_score, c.ips,
		           b.bot_ua, b.browser, b.os,
		           b.pageviews, b.visitors, b.first_seen, b.last_seen
		    FROM beacon_agg b
		    LEFT JOIN collector_scores c ON c.join_key = b.join_key
		    WHERE b.bot_ua OR COALESCE(c.peak_score, 0) >= $4
		),
		page AS (
		    SELECT *, `+pageTotal+`
		    FROM suspects
		    ORDER BY peak_score DESC, pageviews DESC, ip
		    LIMIT $5 OFFSET $6
		),
		details AS (`+pageDetails+`
		)
		SELECT p.ip, p.peak_score, p.bot_ua, p.browser, p.os,
		       COALESCE(d.ja4, ''), COALESCE(d.ja4_count, 0),
		       COALESCE(d.country, ''), COALESCE(d.asn, 0), COALESCE(d.asn_org, ''),
		       COALESCE(d.known_ja4, false), COALESCE(d.known_asn, false),
		       p.pageviews, p.visitors, p.first_seen, p.last_seen, p.total
		FROM page p
		LEFT JOIN details d ON d.join_key = p.join_key
		ORDER BY p.peak_score DESC, p.pageviews DESC, p.ip`,
		siteID, from, to, botScoreMin, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("api: js bots: %w", err)
	}
	defer rows.Close()

	bots := []JSBot{}
	var total int
	for rows.Next() {
		var (
			bot JSBot
			ip  netip.Addr
		)
		if err := rows.Scan(&ip, &bot.PeakScore, &bot.IsBotUA, &bot.Browser, &bot.OS, &bot.JA4,
			&bot.JA4Count, &bot.Country, &bot.ASN, &bot.ASNName, &bot.IsKnownBotJA4, &bot.IsKnownBotASN,
			&bot.Pageviews, &bot.Visitors, &bot.FirstSeen, &bot.LastSeen, &total); err != nil {
			return nil, 0, fmt.Errorf("api: scan js bot: %w", err)
		}
		bot.IP = ip.String()
		bot.JA4Label, _ = s.knownBots.Label(bot.JA4)
		bots = append(bots, bot)
	}
	return bots, total, rows.Err()
}
