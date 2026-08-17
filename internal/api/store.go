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

	// One pass for the exact statistics. An IP counts as a bot if *any*
	// snapshot of it in range scored at or above the threshold - a burst
	// that later decays shouldn't erase the fact that it happened.
	err := s.pool.QueryRow(ctx, `
		WITH per_ip AS (
		    SELECT ip, max(bot_score) AS peak_score
		    FROM traffic_snapshots
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY ip
		)
		SELECT
		    (SELECT count(*) FROM per_ip),
		    (SELECT count(*) FROM per_ip WHERE peak_score >= $4),
		    COALESCE(max(request_rate), 0),
		    COALESCE(avg(request_rate), 0),
		    count(*)
		FROM traffic_snapshots
		WHERE site_id = $1 AND time >= $2 AND time < $3`,
		siteID, from, to, botScoreMin,
	).Scan(&out.UniqueIPs, &out.BotIPs, &out.PeakRequestRate, &out.AvgRequestRate, &out.Snapshots)
	if err != nil {
		return Summary{}, fmt.Errorf("api: summary: %w", err)
	}
	out.HumanIPs = out.UniqueIPs - out.BotIPs

	peak, err := s.peakWindowRequests(ctx, siteID, from, to)
	if err != nil {
		return Summary{}, err
	}
	out.PeakWindowRequests = peak

	return out, nil
}

// peakWindowRequests finds the busiest single sliding window in the
// range: for each flush event (all rows from one flush share that flush's
// exact timestamp), it totals every active IP's window counters, then
// takes the largest such total.
//
// This reads counters the collector already maintains rather than
// reconstructing anything, so it's exact regardless of how irregularly
// flushes happen to be spaced - which matters, because the collector only
// writes rows for IPs seen since the previous flush, making flush events
// genuinely irregular whenever traffic is bursty.
func (s *Store) peakWindowRequests(ctx context.Context, siteID string, from, to time.Time) (int, error) {
	var peak int
	err := s.pool.QueryRow(ctx, `
		WITH per_flush AS (
		    SELECT time, sum(prev_window_count + curr_window_count) AS window_requests
		    FROM traffic_snapshots
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY time
		)
		SELECT COALESCE(max(window_requests), 0) FROM per_flush`,
		siteID, from, to,
	).Scan(&peak)
	if err != nil {
		return 0, fmt.Errorf("api: peak window requests: %w", err)
	}
	return peak, nil
}

// Bucket is one time slice of a timeseries.
type Bucket struct {
	Time            time.Time `json:"time"`
	UniqueIPs       int       `json:"unique_ips"`
	BotIPs          int       `json:"bot_ips"`
	PeakRequestRate float64   `json:"peak_request_rate"`
	AvgRequestRate  float64   `json:"avg_request_rate"`
}

// Timeseries buckets a site's activity over [from, to). interval must
// already have been validated by ParseInterval.
func (s *Store) Timeseries(ctx context.Context, siteID string, from, to time.Time, interval string, botScoreMin int) ([]Bucket, error) {
	// interval is interpolated through a bound parameter cast to
	// ::interval rather than string-formatted into the SQL, and
	// ParseInterval separately restricts it to a fixed allowlist - so
	// neither injection nor an absurd bucket size is reachable from a
	// request.
	rows, err := s.pool.Query(ctx, `
		WITH per_ip_bucket AS (
		    SELECT time_bucket($4::interval, time) AS bucket, ip, max(bot_score) AS peak_score
		    FROM traffic_snapshots
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY bucket, ip
		),
		per_bucket_rate AS (
		    SELECT time_bucket($4::interval, time) AS bucket,
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
		siteID, from, to, interval, botScoreMin,
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
	JA4Label  string    `json:"ja4_label,omitempty"`
	LastSeen  time.Time `json:"last_seen"`
	Snapshots int       `json:"snapshots"`
}

// TopIPs returns the highest-scoring IPs for a site over [from, to),
// most suspicious first, alongside the total number of distinct IPs so a
// caller can page through them.
func (s *Store) TopIPs(ctx context.Context, siteID string, from, to time.Time, limit, offset int) ([]IPStat, int, error) {
	total, err := s.countDistinct(ctx, `ip`, siteID, from, to)
	if err != nil {
		return nil, 0, err
	}

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
		       COALESCE(max(ja4), ''),
		       max(time),
		       count(*)
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
	for rows.Next() {
		var (
			stat IPStat
			ip   netip.Addr
		)
		if err := rows.Scan(&ip, &stat.PeakScore, &stat.PeakRequestRate, &stat.Country, &stat.ASN,
			&stat.ASNName, &stat.IsKnownBotJA4, &stat.IsKnownBotASN, &stat.JA4, &stat.LastSeen, &stat.Snapshots); err != nil {
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
	total, err := s.countDistinct(ctx, `country`, siteID, from, to)
	if err != nil {
		return nil, 0, err
	}

	rows, err := s.pool.Query(ctx, `
		WITH per_ip AS (
		    SELECT ip, max(country) AS country, max(bot_score) AS peak_score
		    FROM traffic_snapshots
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY ip
		)
		SELECT country, count(*), count(*) FILTER (WHERE peak_score >= $6)
		FROM per_ip
		GROUP BY country
		ORDER BY count(*) DESC, country
		LIMIT $4 OFFSET $5`,
		siteID, from, to, limit, offset, botScoreMin,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("api: countries: %w", err)
	}
	defer rows.Close()

	stats, err := scanGroupStats(rows, false)
	return stats, total, err
}

// ASNs breaks a site's distinct IPs down by ASN, busiest first.
func (s *Store) ASNs(ctx context.Context, siteID string, from, to time.Time, limit, offset, botScoreMin int) ([]GroupStat, int, error) {
	total, err := s.countDistinct(ctx, `asn`, siteID, from, to)
	if err != nil {
		return nil, 0, err
	}

	rows, err := s.pool.Query(ctx, `
		WITH per_ip AS (
		    SELECT ip, max(asn) AS asn, max(asn_org) AS asn_org, max(bot_score) AS peak_score
		    FROM traffic_snapshots
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY ip
		)
		SELECT asn::text, max(asn_org), count(*), count(*) FILTER (WHERE peak_score >= $6)
		FROM per_ip
		GROUP BY asn
		ORDER BY count(*) DESC, asn
		LIMIT $4 OFFSET $5`,
		siteID, from, to, limit, offset, botScoreMin,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("api: asns: %w", err)
	}
	defer rows.Close()

	stats, err := scanGroupStats(rows, true)
	return stats, total, err
}

// scanGroupStats reads the shared shape Countries and ASNs both return.
// withLabel says whether the result set carries a label column between
// the key and the counts.
func scanGroupStats(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}, withLabel bool) ([]GroupStat, error) {
	stats := []GroupStat{}
	for rows.Next() {
		var stat GroupStat
		var err error
		if withLabel {
			err = rows.Scan(&stat.Key, &stat.Label, &stat.UniqueIPs, &stat.BotIPs)
		} else {
			err = rows.Scan(&stat.Key, &stat.UniqueIPs, &stat.BotIPs)
		}
		if err != nil {
			return nil, fmt.Errorf("api: scan group stat: %w", err)
		}
		stats = append(stats, stat)
	}
	return stats, rows.Err()
}
