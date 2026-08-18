package api

import (
	"context"
	"fmt"
	"net/netip"
	"time"
)

// SiteOverview is one site's headline numbers within a multi-site
// overview.
type SiteOverview struct {
	SiteID          string    `json:"site_id"`
	UniqueIPs       int       `json:"unique_ips"`
	BotIPs          int       `json:"bot_ips"`
	HumanIPs        int       `json:"human_ips"`
	PeakRequestRate float64   `json:"peak_request_rate"`
	AvgRequestRate  float64   `json:"avg_request_rate"`
	Snapshots       int       `json:"snapshots"`
	LastSeen        time.Time `json:"last_seen"`
}

// Overview summarises several sites at once, so a management panel's
// landing page can render every customer it manages from a single request
// rather than fanning out one call per site.
//
// sites restricts the result; passing nil means "every site in the
// database" and is only reached for a wildcard token. Sites with no
// traffic in the range are simply absent from the result rather than
// returned as zeroed rows - the caller already knows which sites it asked
// about, and inventing rows would imply the site exists when it may not.
func (s *Store) Overview(ctx context.Context, sites []string, from, to time.Time, botScoreMin int) ([]SiteOverview, error) {
	// $3 is either a text[] of allowed sites or NULL; `($3::text[] IS NULL
	// OR site_id = ANY($3))` then means "no restriction" or "one of
	// these", without needing two near-identical queries.
	rows, err := s.pool.Query(ctx, `
		WITH scoped AS (
		    SELECT * FROM traffic_snapshots
		    WHERE time >= $1 AND time < $2
		      AND ($3::text[] IS NULL OR site_id = ANY($3))
		),
		per_ip AS (
		    SELECT site_id, ip, max(bot_score) AS peak_score
		    FROM scoped GROUP BY site_id, ip
		),
		ip_counts AS (
		    SELECT site_id,
		           count(*) AS unique_ips,
		           count(*) FILTER (WHERE peak_score >= $4) AS bot_ips
		    FROM per_ip GROUP BY site_id
		),
		rate_stats AS (
		    SELECT site_id,
		           max(request_rate) AS peak_rate,
		           avg(request_rate) AS avg_rate,
		           count(*) AS snapshots,
		           max(time) AS last_seen
		    FROM scoped GROUP BY site_id
		)
		SELECT c.site_id, c.unique_ips, c.bot_ips, r.peak_rate, r.avg_rate, r.snapshots, r.last_seen
		FROM ip_counts c JOIN rate_stats r USING (site_id)
		ORDER BY c.unique_ips DESC, c.site_id`,
		from, to, sites, botScoreMin,
	)
	if err != nil {
		return nil, fmt.Errorf("api: overview: %w", err)
	}
	defer rows.Close()

	out := []SiteOverview{}
	for rows.Next() {
		var o SiteOverview
		if err := rows.Scan(&o.SiteID, &o.UniqueIPs, &o.BotIPs, &o.PeakRequestRate, &o.AvgRequestRate, &o.Snapshots, &o.LastSeen); err != nil {
			return nil, fmt.Errorf("api: scan overview: %w", err)
		}
		o.HumanIPs = o.UniqueIPs - o.BotIPs
		out = append(out, o)
	}
	return out, rows.Err()
}

// JA4Stat is one TLS fingerprint's share of a site's traffic.
type JA4Stat struct {
	JA4 string `json:"ja4"`
	// Label is the human-readable name from scoring.KnownBotJA4 where the
	// fingerprint is a recognised bot ("Googlebot", a scraping tool, ...),
	// empty otherwise. Resolved here rather than stored per row because
	// the mapping is build-time data that can be refreshed independently
	// of history already written.
	Label         string `json:"label,omitempty"`
	IsKnownBotJA4 bool   `json:"is_known_bot_ja4"`
	UniqueIPs     int    `json:"unique_ips"`
	BotIPs        int    `json:"bot_ips"`
	// Empty is true for the group of traffic that carried no usable
	// fingerprint at all - plaintext HTTP, or a ClientHello that couldn't
	// be parsed in time. Kept as its own group rather than dropped so the
	// numbers still add up to the site's total.
	Empty bool `json:"empty,omitempty"`
}

// JA4s breaks a site's distinct IPs down by TLS fingerprint, busiest
// first - the view that's specific to what this collector does, as
// opposed to country/ASN, which any IP-geolocation tool could produce.
func (s *Store) JA4s(ctx context.Context, siteID string, from, to time.Time, limit, offset, botScoreMin int) ([]JA4Stat, int, error) {
	total, err := s.countDistinct(ctx, countJA4, siteID, from, to)
	if err != nil {
		return nil, 0, err
	}

	rows, err := s.pool.Query(ctx, `
		WITH per_ip AS (
		    SELECT ip, max(ja4) AS ja4, bool_or(is_known_bot_ja4) AS known_bot, max(bot_score) AS peak_score
		    FROM traffic_snapshots
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY ip
		)
		SELECT ja4, bool_or(known_bot), count(*), count(*) FILTER (WHERE peak_score >= $6)
		FROM per_ip
		GROUP BY ja4
		ORDER BY count(*) DESC, ja4
		LIMIT $4 OFFSET $5`,
		siteID, from, to, limit, offset, botScoreMin,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("api: ja4s: %w", err)
	}
	defer rows.Close()

	stats := []JA4Stat{}
	for rows.Next() {
		var stat JA4Stat
		if err := rows.Scan(&stat.JA4, &stat.IsKnownBotJA4, &stat.UniqueIPs, &stat.BotIPs); err != nil {
			return nil, 0, fmt.Errorf("api: scan ja4 stat: %w", err)
		}
		stat.Empty = stat.JA4 == ""
		stat.Label, _ = s.knownBots.Label(stat.JA4)
		stats = append(stats, stat)
	}
	return stats, total, rows.Err()
}

// ScoreBucket is one 10-point band of the bot-score histogram.
type ScoreBucket struct {
	Min       int `json:"min"`
	Max       int `json:"max"`
	UniqueIPs int `json:"unique_ips"`
}

// ScoreDistribution buckets a site's distinct IPs by their peak bot score
// into fixed 10-point bands, for a histogram. Every band from 0-9 up to
// 90-100 is always present, including empty ones, so a chart doesn't have
// to synthesise the gaps itself.
func (s *Store) ScoreDistribution(ctx context.Context, siteID string, from, to time.Time) ([]ScoreBucket, error) {
	rows, err := s.pool.Query(ctx, `
		WITH per_ip AS (
		    SELECT ip, max(bot_score) AS peak_score
		    FROM traffic_snapshots
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY ip
		)
		-- least(...,9) folds a score of exactly 100 into the top band
		-- rather than creating an eleventh bucket holding only that value.
		SELECT least(peak_score / 10, 9) AS band, count(*)
		FROM per_ip GROUP BY band`,
		siteID, from, to,
	)
	if err != nil {
		return nil, fmt.Errorf("api: score distribution: %w", err)
	}
	defer rows.Close()

	counts := make(map[int]int, 10)
	for rows.Next() {
		var band, n int
		if err := rows.Scan(&band, &n); err != nil {
			return nil, fmt.Errorf("api: scan score bucket: %w", err)
		}
		counts[band] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	buckets := make([]ScoreBucket, 0, 10)
	for band := 0; band < 10; band++ {
		max := band*10 + 9
		if band == 9 {
			max = 100 // the top band absorbs a perfect score
		}
		buckets = append(buckets, ScoreBucket{Min: band * 10, Max: max, UniqueIPs: counts[band]})
	}
	return buckets, nil
}

// IPTimelinePoint is one snapshot of a single IP, for its detail view.
type IPTimelinePoint struct {
	Time           time.Time `json:"time"`
	RequestRate    float64   `json:"request_rate"`
	BotScore       int       `json:"bot_score"`
	WindowRequests int       `json:"window_requests"`
}

// IPDetail is everything known about one IP within a time range - the
// drill-down behind a row in TopIPs.
type IPDetail struct {
	SiteID string    `json:"site_id"`
	IP     string    `json:"ip"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	// Found is false when this IP has no snapshots in the range. That's an
	// ordinary answer (the IP simply wasn't active in the window the
	// caller asked about), not an error, so it comes back 200 with the
	// counters zeroed rather than as a 404.
	Found bool `json:"found"`

	PeakScore          int     `json:"peak_score"`
	PeakRequestRate    float64 `json:"peak_request_rate"`
	AvgRequestRate     float64 `json:"avg_request_rate"`
	PeakWindowRequests int     `json:"peak_window_requests"`

	Country       string `json:"country"`
	ASN           int    `json:"asn"`
	ASNName       string `json:"asn_name"`
	JA4           string `json:"ja4"`
	JA4Label      string `json:"ja4_label,omitempty"`
	IsKnownBotJA4 bool   `json:"is_known_bot_ja4"`
	IsKnownBotASN bool   `json:"is_known_bot_asn"`

	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
	Snapshots int       `json:"snapshots"`

	Timeline []IPTimelinePoint `json:"timeline"`
}

// IPDetail returns one IP's aggregate figures and its snapshot timeline.
func (s *Store) IPDetail(ctx context.Context, siteID string, ip netip.Addr, from, to time.Time, limit int) (IPDetail, error) {
	out := IPDetail{SiteID: siteID, IP: ip.String(), From: from, To: to, Timeline: []IPTimelinePoint{}}

	err := s.pool.QueryRow(ctx, `
		SELECT count(*),
		       COALESCE(max(bot_score), 0),
		       COALESCE(max(request_rate), 0),
		       COALESCE(avg(request_rate), 0),
		       COALESCE(max(prev_window_count + curr_window_count), 0),
		       COALESCE(max(country), ''),
		       COALESCE(max(asn), 0),
		       COALESCE(max(asn_org), ''),
		       COALESCE(max(ja4), ''),
		       COALESCE(bool_or(is_known_bot_ja4), false),
		       COALESCE(bool_or(is_known_bot_asn), false),
		       min(time), max(time)
		FROM traffic_snapshots
		WHERE site_id = $1 AND ip = $2 AND time >= $3 AND time < $4`,
		siteID, ip, from, to,
	).Scan(&out.Snapshots, &out.PeakScore, &out.PeakRequestRate, &out.AvgRequestRate,
		&out.PeakWindowRequests, &out.Country, &out.ASN, &out.ASNName, &out.JA4,
		&out.IsKnownBotJA4, &out.IsKnownBotASN, &nullableTime{&out.FirstSeen}, &nullableTime{&out.LastSeen})
	if err != nil {
		return IPDetail{}, fmt.Errorf("api: ip detail: %w", err)
	}
	if out.Snapshots == 0 {
		return out, nil // Found stays false; the zeroed struct is the answer
	}
	out.Found = true
	out.JA4Label, _ = s.knownBots.Label(out.JA4)

	rows, err := s.pool.Query(ctx, `
		SELECT time, request_rate, bot_score, prev_window_count + curr_window_count
		FROM traffic_snapshots
		WHERE site_id = $1 AND ip = $2 AND time >= $3 AND time < $4
		ORDER BY time
		LIMIT $5`,
		siteID, ip, from, to, limit,
	)
	if err != nil {
		return IPDetail{}, fmt.Errorf("api: ip timeline: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var p IPTimelinePoint
		if err := rows.Scan(&p.Time, &p.RequestRate, &p.BotScore, &p.WindowRequests); err != nil {
			return IPDetail{}, fmt.Errorf("api: scan timeline point: %w", err)
		}
		out.Timeline = append(out.Timeline, p)
	}
	return out, rows.Err()
}

// nullableTime scans a possibly-NULL timestamptz into a time.Time,
// leaving it at its zero value when NULL. min()/max() over zero rows
// return NULL, which a plain *time.Time can't accept.
type nullableTime struct{ dest *time.Time }

func (n *nullableTime) Scan(src any) error {
	if src == nil {
		return nil
	}
	t, ok := src.(time.Time)
	if !ok {
		return fmt.Errorf("api: cannot scan %T into time.Time", src)
	}
	*n.dest = t
	return nil
}

// Snapshot is one raw row, for the export/debug endpoint.
type Snapshot struct {
	Time            time.Time `json:"time"`
	IP              string    `json:"ip"`
	JA4             string    `json:"ja4"`
	PrevWindowCount int       `json:"prev_window_count"`
	CurrWindowCount int       `json:"curr_window_count"`
	RequestRate     float64   `json:"request_rate"`
	BotScore        int       `json:"bot_score"`
	IsKnownBotJA4   bool      `json:"is_known_bot_ja4"`
	Country         string    `json:"country"`
	ASN             int       `json:"asn"`
	ASNName         string    `json:"asn_name"`
	IsKnownBotASN   bool      `json:"is_known_bot_asn"`
}

// Snapshots returns raw rows, newest first, for export or for debugging a
// number the aggregate endpoints reported. Paginated, and returns the
// total so a caller can tell how much more there is.
func (s *Store) Snapshots(ctx context.Context, siteID string, from, to time.Time, limit, offset int) ([]Snapshot, int, error) {
	var total int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM traffic_snapshots WHERE site_id = $1 AND time >= $2 AND time < $3`,
		siteID, from, to,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("api: count snapshots: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT time, ip, ja4, prev_window_count, curr_window_count, request_rate,
		       bot_score, is_known_bot_ja4, country, asn, asn_org, is_known_bot_asn
		FROM traffic_snapshots
		WHERE site_id = $1 AND time >= $2 AND time < $3
		ORDER BY time DESC, ip
		LIMIT $4 OFFSET $5`,
		siteID, from, to, limit, offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("api: snapshots: %w", err)
	}
	defer rows.Close()

	out := []Snapshot{}
	for rows.Next() {
		var (
			snap Snapshot
			ip   netip.Addr
		)
		if err := rows.Scan(&snap.Time, &ip, &snap.JA4, &snap.PrevWindowCount, &snap.CurrWindowCount,
			&snap.RequestRate, &snap.BotScore, &snap.IsKnownBotJA4, &snap.Country, &snap.ASN,
			&snap.ASNName, &snap.IsKnownBotASN); err != nil {
			return nil, 0, fmt.Errorf("api: scan snapshot: %w", err)
		}
		snap.IP = ip.String()
		out = append(out, snap)
	}
	return out, total, rows.Err()
}

// countColumn is a column this package may count distinct values of.
//
// A named type with unexported values, not a string. The column name is
// interpolated into SQL - Postgres has no placeholder for an identifier -
// so the only thing standing between this query and CWE-89 is that no
// request-derived string can ever arrive here. A comment saying so is a
// comment; a type saying so is checked by the compiler, and a future
// handler that tries to pass a query parameter through does not build.
type countColumn string

const (
	countIP      countColumn = "ip"
	countCountry countColumn = "country"
	countASN     countColumn = "asn"
	countJA4     countColumn = "ja4"
)

// countDistinct counts the distinct values of one column for a site in a
// range, so paginated breakdowns can report a total.
func (s *Store) countDistinct(ctx context.Context, column countColumn, siteID string, from, to time.Time) (int, error) {
	// Belt and braces on top of the type. If somebody adds a value to
	// countColumn without adding it here, the query is refused rather
	// than run - which is the failure direction that matters when the
	// alternative is interpolating an unreviewed identifier.
	switch column {
	case countIP, countCountry, countASN, countJA4:
	default:
		return 0, fmt.Errorf("api: %q is not a countable column", column)
	}

	var total int
	err := s.pool.QueryRow(ctx,
		fmt.Sprintf(`SELECT count(DISTINCT %s) FROM traffic_snapshots WHERE site_id = $1 AND time >= $2 AND time < $3`, column),
		siteID, from, to,
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("api: count distinct %s: %w", column, err)
	}
	return total, nil
}
