// Package api serves a read-only JSON view of the collector's
// traffic_snapshots table, so an external management panel can pull each
// site's statistics over HTTP instead of connecting to the database
// directly. It never writes: every query it issues is a SELECT, and the
// intended deployment gives it a read-only Postgres role (see the
// README).
//
// A note on what this table can honestly answer, since it shapes every
// query below: traffic_snapshots holds one row per (flush interval,
// active IP) - a periodic sample of the collector's in-memory
// sliding-window state, NOT a per-request log. Consecutive samples of the
// same IP overlap heavily (a 60s window sampled every 10s by default), so
// summing curr_window_count across rows would badly overcount.
//
// Every number reported here is therefore exact by construction: distinct
// IP counts, max/avg over the sampled rates, and the peak of the
// collector's own window counters. There is deliberately no cumulative
// "total requests" figure - see Summary.PeakWindowRequests for what
// replaced an earlier attempt at one, and NOTES.md for what an exact
// total would actually require.
package api

import (
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/scoring"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultBotScoreMin is the score at or above which an IP is counted as a
// bot in summaries. A heuristic starting point, not a tuned threshold -
// the same caveat internal/scoring documents for its own constants -
// which is why callers can override it per request.
const DefaultBotScoreMin = 50

// maxRows caps how many rows any "top N" query will return, so a single
// request can't ask for an unbounded response.
const maxRows = 1000

// representativeJA4 and distinctJA4s are how a group of snapshots
// answers "which fingerprint" and "how many".
//
// Every query here groups snapshots by address, and an address is not a
// client. privacy.ip_storage = "masked" stores a /24, so one row can
// stand for 256 machines; even in full mode a household or an office is
// one address. So the fingerprints in a group are genuinely several, and
// that is ordinary rather than exceptional.
//
// It used to be max(ja4) wrapped in a COALESCE, sitting among the
// country and ASN columns under a comment explaining that max() picks a
// stable representative for values that are constant per address. That
// is true of country and ASN, which come from the same lookup every
// flush. It is not true of a TLS fingerprint, which is a property of the
// client. The result: bool_or(is_known_bot_ja4) and max(ja4) were
// independent aggregates over the same group, so the flag and the
// fingerprint printed beside it did not have to come from the same row.
//
// Seen on a live deployment, on the page rather than in a query: an
// address showing score 50 and "known bot", next to the one fingerprint
// in its group that is not in the known-bot set and therefore carries no
// label. Nothing on the page could explain why it had been scored.
//
// The fix is an order, not a new column: prefer a fingerprint that was
// flagged, then the most recent. So whenever the flag is true, the
// fingerprint shown is one that made it true. distinctJA4s is what stops
// the answer from pretending to be the only one - a caller that draws
// the fingerprint can say how many the group had.
// pageTotal is how a paginated breakdown reports its own size.
//
// # Why it is in the same query and not a second one
//
// It used to be a second query: count(DISTINCT <column>) over the raw
// rows, run just before the breakdown. That was two full passes over the
// window instead of one - measured on 11,1M rows over 90 days, 2,93 s
// for the count and 4,16 s for the breakdown - and PostgreSQL answers
// count(DISTINCT x) by *sorting*, which is the expensive form.
//
// But the reason it changed is not the seconds. The two queries were
// counting different things, and the page showed them side by side:
//
//	asn      | the page said 855 | 95 rows could be paged through
//	country  |                 8 | 8
//	ja4      |               380 | 380
//
// The count looked at the raw rows; the breakdown looks at one row per
// address (max(asn), max(country), the representative fingerprint). So
// an address whose ASN resolution changed inside the window contributed
// two values to the count and one to the breakdown - and in this
// product a resolution changes when the range dataset is refreshed
// (D3), which is a feature rather than an anomaly.
//
// A total that is not the total of the thing being paged is worse than a
// slow one: somebody pages to the end and finds empty pages, with no way
// to tell which of the two numbers lied. Now there is one definition,
// and the page and its total cannot disagree because they are one pass.
//
// Window functions run after GROUP BY and before LIMIT, so this counts
// the groups the query would have returned unpaginated - which is
// exactly what a pager needs.
const pageTotal = `count(*) OVER () AS total`

const representativeJA4 = `COALESCE(
	(array_agg(ja4 ORDER BY is_known_bot_ja4 DESC, time DESC) FILTER (WHERE ja4 <> ''))[1], '')`

const distinctJA4s = `count(DISTINCT ja4) FILTER (WHERE ja4 <> '')`

// Store answers read-only questions about traffic_snapshots.
type Store struct {
	pool *pgxpool.Pool
	// knownBots labels JA4 fingerprints on the way out.
	//
	// Carried on the Store rather than read from a package global,
	// because "this deployment never fetched the dataset" is a real and
	// supported state - see internal/botdata. A nil set labels nothing
	// and breaks nothing.
	knownBots scoring.KnownBots
}

// SetKnownBots gives the store the fingerprint labels to use. Safe to
// call once at startup, before serving.
func (s *Store) SetKnownBots(k scoring.KnownBots) { s.knownBots = k }

// KnownBots reports the set in use.
func (s *Store) KnownBots() scoring.KnownBots { return s.knownBots }

// NewStore opens a connection pool to databaseURL and verifies it's
// reachable, the same startup contract as storage.NewWriter. It never
// runs DDL and never writes.
func NewStore(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("api: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("api: ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Close releases the connection pool. Safe to call once.
// Pool exposes the connection pool for work that is not a query this
// package owns - today, the heartbeat row.
//
// Deliberately narrow, and the narrowness is the point: this role holds
// SELECT and nothing else on the analytics tables, so a caller handed
// this pool gains no ability to write anything it could not already.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func (s *Store) Close() {
	s.pool.Close()
}

// Summary is the headline view of one site over one time range.
type Summary struct {
	SiteID string    `json:"site_id"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`

	// UniqueIPs and the bot/human split are exact: they're distinct-IP
	// counts, not derived from the overlapping window counters.
	UniqueIPs int `json:"unique_ips"`
	BotIPs    int `json:"bot_ips"`
	HumanIPs  int `json:"human_ips"`
	// BotScoreMin is the threshold that produced the split above, echoed
	// back so a caller can tell which cutoff a given response used.
	BotScoreMin int `json:"bot_score_min"`

	// PeakRequestRate and AvgRequestRate are in requests/second, taken
	// across the sampled snapshots - exact as sample statistics.
	PeakRequestRate float64 `json:"peak_request_rate"`
	AvgRequestRate  float64 `json:"avg_request_rate"`

	// PeakWindowRequests is the most requests seen across all IPs within a
	// single sliding window (60s by default - cache.window_size_seconds),
	// i.e. the busiest-minute figure. Exact, not an estimate: it reads the
	// collector's own window counters at the single flush where their sum
	// was highest, so no assumption about sampling regularity enters into
	// it.
	//
	// Deliberately not a cumulative "total requests over the range": the
	// windows of consecutive snapshots overlap, so they can't be summed,
	// and reconstructing a total from the sampled rate was tried and
	// measured to undercount a short burst by roughly the
	// window-size:flush-interval ratio. An exact cumulative total needs a
	// monotonic counter the collector doesn't currently keep - see
	// NOTES.md.
	PeakWindowRequests int `json:"peak_window_requests"`

	// Snapshots is how many rows backed this summary, for transparency
	// about how much data the numbers above rest on.
	Snapshots int `json:"snapshots"`
}

// Sites returns every site ID present in the table. Used to answer "what
// can this token see" for wildcard tokens.
func (s *Store) Sites(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT site_id FROM traffic_snapshots WHERE site_id <> '' ORDER BY site_id`)
	if err != nil {
		return nil, fmt.Errorf("api: query sites: %w", err)
	}
	defer rows.Close()

	sites := []string{}
	for rows.Next() {
		var site string
		if err := rows.Scan(&site); err != nil {
			return nil, fmt.Errorf("api: scan site: %w", err)
		}
		sites = append(sites, site)
	}
	return sites, rows.Err()
}

// Summary computes the headline statistics for one site over [from, to).
func (s *Store) Summary(ctx context.Context, siteID string, from, to time.Time, botScoreMin int) (Summary, error) {
	out := Summary{SiteID: siteID, From: from, To: to, BotScoreMin: botScoreMin}

	// The visitor counts, from the raw table, and there is nowhere else
	// they can come from.
	//
	// An IP counts as a bot if *any* snapshot of it in range scored at or
	// above the threshold - a burst that later decays shouldn't erase the
	// fact that it happened.
	//
	// # Why this half is not rolled up, and what that costs
	//
	// count(distinct ip) does not aggregate: two days' visitors are not
	// the sum of each day's. And bot_ips could not be precomputed even
	// if it did, because the threshold arrives in the request - see
	// traffic_rollup's comment in internal/storage/schema.sql.
	//
	// Measured on 12 million rows over 90 days, this endpoint's halves:
	// the counts below 25,36 s, the aggregable figures 1,46 s, the peak
	// window 10,26 s. O2 removed the second and third. This one is what
	// O3 is for, and until then a 90-day summary is still slower than
	// the panel's client will wait.
	err := s.pool.QueryRow(ctx, `
		WITH per_ip AS (
		    SELECT ip, max(bot_score) AS peak_score
		    FROM traffic_snapshots
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY ip
		)
		SELECT (SELECT count(*) FROM per_ip),
		       (SELECT count(*) FROM per_ip WHERE peak_score >= $4)`,
		siteID, from, to, botScoreMin,
	).Scan(&out.UniqueIPs, &out.BotIPs)
	if err != nil {
		return Summary{}, fmt.Errorf("api: summary: %w", err)
	}
	out.HumanIPs = out.UniqueIPs - out.BotIPs

	// The four aggregable figures, from the rollup where it reaches and
	// the raw table for the rest. The watermark is read first and passed
	// in, so the decision about which half answers is taken in one place
	// - see internal/api/rollup.go.
	watermark, err := s.rollupWatermark(ctx, siteID)
	if err != nil {
		return Summary{}, err
	}
	agg, err := s.aggregableOver(ctx, siteID, from, to, watermark)
	if err != nil {
		return Summary{}, err
	}
	out.Snapshots = agg.Snapshots
	out.PeakRequestRate = agg.PeakRequestRate
	out.AvgRequestRate = agg.AvgRequestRate
	out.PeakWindowRequests = agg.PeakWindow

	return out, nil
}

// The peak window is a maximum over flush events: all rows from one
// flush share that flush's exact timestamp, so totalling every active
// IP's window counters per timestamp and taking the largest total is
// exact regardless of how irregularly flushes are spaced - which matters,
// because the collector only writes rows for IPs seen since the previous
// flush, making flush events genuinely irregular whenever traffic is
// bursty.
//
// It used to be its own query here. Since O2 it comes out of
// aggregableOver with the other three aggregable figures, because it is
// one: the maximum of a set of per-flush totals is the maximum of the
// per-bucket maxima of those totals. That is what let it move into the
// rollup, and it was the expensive one - 10,26 s of the 90-day summary's
// 37, against 1,46 s for the rates.
//
// The old single-pass version is still run on every integration test, as
// the oracle the rollup path is checked against rather than as
// production code. See rollup_integration_test.go.

// Bucket is one time slice of a timeseries.
type Bucket struct {
	Time            time.Time `json:"time"`
	UniqueIPs       int       `json:"unique_ips"`
	BotIPs          int       `json:"bot_ips"`
	PeakRequestRate float64   `json:"peak_request_rate"`
	AvgRequestRate  float64   `json:"avg_request_rate"`
}

// Timeseries buckets a site's activity over [from, to). interval must
// already have been validated by ParseInterval and zone by ParseTimezone.
//
// zone is what makes a "day" the customer's day rather than UTC's. See
// ParseTimezone for the measurement that made it necessary.
func (s *Store) Timeseries(ctx context.Context, siteID string, from, to time.Time, interval, zone string, botScoreMin int) ([]Bucket, error) {
	// interval is interpolated through a bound parameter cast to
	// ::interval rather than string-formatted into the SQL, and
	// ParseInterval separately restricts it to a fixed allowlist - so
	// neither injection nor an absurd bucket size is reachable from a
	// request. zone travels the same way.
	rows, err := s.pool.Query(ctx, `
		WITH per_ip_bucket AS (
		    SELECT time_bucket($4::interval, time, $6::text) AS bucket, ip, max(bot_score) AS peak_score
		    FROM traffic_snapshots
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY bucket, ip
		),
		per_bucket_rate AS (
		    SELECT time_bucket($4::interval, time, $6::text) AS bucket,
		           max(request_rate) AS peak_rate,
		           avg(request_rate) AS avg_rate
		    FROM traffic_snapshots
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY bucket
		)
		SELECT b.bucket,
		       count(*) AS unique_ips,
		       count(*) FILTER (WHERE b.peak_score >= $5) AS bot_ips,
		       COALESCE(r.peak_rate, 0),
		       COALESCE(r.avg_rate, 0)
		FROM per_ip_bucket b
		JOIN per_bucket_rate r USING (bucket)
		GROUP BY b.bucket, r.peak_rate, r.avg_rate
		ORDER BY b.bucket`,
		siteID, from, to, interval, botScoreMin, zone,
	)
	if err != nil {
		return nil, fmt.Errorf("api: timeseries: %w", err)
	}
	defer rows.Close()

	buckets := []Bucket{}
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.Time, &b.UniqueIPs, &b.BotIPs, &b.PeakRequestRate, &b.AvgRequestRate); err != nil {
			return nil, fmt.Errorf("api: scan bucket: %w", err)
		}
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}

// IPStat is one IP's activity within a range, as reported by TopIPs.
type IPStat struct {
	IP              string  `json:"ip"`
	PeakScore       int     `json:"peak_score"`
	PeakRequestRate float64 `json:"peak_request_rate"`
	Country         string  `json:"country"`
	ASN             int     `json:"asn"`
	ASNName         string  `json:"asn_name"`
	IsKnownBotJA4   bool    `json:"is_known_bot_ja4"`
	IsKnownBotASN   bool    `json:"is_known_bot_asn"`
	JA4             string  `json:"ja4"`
	// JA4Label is the human-readable bot name for JA4 where it's a
	// recognised fingerprint, so a table can show "Googlebot" rather than
	// a 40-character hash.
	JA4Label string `json:"ja4_label,omitempty"`
	// JA4Count is how many distinct fingerprints this address showed in
	// range. One is the ordinary case; more than one means JA4 above is a
	// representative rather than the answer, and a page drawing it should
	// say so. See representativeJA4.
	JA4Count  int       `json:"ja4_count"`
	LastSeen  time.Time `json:"last_seen"`
	Snapshots int       `json:"snapshots"`
}

// TopIPs returns the highest-scoring IPs for a site over [from, to),
// most suspicious first, alongside the total number of distinct IPs so a
// caller can page through them.
func (s *Store) TopIPs(ctx context.Context, siteID string, from, to time.Time, limit, offset int) ([]IPStat, int, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT ip,
		       max(bot_score),
		       max(request_rate),
		       -- max() over text picks a stable representative value for
		       -- columns that are effectively constant per IP anyway
		       -- (country/ASN come from the same lookup every flush); it
		       -- avoids adding them all to GROUP BY, which would split one
		       -- IP into several rows if a refresh ever changed them
		       -- mid-range.
		       COALESCE(max(country), ''),
		       COALESCE(max(asn), 0),
		       COALESCE(max(asn_org), ''),
		       bool_or(is_known_bot_ja4),
		       bool_or(is_known_bot_asn),
		       `+representativeJA4+`,
		       `+distinctJA4s+`,
		       max(time),
		       count(*),
		       -- The number of addresses this breakdown has, from the same
		       -- pass - see pageTotal. Here the old second query and this
		       -- one agreed (both count distinct addresses, and this query
		       -- groups by address), so the change is the pass that was
		       -- saved rather than a number that was wrong.
		       `+pageTotal+`
		FROM traffic_snapshots
		WHERE site_id = $1 AND time >= $2 AND time < $3
		GROUP BY ip
		ORDER BY max(bot_score) DESC, max(request_rate) DESC, ip
		LIMIT $4 OFFSET $5`,
		siteID, from, to, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("api: top ips: %w", err)
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
			return nil, 0, fmt.Errorf("api: scan ip stat: %w", err)
		}
		stat.IP = ip.String()
		stat.JA4Label, _ = s.knownBots.Label(stat.JA4)
		stats = append(stats, stat)
	}
	return stats, total, rows.Err()
}

// GroupStat is one country's or one ASN's share of a site's traffic.
type GroupStat struct {
	// Key is the country code or the ASN number as a string; Label is a
	// human-readable name where one exists (the ASN's organization), and
	// is empty for countries, whose code is already the label.
	Key       string `json:"key"`
	Label     string `json:"label,omitempty"`
	UniqueIPs int    `json:"unique_ips"`
	BotIPs    int    `json:"bot_ips"`
}

// Countries breaks a site's distinct IPs down by country, busiest first.
// IPs whose country never resolved are grouped under an empty key rather
// than dropped, so the numbers still add up to the site's total.
func (s *Store) Countries(ctx context.Context, siteID string, from, to time.Time, limit, offset, botScoreMin int) ([]GroupStat, int, error) {
	rows, err := s.pool.Query(ctx, `
		WITH per_ip AS (
		    SELECT ip, max(country) AS country, max(bot_score) AS peak_score
		    FROM traffic_snapshots
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY ip
		),
		grouped AS (
		    SELECT country, count(*) AS ips,
		           count(*) FILTER (WHERE peak_score >= $6) AS bot_ips
		    FROM per_ip
		    GROUP BY country
		)
		SELECT country, ips, bot_ips, `+pageTotal+`
		FROM grouped
		ORDER BY ips DESC, country
		LIMIT $4 OFFSET $5`,
		siteID, from, to, limit, offset, botScoreMin,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("api: countries: %w", err)
	}
	defer rows.Close()

	return scanGroupStats(rows, false)
}

// ASNs breaks a site's distinct IPs down by ASN, busiest first.
func (s *Store) ASNs(ctx context.Context, siteID string, from, to time.Time, limit, offset, botScoreMin int) ([]GroupStat, int, error) {
	rows, err := s.pool.Query(ctx, `
		WITH per_ip AS (
		    SELECT ip, max(asn) AS asn, max(asn_org) AS asn_org, max(bot_score) AS peak_score
		    FROM traffic_snapshots
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY ip
		),
		grouped AS (
		    SELECT asn, max(asn_org) AS asn_org, count(*) AS ips,
		           count(*) FILTER (WHERE peak_score >= $6) AS bot_ips
		    FROM per_ip
		    GROUP BY asn
		)
		SELECT asn::text, asn_org, ips, bot_ips, `+pageTotal+`
		FROM grouped
		ORDER BY ips DESC, asn
		LIMIT $4 OFFSET $5`,
		siteID, from, to, limit, offset, botScoreMin,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("api: asns: %w", err)
	}
	defer rows.Close()

	return scanGroupStats(rows, true)
}

// scanGroupStats reads the shared shape Countries and ASNs both return.
// withLabel says whether the result set carries a label column between
// the key and the counts.
func scanGroupStats(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}, withLabel bool) ([]GroupStat, int, error) {
	stats := []GroupStat{}
	// The total rides on every row, because a window function has
	// nowhere else to put it. Every row carries the same value, so the
	// last one read is the answer - and with no rows it stays 0, which
	// is the right total for a breakdown that has no groups.
	var total int
	for rows.Next() {
		var stat GroupStat
		var err error
		if withLabel {
			err = rows.Scan(&stat.Key, &stat.Label, &stat.UniqueIPs, &stat.BotIPs, &total)
		} else {
			err = rows.Scan(&stat.Key, &stat.UniqueIPs, &stat.BotIPs, &total)
		}
		if err != nil {
			return nil, 0, fmt.Errorf("api: scan group stat: %w", err)
		}
		stats = append(stats, stat)
	}
	return stats, total, rows.Err()
}
