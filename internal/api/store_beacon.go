package api

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"time"
)

// sessionTimeout is the gap between one visitor's events that starts a
// new session. Thirty minutes is the near-universal convention (GA,
// Matomo, Umami and Plausible all use it), and matching it is what makes
// this project's session numbers comparable to whatever the customer was
// using before.
//
// A constant rather than a query parameter on purpose: letting each
// caller pick would mean two panels, or two charts in one panel, quietly
// reporting different session counts for the same traffic.
const sessionTimeout = "30 minutes"

// beaconFilterCTE is the first CTE of every query in this file: it
// narrows beacon_events to one site, one time range, and one bot
// population.
//
// The bots predicate is written against a bound parameter rather than
// string-built, so nothing about it depends on request text reaching the
// SQL. Reading it: 'include' passes everything; otherwise the row is kept
// exactly when its is_bot_ua matches whether the caller asked for
// 'only'.
const beaconFilterCTE = `
	WITH filtered AS (
	    SELECT *
	    FROM beacon_events
	    WHERE site_id = $1 AND time >= $2 AND time < $3
	      AND ($4::text = 'include' OR ($4::text = 'only') = is_bot_ua)
	)`

// sessionCTEs sessionizes `filtered` into `sessions`, one row per event
// tagged with which session of that visitor it belongs to. Appended to
// beaconFilterCTE by the queries that need sessions; $5 is the timeout.
//
// Sessions are derived here, at read time, rather than assigned when the
// event arrives. Assigning at ingest would require the beacon to hold
// per-visitor state whose cardinality an attacker controls - the same
// unbounded-key-space problem that keeps the collector from keying by
// (IP, path). Deriving costs a sort per query and keeps ingest a pure
// function of one request.
const sessionCTEs = `,
	marked AS (
	    SELECT visitor_id, time, event_type, path,
	           CASE WHEN lag(time) OVER w IS NULL
	                  OR time - lag(time) OVER w > $5::interval
	                THEN 1 ELSE 0 END AS starts
	    FROM filtered
	    WINDOW w AS (PARTITION BY visitor_id ORDER BY time)
	),
	sessions AS (
	    SELECT visitor_id, time, event_type, path,
	           sum(starts) OVER (PARTITION BY visitor_id ORDER BY time ROWS UNBOUNDED PRECEDING) AS session_seq
	    FROM marked
	)`

// BeaconSummary is the headline view of one site's client-side activity.
//
// Every figure here counts a different thing from the collector's
// Summary, and they are not interchangeable: Visitors counts people (as
// well as a cookieless daily hash can), while Summary.UniqueIPs counts
// addresses, which CGNAT and dynamic assignment make a poor proxy for
// people in both directions.
type BeaconSummary struct {
	SiteID string    `json:"site_id"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	// Bots echoes the filter that produced these numbers, so a response
	// always carries enough to explain itself.
	Bots string `json:"bots"`

	Pageviews int `json:"pageviews"`
	// Events counts named custom events, not pageviews.
	Events   int `json:"events"`
	Visitors int `json:"visitors"`
	Sessions int `json:"sessions"`

	// BouncedSessions is sessions with one pageview or fewer.
	BouncedSessions int `json:"bounced_sessions"`
	// BounceRate is BouncedSessions/Sessions, 0 when there are none.
	BounceRate float64 `json:"bounce_rate"`
	// PagesPerSession is Pageviews/Sessions, 0 when there are none.
	PagesPerSession float64 `json:"pages_per_session"`

	// AvgSessionSeconds is the mean gap between a session's first and
	// last recorded event.
	//
	// It systematically *underestimates* real reading time, and that is
	// inherent rather than a defect here: nothing tells a beacon when a
	// visitor stopped reading and closed the tab, so the last page of
	// every session contributes zero, and a session with a single event
	// has a duration of exactly zero. Every tool built this way has the
	// same floor. Read it alongside BounceRate, which explains most of
	// what drags it down.
	AvgSessionSeconds float64 `json:"avg_session_seconds"`
}

// BeaconSummary computes the headline client-side statistics for one
// site over [from, to).
//
// Sessions are counted within the range only: one that began before
// `from` is truncated at it and one still running at `to` is cut short,
// so a very narrow range inflates session counts and depresses
// durations. This is the standard behavior of range-scoped
// sessionization, and the reason a panel should prefer whole days.
func (s *Store) BeaconSummary(ctx context.Context, siteID string, from, to time.Time, bots BotFilter) (BeaconSummary, error) {
	out := BeaconSummary{SiteID: siteID, From: from, To: to, Bots: string(bots)}

	err := s.pool.QueryRow(ctx, beaconFilterCTE+sessionCTEs+`,
		per_session AS (
		    SELECT visitor_id, session_seq,
		           count(*) FILTER (WHERE event_type = 'pageview') AS pageviews,
		           max(time) - min(time) AS duration
		    FROM sessions
		    GROUP BY visitor_id, session_seq
		)
		SELECT
		    (SELECT count(*) FROM filtered WHERE event_type = 'pageview'),
		    (SELECT count(*) FROM filtered WHERE event_type = 'event'),
		    (SELECT count(DISTINCT visitor_id) FROM filtered),
		    (SELECT count(*) FROM per_session),
		    (SELECT count(*) FROM per_session WHERE pageviews <= 1),
		    (SELECT COALESCE(avg(extract(epoch FROM duration)), 0) FROM per_session)`,
		siteID, from, to, string(bots), sessionTimeout,
	).Scan(&out.Pageviews, &out.Events, &out.Visitors, &out.Sessions, &out.BouncedSessions, &out.AvgSessionSeconds)
	if err != nil {
		return BeaconSummary{}, fmt.Errorf("api: beacon summary: %w", err)
	}

	// Derived in Go rather than SQL: the zero-session case is a plain
	// guard here, where in SQL it would need a NULLIF whose intent is
	// much less obvious to the next reader.
	if out.Sessions > 0 {
		out.BounceRate = float64(out.BouncedSessions) / float64(out.Sessions)
		out.PagesPerSession = float64(out.Pageviews) / float64(out.Sessions)
	}
	return out, nil
}

// BeaconBucket is one time slice of client-side activity.
type BeaconBucket struct {
	Time      time.Time `json:"time"`
	Pageviews int       `json:"pageviews"`
	Visitors  int       `json:"visitors"`
	// Sessions counts sessions that *started* in this bucket. A session
	// spanning several buckets is counted once, in its first - the
	// alternative (counting it in every bucket it touches) would make the
	// column sum to more than the range's own session total.
	Sessions int `json:"sessions"`
}

// BeaconTimeseries buckets client-side activity over [from, to).
// interval must already have been validated by ParseInterval.
func (s *Store) BeaconTimeseries(ctx context.Context, siteID string, from, to time.Time, interval string, bots BotFilter) ([]BeaconBucket, error) {
	rows, err := s.pool.Query(ctx, beaconFilterCTE+sessionCTEs+`,
		session_starts AS (
		    SELECT min(time) AS started
		    FROM sessions
		    GROUP BY visitor_id, session_seq
		),
		per_bucket AS (
		    SELECT time_bucket($6::interval, time) AS bucket,
		           count(*) FILTER (WHERE event_type = 'pageview') AS pageviews,
		           count(DISTINCT visitor_id) AS visitors
		    FROM filtered
		    GROUP BY bucket
		),
		per_bucket_sessions AS (
		    SELECT time_bucket($6::interval, started) AS bucket, count(*) AS sessions
		    FROM session_starts
		    GROUP BY bucket
		)
		-- LEFT, not FULL: a session's first event is by definition in the
		-- same bucket as the session start, so per_bucket already has every
		-- bucket per_bucket_sessions could contribute.
		SELECT b.bucket, b.pageviews, b.visitors, COALESCE(sx.sessions, 0)
		FROM per_bucket b
		LEFT JOIN per_bucket_sessions sx USING (bucket)
		ORDER BY b.bucket`,
		siteID, from, to, string(bots), sessionTimeout, interval,
	)
	if err != nil {
		return nil, fmt.Errorf("api: beacon timeseries: %w", err)
	}
	defer rows.Close()

	buckets := []BeaconBucket{}
	for rows.Next() {
		var b BeaconBucket
		if err := rows.Scan(&b.Time, &b.Pageviews, &b.Visitors, &b.Sessions); err != nil {
			return nil, fmt.Errorf("api: scan beacon bucket: %w", err)
		}
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}

// BeaconGroupStat is one value's share of a site's client-side traffic.
type BeaconGroupStat struct {
	Key string `json:"key"`
	// Pageviews counts pageview events in this group; Visitors counts
	// distinct visitors with *any* event in it, so a group reached only
	// by custom events still reports the people who reached it.
	Pageviews int `json:"pageviews"`
	Visitors  int `json:"visitors"`
	// Empty marks the group of rows where the value was never determined
	// (an unrecognized browser, an unresolved country, a direct visit
	// with no referrer). Flagged rather than dropped, so the numbers
	// still add up to the site's total.
	Empty bool `json:"empty,omitempty"`
}

// breakdownExpr names a column this package groups beacon events by.
// Deliberately a closed set of package constants: the value is
// interpolated into SQL by beaconBreakdown, and that is only safe
// because no caller can supply one.
type breakdownExpr string

const (
	byPath         breakdownExpr = "path"
	byReferrerHost breakdownExpr = "referrer_host"
	byBrowser      breakdownExpr = "browser"
	byOS           breakdownExpr = "os"
	byDevice       breakdownExpr = "device"
	byLanguage     breakdownExpr = "language"
)

// beaconBreakdown groups the filtered events by one column, busiest
// first, and returns the total number of distinct groups for paging.
//
// expr reaches the SQL through fmt.Sprintf, which is safe here and
// nowhere else: every call site passes one of the compile-time constants
// above. A future caller tempted to pass a request-supplied column name
// must not - that is exactly the injection this closed type exists to
// prevent.
func (s *Store) beaconBreakdown(ctx context.Context, siteID string, expr breakdownExpr, p beaconParams) ([]BeaconGroupStat, int, error) {
	total, err := s.beaconCountDistinct(ctx, siteID, expr, p)
	if err != nil {
		return nil, 0, err
	}

	rows, err := s.pool.Query(ctx, beaconFilterCTE+fmt.Sprintf(`
		SELECT %s,
		       count(*) FILTER (WHERE event_type = 'pageview') AS pageviews,
		       count(DISTINCT visitor_id) AS visitors
		FROM filtered
		GROUP BY 1
		-- The trailing key is a tie-break, without which two pages with
		-- equal counts could swap places between page 1 and page 2 of the
		-- same paginated walk and be shown twice or not at all.
		ORDER BY pageviews DESC, visitors DESC, 1
		LIMIT $5 OFFSET $6`, expr),
		siteID, p.from, p.to, string(p.bots), p.limit, p.offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("api: beacon breakdown by %s: %w", expr, err)
	}
	defer rows.Close()

	stats, err := scanBeaconGroupStats(rows)
	return stats, total, err
}

func (s *Store) beaconCountDistinct(ctx context.Context, siteID string, expr breakdownExpr, p beaconParams) (int, error) {
	var total int
	err := s.pool.QueryRow(ctx, beaconFilterCTE+fmt.Sprintf(`
		SELECT count(DISTINCT %s) FROM filtered`, expr),
		siteID, p.from, p.to, string(p.bots),
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("api: beacon count distinct %s: %w", expr, err)
	}
	return total, nil
}

func scanBeaconGroupStats(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]BeaconGroupStat, error) {
	stats := []BeaconGroupStat{}
	for rows.Next() {
		var stat BeaconGroupStat
		if err := rows.Scan(&stat.Key, &stat.Pageviews, &stat.Visitors); err != nil {
			return nil, fmt.Errorf("api: scan beacon group stat: %w", err)
		}
		stat.Empty = stat.Key == ""
		stats = append(stats, stat)
	}
	return stats, rows.Err()
}

// BeaconPages breaks client-side traffic down by path.
func (s *Store) BeaconPages(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return s.beaconBreakdown(ctx, siteID, byPath, p)
}

// BeaconReferrers breaks client-side traffic down by referring host.
// Same-origin referrers never reach the database - the snippet drops
// them in the browser - so the empty group here is genuinely "direct or
// unknown", not "internal navigation".
func (s *Store) BeaconReferrers(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return s.beaconBreakdown(ctx, siteID, byReferrerHost, p)
}

// BeaconBrowsers breaks client-side traffic down by browser.
func (s *Store) BeaconBrowsers(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return s.beaconBreakdown(ctx, siteID, byBrowser, p)
}

// BeaconOperatingSystems breaks client-side traffic down by OS.
func (s *Store) BeaconOperatingSystems(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return s.beaconBreakdown(ctx, siteID, byOS, p)
}

// BeaconDevices breaks client-side traffic down by device form factor.
func (s *Store) BeaconDevices(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return s.beaconBreakdown(ctx, siteID, byDevice, p)
}

// BeaconLanguages breaks client-side traffic down by browser language.
func (s *Store) BeaconLanguages(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return s.beaconBreakdown(ctx, siteID, byLanguage, p)
}

// CampaignStat is one marketing campaign's share of traffic.
type CampaignStat struct {
	// Key is the stored, normalized campaign query string, e.g.
	// "utm_medium=email&utm_source=newsletter".
	Key string `json:"key"`
	// Params is Key decoded into its individual parameters, so a panel
	// can group by utm_source alone without re-implementing query
	// parsing. Decoding happens here rather than in SQL because
	// percent-encoding is a Go-standard-library problem and an
	// error-prone hand-rolled one in PostgreSQL.
	Params    map[string]string `json:"params"`
	Pageviews int               `json:"pageviews"`
	Visitors  int               `json:"visitors"`
}

// BeaconCampaigns breaks traffic down by the campaign parameters that
// brought it in. Only events that carried at least one campaign
// parameter are counted - the vast majority of traffic has none, and
// including it would bury every real campaign under one enormous empty
// row.
func (s *Store) BeaconCampaigns(ctx context.Context, siteID string, p beaconParams) ([]CampaignStat, int, error) {
	var total int
	err := s.pool.QueryRow(ctx, beaconFilterCTE+`
		SELECT count(DISTINCT query) FROM filtered WHERE query <> ''`,
		siteID, p.from, p.to, string(p.bots),
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("api: beacon campaigns total: %w", err)
	}

	rows, err := s.pool.Query(ctx, beaconFilterCTE+`
		SELECT query,
		       count(*) FILTER (WHERE event_type = 'pageview') AS pageviews,
		       count(DISTINCT visitor_id) AS visitors
		FROM filtered
		WHERE query <> ''
		GROUP BY query
		ORDER BY pageviews DESC, visitors DESC, query
		LIMIT $5 OFFSET $6`,
		siteID, p.from, p.to, string(p.bots), p.limit, p.offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("api: beacon campaigns: %w", err)
	}
	defer rows.Close()

	stats := []CampaignStat{}
	for rows.Next() {
		var stat CampaignStat
		if err := rows.Scan(&stat.Key, &stat.Pageviews, &stat.Visitors); err != nil {
			return nil, 0, fmt.Errorf("api: scan campaign stat: %w", err)
		}
		stat.Params = decodeCampaign(stat.Key)
		stats = append(stats, stat)
	}
	return stats, total, rows.Err()
}

// decodeCampaign parses a stored campaign string into its parameters. A
// value that somehow fails to parse yields an empty map rather than an
// error: the counts are still correct and worth returning, and the raw
// Key is right there for the caller to inspect.
func decodeCampaign(raw string) map[string]string {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return map[string]string{}
	}
	params := make(map[string]string, len(values))
	for key := range values {
		params[key] = values.Get(key)
	}
	return params
}

// EventStat is one named custom event's frequency.
type EventStat struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	// Visitors is how many distinct people raised it, which separates
	// "300 clicks from one person" from "300 people clicked once".
	Visitors int `json:"visitors"`
}

// BeaconEvents lists named custom events, most frequent first.
func (s *Store) BeaconEvents(ctx context.Context, siteID string, p beaconParams) ([]EventStat, int, error) {
	var total int
	err := s.pool.QueryRow(ctx, beaconFilterCTE+`
		SELECT count(DISTINCT event_name) FROM filtered WHERE event_type = 'event'`,
		siteID, p.from, p.to, string(p.bots),
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("api: beacon events total: %w", err)
	}

	rows, err := s.pool.Query(ctx, beaconFilterCTE+`
		SELECT event_name, count(*), count(DISTINCT visitor_id)
		FROM filtered
		WHERE event_type = 'event'
		GROUP BY event_name
		ORDER BY count(*) DESC, event_name
		LIMIT $5 OFFSET $6`,
		siteID, p.from, p.to, string(p.bots), p.limit, p.offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("api: beacon events: %w", err)
	}
	defer rows.Close()

	stats := []EventStat{}
	for rows.Next() {
		var stat EventStat
		if err := rows.Scan(&stat.Name, &stat.Count, &stat.Visitors); err != nil {
			return nil, 0, fmt.Errorf("api: scan event stat: %w", err)
		}
		stats = append(stats, stat)
	}
	return stats, total, rows.Err()
}

// SessionPathStat is one path's role as a session boundary.
type SessionPathStat struct {
	Path string `json:"path"`
	// Sessions is how many sessions began (or ended) on this path, not
	// how many pageviews it received.
	Sessions int `json:"sessions"`
	Visitors int `json:"visitors"`
}

// BeaconEntryPages ranks the pages sessions began on - the landing pages
// that acquisition actually lands on, which is a different and usually
// more actionable list than the most-viewed pages.
func (s *Store) BeaconEntryPages(ctx context.Context, siteID string, p beaconParams) ([]SessionPathStat, int, error) {
	return s.sessionBoundaryPages(ctx, siteID, p, true)
}

// BeaconExitPages ranks the pages sessions ended on.
//
// "Ended on" means "was the last page with a recorded event", which is
// an honest definition but a narrower one than it sounds: the beacon
// cannot tell a visitor who left from one who is still reading, so the
// final page of every session in progress at `to` lands here too.
func (s *Store) BeaconExitPages(ctx context.Context, siteID string, p beaconParams) ([]SessionPathStat, int, error) {
	return s.sessionBoundaryPages(ctx, siteID, p, false)
}

// sessionBoundaryPages is BeaconEntryPages and BeaconExitPages, which
// differ only in which end of each session they take.
func (s *Store) sessionBoundaryPages(ctx context.Context, siteID string, p beaconParams, entry bool) ([]SessionPathStat, int, error) {
	// DISTINCT ON keeps the first row per session under the given
	// ordering, so flipping the time direction flips entry to exit.
	direction := "DESC"
	if entry {
		direction = "ASC"
	}

	// Both queries share this shape; the total is the number of distinct
	// boundary paths, which is what a caller pages through.
	boundary := fmt.Sprintf(`,
		boundary AS (
		    SELECT DISTINCT ON (visitor_id, session_seq) visitor_id, path
		    FROM sessions
		    WHERE event_type = 'pageview'
		    ORDER BY visitor_id, session_seq, time %s
		)`, direction)

	var total int
	err := s.pool.QueryRow(ctx, beaconFilterCTE+sessionCTEs+boundary+`
		SELECT count(DISTINCT path) FROM boundary`,
		siteID, p.from, p.to, string(p.bots), sessionTimeout,
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("api: beacon boundary pages total: %w", err)
	}

	rows, err := s.pool.Query(ctx, beaconFilterCTE+sessionCTEs+boundary+`
		SELECT path, count(*), count(DISTINCT visitor_id)
		FROM boundary
		GROUP BY path
		ORDER BY count(*) DESC, path
		LIMIT $6 OFFSET $7`,
		siteID, p.from, p.to, string(p.bots), sessionTimeout, p.limit, p.offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("api: beacon boundary pages: %w", err)
	}
	defer rows.Close()

	stats := []SessionPathStat{}
	for rows.Next() {
		var stat SessionPathStat
		if err := rows.Scan(&stat.Path, &stat.Sessions, &stat.Visitors); err != nil {
			return nil, 0, fmt.Errorf("api: scan session path stat: %w", err)
		}
		stats = append(stats, stat)
	}
	return stats, total, rows.Err()
}

// BeaconCountries breaks client-side traffic down by country.
//
// The country is taken from the beacon's own column when it has one, and
// otherwise recovered from traffic_snapshots for the same IP. That
// fallback is the normal path, not an edge case: the recommended
// deployment leaves the beacon's own geo lookup off precisely because
// the collector on the same host already resolves every IP it sees, and
// without this join that recommended configuration would return one
// large empty group.
func (s *Store) BeaconCountries(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	// One representative country per IP, most recent first - the same
	// "effectively constant per IP" reasoning TopIPs documents.
	const geoCTE = `,
		geo AS (
		    SELECT DISTINCT ON (ip) ip, country
		    FROM traffic_snapshots
		    WHERE site_id = $1 AND time >= $2 AND time < $3 AND country <> ''
		    ORDER BY ip, time DESC
		),
		resolved AS (
		    SELECT COALESCE(NULLIF(f.country, ''), g.country, '') AS country,
		           f.event_type, f.visitor_id
		    FROM filtered f
		    LEFT JOIN geo g ON g.ip = f.ip
		)`

	var total int
	err := s.pool.QueryRow(ctx, beaconFilterCTE+geoCTE+`
		SELECT count(DISTINCT country) FROM resolved`,
		siteID, p.from, p.to, string(p.bots),
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("api: beacon countries total: %w", err)
	}

	rows, err := s.pool.Query(ctx, beaconFilterCTE+geoCTE+`
		SELECT country,
		       count(*) FILTER (WHERE event_type = 'pageview') AS pageviews,
		       count(DISTINCT visitor_id) AS visitors
		FROM resolved
		GROUP BY country
		ORDER BY pageviews DESC, visitors DESC, country
		LIMIT $5 OFFSET $6`,
		siteID, p.from, p.to, string(p.bots), p.limit, p.offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("api: beacon countries: %w", err)
	}
	defer rows.Close()

	stats, err := scanBeaconGroupStats(rows)
	return stats, total, err
}

// BeaconEvent is one raw stored event, for export or for checking where
// an aggregate came from.
type BeaconEvent struct {
	Time      time.Time `json:"time"`
	VisitorID string    `json:"visitor_id"`
	EventType string    `json:"event_type"`
	EventName string    `json:"event_name,omitempty"`

	Path  string `json:"path"`
	Query string `json:"query,omitempty"`
	Title string `json:"title,omitempty"`

	ReferrerHost string `json:"referrer_host,omitempty"`
	ReferrerPath string `json:"referrer_path,omitempty"`

	IP      string `json:"ip"`
	Browser string `json:"browser,omitempty"`
	OS      string `json:"os,omitempty"`
	Device  string `json:"device,omitempty"`
	IsBotUA bool   `json:"is_bot_ua"`

	ScreenW  int    `json:"screen_w,omitempty"`
	ScreenH  int    `json:"screen_h,omitempty"`
	Language string `json:"language,omitempty"`

	Country string `json:"country,omitempty"`
	ASN     int    `json:"asn,omitempty"`
	ASNOrg  string `json:"asn_org,omitempty"`
}

// BeaconRaw returns stored events newest first, paginated.
func (s *Store) BeaconRaw(ctx context.Context, siteID string, p beaconParams) ([]BeaconEvent, int, error) {
	var total int
	err := s.pool.QueryRow(ctx, beaconFilterCTE+`SELECT count(*) FROM filtered`,
		siteID, p.from, p.to, string(p.bots),
	).Scan(&total)
	if err != nil {
		return nil, 0, fmt.Errorf("api: beacon raw total: %w", err)
	}

	rows, err := s.pool.Query(ctx, beaconFilterCTE+`
		SELECT time, visitor_id, event_type, event_name, path, query, title,
		       referrer_host, referrer_path, ip, browser, os, device, is_bot_ua,
		       screen_w, screen_h, language, country, asn, asn_org
		FROM filtered
		-- visitor_id breaks ties within one flush of simultaneous events,
		-- so paging through an export can't show a row twice.
		ORDER BY time DESC, visitor_id
		LIMIT $5 OFFSET $6`,
		siteID, p.from, p.to, string(p.bots), p.limit, p.offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("api: beacon raw: %w", err)
	}
	defer rows.Close()

	events := []BeaconEvent{}
	for rows.Next() {
		var (
			e  BeaconEvent
			ip netip.Addr
		)
		if err := rows.Scan(&e.Time, &e.VisitorID, &e.EventType, &e.EventName, &e.Path, &e.Query, &e.Title,
			&e.ReferrerHost, &e.ReferrerPath, &ip, &e.Browser, &e.OS, &e.Device, &e.IsBotUA,
			&e.ScreenW, &e.ScreenH, &e.Language, &e.Country, &e.ASN, &e.ASNOrg); err != nil {
			return nil, 0, fmt.Errorf("api: scan beacon event: %w", err)
		}
		e.IP = ip.String()
		events = append(events, e)
	}
	return events, total, rows.Err()
}

// BeaconSites returns every site ID present in beacon_events. Separate
// from Sites, which reads traffic_snapshots: a site can have one source
// running and not the other, so answering "which sites have beacon data"
// from the collector's table would be a guess.
func (s *Store) BeaconSites(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT site_id FROM beacon_events WHERE site_id <> '' ORDER BY site_id`)
	if err != nil {
		return nil, fmt.Errorf("api: query beacon sites: %w", err)
	}
	defer rows.Close()

	sites := []string{}
	for rows.Next() {
		var site string
		if err := rows.Scan(&site); err != nil {
			return nil, fmt.Errorf("api: scan beacon site: %w", err)
		}
		sites = append(sites, site)
	}
	return sites, rows.Err()
}
