//go:build integration

// Real coverage of the query layer against a live TimescaleDB, gated
// behind the "integration" build tag like internal/storage's. These
// exercise what server_test.go's fake store deliberately can't: that the
// SQL is valid, that site_id genuinely isolates one customer's data from
// another's, and that the reported figures are the numbers they claim to
// be. Run with:
//
//	docker compose up -d
//	psql "postgres://collector:collector@localhost:5432/analytics" -f internal/storage/schema.sql
//	go test -tags integration ./internal/api/... -v

package api

import (
	"context"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/scoring"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURL = "postgres://collector:collector@localhost:5432/analytics"

// seedRow is one row to insert for a test.
type seedRow struct {
	site    string
	ip      string
	at      time.Time
	rate    float64
	score   int16
	country string
	asn     int
	asnOrg  string
	botJA4  bool
	botASN  bool
	ja4     string
	prevWin int
	currWin int
}

// newTestStore opens a Store and seeds rows, cleaning them up afterwards.
// Every seeded row shares a JA4 marker unique to the test, so cleanup can
// delete exactly what it inserted without disturbing anything else in the
// database.
func newTestStore(t *testing.T, marker string, rows []seedRow) *Store {
	t.Helper()
	return seedStore(t, rows, func(r seedRow) string { return marker })
}

// newTestStoreWithJA4 is newTestStore for tests that need each row to
// carry its own fingerprint (the JA4 breakdown, which groups by it)
// rather than a shared cleanup marker.
func newTestStoreWithJA4(t *testing.T, marker string, rows []seedRow) *Store {
	t.Helper()
	return seedStore(t, rows, func(r seedRow) string { return r.ja4 })
}

// seedStore opens a Store, inserts rows with ja4 chosen by ja4For, and
// cleans up afterwards.
//
// Cleanup deletes by the site_ids that were seeded rather than by a JA4
// marker: rows carrying their own fingerprint have no shared marker to
// match on, and every test here already uses a site_id unique to itself.
func seedStore(t *testing.T, rows []seedRow, ja4For func(seedRow) string) *Store {
	t.Helper()

	store, err := NewStore(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("NewStore: %v (is `docker compose up -d` running, with internal/storage/schema.sql applied?)", err)
	}
	t.Cleanup(store.Close)

	pool, err := pgxpool.New(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	seededSites := map[string]struct{}{}
	for _, r := range rows {
		seededSites[r.site] = struct{}{}
	}
	t.Cleanup(func() {
		for site := range seededSites {
			if _, err := pool.Exec(context.Background(), `DELETE FROM traffic_snapshots WHERE site_id = $1`, site); err != nil {
				t.Logf("cleanup: deleting site %s failed: %v", site, err)
			}
		}
	})

	for _, r := range rows {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO traffic_snapshots
			  (time, site_id, ip, ja4, prev_window_count, curr_window_count, request_rate,
			   bot_score, is_known_bot_ja4, country, asn, asn_org, is_known_bot_asn)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			r.at, r.site, r.ip, ja4For(r), r.prevWin, r.currWin, r.rate,
			r.score, r.botJA4, r.country, r.asn, r.asnOrg, r.botASN)
		if err != nil {
			t.Fatalf("seeding row %+v: %v", r, err)
		}
	}
	return store
}

func TestStore_RealTimescaleDB_SummaryCountsDistinctIPs(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	marker := "t13d_api_summary"

	// Three distinct IPs, one of them sampled twice (so a naive row count
	// would report 4 and be wrong). One IP scores above the bot cutoff.
	store := newTestStore(t, marker, []seedRow{
		{site: "site-a", ip: "203.0.113.1", at: base, rate: 1, score: 90},
		{site: "site-a", ip: "203.0.113.1", at: base.Add(10 * time.Second), rate: 3, score: 90},
		{site: "site-a", ip: "203.0.113.2", at: base, rate: 2, score: 10},
		{site: "site-a", ip: "203.0.113.3", at: base, rate: 1, score: 0},
	})

	got, err := store.Summary(context.Background(), "site-a", base.Add(-time.Minute), base.Add(time.Minute), DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}

	if got.UniqueIPs != 3 {
		t.Errorf("UniqueIPs = %d, want 3 (distinct IPs, not row count)", got.UniqueIPs)
	}
	if got.BotIPs != 1 {
		t.Errorf("BotIPs = %d, want 1", got.BotIPs)
	}
	if got.HumanIPs != 2 {
		t.Errorf("HumanIPs = %d, want 2", got.HumanIPs)
	}
	if got.Snapshots != 4 {
		t.Errorf("Snapshots = %d, want 4 (the raw row count, for transparency)", got.Snapshots)
	}
	if got.PeakRequestRate != 3 {
		t.Errorf("PeakRequestRate = %v, want 3", got.PeakRequestRate)
	}
	if want := (1.0 + 3.0 + 2.0 + 1.0) / 4; got.AvgRequestRate != want {
		t.Errorf("AvgRequestRate = %v, want %v", got.AvgRequestRate, want)
	}
}

// TestStore_RealTimescaleDB_PeakWindowRequestsSumsAcrossIPsAtOneFlush
// pins the busiest-window figure: for each flush, every active IP's
// window counters are totalled, and the largest such total wins.
func TestStore_RealTimescaleDB_PeakWindowRequestsSumsAcrossIPsAtOneFlush(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	marker := "t13d_api_peakwindow"

	// Flush 1: two IPs, (1+9) + (0+5) = 15 requests in window.
	// Flush 2: one IP,  (9+11)        = 20 requests in window  <- the peak.
	store := newTestStore(t, marker, []seedRow{
		{site: "site-peak", ip: "203.0.113.1", at: base, prevWin: 1, currWin: 9},
		{site: "site-peak", ip: "203.0.113.2", at: base, prevWin: 0, currWin: 5},
		{site: "site-peak", ip: "203.0.113.1", at: base.Add(10 * time.Second), prevWin: 9, currWin: 11},
	})

	got, err := store.Summary(context.Background(), "site-peak", base.Add(-time.Minute), base.Add(time.Minute), DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if got.PeakWindowRequests != 20 {
		t.Errorf("PeakWindowRequests = %d, want 20 (the busiest single flush, not a sum across flushes)", got.PeakWindowRequests)
	}
}

// TestStore_RealTimescaleDB_PeakWindowRequestsSurvivesIrregularFlushes is
// the regression test for a real defect found by measurement: an earlier
// version of this field reconstructed a request total by integrating the
// sampled rate over time, which assumed evenly spaced flushes. The
// collector only writes rows for IPs seen since the previous flush, so
// bursty traffic produces genuinely irregular gaps - and a real run that
// sent 38 requests reported an estimate of 3. Reading the collector's own
// counters instead makes the spacing irrelevant, which is what this
// asserts.
func TestStore_RealTimescaleDB_PeakWindowRequestsSurvivesIrregularFlushes(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	marker := "t13d_api_irregular"

	// Gaps of 20s, 4s, 2s - the same irregular shape a real bursty run
	// produced - with the window counters climbing to 38.
	store := newTestStore(t, marker, []seedRow{
		{site: "site-irr", ip: "203.0.113.1", at: base, rate: 0.133, currWin: 8},
		{site: "site-irr", ip: "203.0.113.1", at: base.Add(20 * time.Second), rate: 0.3, currWin: 18},
		{site: "site-irr", ip: "203.0.113.1", at: base.Add(24 * time.Second), rate: 0.467, currWin: 28},
		{site: "site-irr", ip: "203.0.113.1", at: base.Add(26 * time.Second), rate: 0.633, currWin: 38},
	})

	got, err := store.Summary(context.Background(), "site-irr", base.Add(-time.Minute), base.Add(time.Minute), DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if got.PeakWindowRequests != 38 {
		t.Errorf("PeakWindowRequests = %d, want 38 - it must read the counters directly, not reconstruct a total from the sampled rate", got.PeakWindowRequests)
	}
}

// TestStore_RealTimescaleDB_SiteIsolation is the load-bearing test for
// the whole multi-customer design: every query must be scoped to the
// requested site. A leak here would show one customer another's traffic.
func TestStore_RealTimescaleDB_SiteIsolation(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	marker := "t13d_api_isolation"

	// Same IP, same timestamps, different sites - so only site_id can
	// tell the two customers' data apart.
	store := newTestStore(t, marker, []seedRow{
		{site: "site-a", ip: "203.0.113.1", at: base, rate: 1, score: 10, country: "US", asn: 111, asnOrg: "AAA"},
		{site: "site-a", ip: "203.0.113.1", at: base.Add(10 * time.Second), rate: 1, score: 10, country: "US", asn: 111, asnOrg: "AAA"},
		{site: "site-b", ip: "203.0.113.1", at: base, rate: 9, score: 99, country: "DE", asn: 222, asnOrg: "BBB"},
		{site: "site-b", ip: "203.0.113.2", at: base, rate: 9, score: 99, country: "DE", asn: 222, asnOrg: "BBB"},
	})

	ctx := context.Background()
	from, to := base.Add(-time.Minute), base.Add(time.Minute)

	summary, err := store.Summary(ctx, "site-a", from, to, DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary.UniqueIPs != 1 {
		t.Errorf("site-a UniqueIPs = %d, want 1 (site-b's extra IP must not appear)", summary.UniqueIPs)
	}
	if summary.PeakRequestRate != 1 {
		t.Errorf("site-a PeakRequestRate = %v, want 1 (site-b's rate of 9 must not leak)", summary.PeakRequestRate)
	}
	if summary.BotIPs != 0 {
		t.Errorf("site-a BotIPs = %d, want 0 (site-b's high-scoring IP must not leak)", summary.BotIPs)
	}

	countries, _, err := store.Countries(ctx, "site-a", from, to, 10, 0, DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("Countries: %v", err)
	}
	for _, c := range countries {
		if c.Key == "DE" {
			t.Errorf("site-a countries include DE, which only site-b has: %+v", countries)
		}
	}

	asns, _, err := store.ASNs(ctx, "site-a", from, to, 10, 0, DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("ASNs: %v", err)
	}
	for _, a := range asns {
		if a.Key == "222" {
			t.Errorf("site-a ASNs include 222, which only site-b has: %+v", asns)
		}
	}

	ips, _, err := store.TopIPs(ctx, "site-a", from, to, 10, 0)
	if err != nil {
		t.Fatalf("TopIPs: %v", err)
	}
	if len(ips) != 1 {
		t.Fatalf("site-a TopIPs returned %d rows, want 1: %+v", len(ips), ips)
	}
	if ips[0].PeakScore != 10 {
		t.Errorf("site-a top IP PeakScore = %d, want 10 (site-b's 99 must not leak)", ips[0].PeakScore)
	}

	buckets, err := store.Timeseries(ctx, "site-a", from, to, "1 minute", DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("Timeseries: %v", err)
	}
	for _, b := range buckets {
		if b.BotIPs != 0 {
			t.Errorf("site-a timeseries bucket reports %d bot IPs, want 0: %+v", b.BotIPs, b)
		}
	}

	// The endpoints added after the first cut have to be held to the same
	// standard - a new query that forgot its site_id filter would leak
	// exactly the same way, and would be easy to miss.
	dist, err := store.ScoreDistribution(ctx, "site-a", from, to)
	if err != nil {
		t.Fatalf("ScoreDistribution: %v", err)
	}
	for _, b := range dist {
		if b.Min >= 90 && b.UniqueIPs != 0 {
			t.Errorf("site-a score distribution has %d IPs in the 90+ band, want 0 (only site-b scored 99)", b.UniqueIPs)
		}
	}

	snaps, total, err := store.Snapshots(ctx, "site-a", from, to, 10, 0)
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	if total != 2 || len(snaps) != 2 {
		t.Errorf("site-a snapshots = %d rows (total %d), want 2 - site-b's rows must not appear", len(snaps), total)
	}

	// site-b's IP 203.0.113.2 never appeared under site-a, so asking
	// site-a about it must report "not found" rather than site-b's data.
	detail, err := store.IPDetail(ctx, "site-a", netip.MustParseAddr("203.0.113.2"), from, to, 100)
	if err != nil {
		t.Fatalf("IPDetail: %v", err)
	}
	if detail.Found {
		t.Errorf("site-a IPDetail found an IP that only exists under site-b: %+v", detail)
	}

	// The shared IP does exist under site-a, but must carry site-a's
	// numbers, not site-b's.
	shared, err := store.IPDetail(ctx, "site-a", netip.MustParseAddr("203.0.113.1"), from, to, 100)
	if err != nil {
		t.Fatalf("IPDetail: %v", err)
	}
	if !shared.Found || shared.PeakScore != 10 || shared.Country != "US" {
		t.Errorf("site-a IPDetail = %+v, want found with score 10 and country US (site-b's 99/DE must not leak)", shared)
	}

	ja4s, _, err := store.JA4s(ctx, "site-a", from, to, 10, 0, DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("JA4s: %v", err)
	}
	for _, j := range ja4s {
		if j.UniqueIPs != 1 {
			t.Errorf("site-a ja4 group %+v counts %d IPs, want 1 - site-b's IPs must not be counted", j, j.UniqueIPs)
		}
	}
}

func TestStore_RealTimescaleDB_JA4sLabelsKnownBots(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	marker := "t13d_api_ja4"

	// The fingerprint set is supplied here rather than read from a
	// package global. This project ships no copy of that dataset - a
	// deployment fetches its own - so a test that reached for an
	// embedded list would be testing something that no longer exists,
	// and would have skipped itself rather than failing when it went
	// away.
	const knownJA4 = "t13d1516h2_8daaf6152771_b186095e22b6"
	const knownLabel = "curl"

	store := newTestStoreWithJA4(t, marker, []seedRow{
		{site: "site-ja4", ip: "203.0.113.1", at: base, score: 90, botJA4: true, ja4: knownJA4},
		{site: "site-ja4", ip: "203.0.113.2", at: base, score: 10, ja4: "t13d1516h2_notabot_xxxx"},
		{site: "site-ja4", ip: "203.0.113.3", at: base, score: 0, ja4: ""}, // plaintext, no fingerprint
	})
	store.SetKnownBots(scoring.KnownBots{knownJA4: knownLabel})

	stats, total, err := store.JA4s(context.Background(), "site-ja4", base.Add(-time.Minute), base.Add(time.Minute), 10, 0, DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("JA4s: %v", err)
	}
	if total != 3 {
		t.Errorf("total = %d, want 3 distinct fingerprints (including the empty one)", total)
	}

	var sawLabel, sawEmpty bool
	for _, s := range stats {
		if s.JA4 == knownJA4 {
			if s.Label != knownLabel {
				t.Errorf("known JA4 %q label = %q, want %q", s.JA4, s.Label, knownLabel)
			}
			if !s.IsKnownBotJA4 {
				t.Errorf("known JA4 %q has IsKnownBotJA4 = false", s.JA4)
			}
			sawLabel = true
		}
		if s.JA4 == "" {
			if !s.Empty {
				t.Error("the empty-fingerprint group is not flagged Empty")
			}
			sawEmpty = true
		}
	}
	if !sawLabel {
		t.Error("the known-bot fingerprint never came back")
	}
	if !sawEmpty {
		t.Error("traffic with no fingerprint was dropped instead of grouped under an empty key")
	}
}

func TestStore_RealTimescaleDB_ScoreDistributionCoversEveryBand(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	marker := "t13d_api_dist"

	store := newTestStore(t, marker, []seedRow{
		{site: "site-dist", ip: "203.0.113.1", at: base, score: 5},   // band 0
		{site: "site-dist", ip: "203.0.113.2", at: base, score: 55},  // band 5
		{site: "site-dist", ip: "203.0.113.3", at: base, score: 100}, // band 9 (folded)
	})

	buckets, err := store.ScoreDistribution(context.Background(), "site-dist", base.Add(-time.Minute), base.Add(time.Minute))
	if err != nil {
		t.Fatalf("ScoreDistribution: %v", err)
	}
	if len(buckets) != 10 {
		t.Fatalf("got %d buckets, want all 10 bands present (including empty ones, so a chart needn't fill gaps)", len(buckets))
	}
	if buckets[0].UniqueIPs != 1 || buckets[5].UniqueIPs != 1 || buckets[9].UniqueIPs != 1 {
		t.Errorf("buckets = %+v, want one IP each in bands 0, 5 and 9", buckets)
	}
	if buckets[9].Max != 100 {
		t.Errorf("top band Max = %d, want 100 (a perfect score folds into the top band, not an 11th)", buckets[9].Max)
	}
	if buckets[1].UniqueIPs != 0 {
		t.Errorf("band 1 = %d, want 0", buckets[1].UniqueIPs)
	}
}

func TestStore_RealTimescaleDB_IPDetailReturnsTimeline(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	marker := "t13d_api_ipdetail"

	store := newTestStore(t, marker, []seedRow{
		{site: "site-ip", ip: "203.0.113.1", at: base, rate: 1, score: 20, currWin: 10, country: "US", asn: 111, asnOrg: "AAA"},
		{site: "site-ip", ip: "203.0.113.1", at: base.Add(10 * time.Second), rate: 3, score: 80, currWin: 30, country: "US", asn: 111, asnOrg: "AAA", botASN: true},
	})

	got, err := store.IPDetail(context.Background(), "site-ip", netip.MustParseAddr("203.0.113.1"), base.Add(-time.Minute), base.Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("IPDetail: %v", err)
	}
	if !got.Found {
		t.Fatal("Found = false, want true")
	}
	if got.PeakScore != 80 || got.PeakRequestRate != 3 || got.PeakWindowRequests != 30 {
		t.Errorf("peaks = score %d, rate %v, window %d; want 80/3/30", got.PeakScore, got.PeakRequestRate, got.PeakWindowRequests)
	}
	if got.Country != "US" || got.ASN != 111 || got.ASNName != "AAA" || !got.IsKnownBotASN {
		t.Errorf("enrichment = %q/%d/%q botASN=%v, want US/111/AAA/true", got.Country, got.ASN, got.ASNName, got.IsKnownBotASN)
	}
	if got.Snapshots != 2 || len(got.Timeline) != 2 {
		t.Fatalf("Snapshots = %d, timeline = %d points; want 2 and 2", got.Snapshots, len(got.Timeline))
	}
	if !got.Timeline[0].Time.Before(got.Timeline[1].Time) {
		t.Error("timeline is not in ascending time order")
	}
	if got.FirstSeen.After(got.LastSeen) {
		t.Errorf("FirstSeen %v is after LastSeen %v", got.FirstSeen, got.LastSeen)
	}
}

func TestStore_RealTimescaleDB_IPDetailNotFoundIsNotAnError(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	store := newTestStore(t, "t13d_api_ipmissing", nil)

	got, err := store.IPDetail(context.Background(), "site-none", netip.MustParseAddr("203.0.113.99"), base, base.Add(time.Minute), 100)
	if err != nil {
		t.Fatalf("IPDetail: %v", err)
	}
	if got.Found {
		t.Error("Found = true for an IP with no snapshots")
	}
	if got.Timeline == nil {
		t.Error("Timeline = nil, want an empty non-nil slice so it serialises as []")
	}
}

func TestStore_RealTimescaleDB_OverviewCoversSeveralSitesAtOnce(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	marker := "t13d_api_overview"

	store := newTestStore(t, marker, []seedRow{
		{site: "zzz-ov-a", ip: "203.0.113.1", at: base, rate: 1, score: 90},
		{site: "zzz-ov-a", ip: "203.0.113.2", at: base, rate: 2, score: 10},
		{site: "zzz-ov-b", ip: "203.0.113.3", at: base, rate: 5, score: 10},
	})
	ctx := context.Background()
	from, to := base.Add(-time.Minute), base.Add(time.Minute)

	// Restricted to the two seeded sites, so other tests' data can't
	// affect the assertions.
	got, err := store.Overview(ctx, []string{"zzz-ov-a", "zzz-ov-b"}, from, to, DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d sites, want 2: %+v", len(got), got)
	}
	// Ordered by unique IPs descending, so site-a (2 IPs) comes first.
	if got[0].SiteID != "zzz-ov-a" || got[0].UniqueIPs != 2 || got[0].BotIPs != 1 || got[0].HumanIPs != 1 {
		t.Errorf("first site = %+v, want zzz-ov-a with 2 IPs (1 bot, 1 human)", got[0])
	}
	if got[1].SiteID != "zzz-ov-b" || got[1].UniqueIPs != 1 || got[1].PeakRequestRate != 5 {
		t.Errorf("second site = %+v, want zzz-ov-b with 1 IP and peak rate 5", got[1])
	}
}

func TestStore_RealTimescaleDB_OverviewRestrictsToTheGivenSites(t *testing.T) {
	// The nil-means-everything behaviour is what a wildcard token gets;
	// an explicit list must exclude everything else, which is what stops
	// one customer's token seeing another's row in the overview.
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	marker := "t13d_api_ovscope"

	store := newTestStore(t, marker, []seedRow{
		{site: "zzz-scope-a", ip: "203.0.113.1", at: base, rate: 1},
		{site: "zzz-scope-b", ip: "203.0.113.2", at: base, rate: 1},
	})

	got, err := store.Overview(context.Background(), []string{"zzz-scope-a"}, base.Add(-time.Minute), base.Add(time.Minute), DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(got) != 1 || got[0].SiteID != "zzz-scope-a" {
		t.Errorf("Overview = %+v, want only zzz-scope-a", got)
	}
}

func TestStore_RealTimescaleDB_PaginationPagesThroughResults(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	marker := "t13d_api_paging"

	rows := make([]seedRow, 0, 5)
	for i := 1; i <= 5; i++ {
		rows = append(rows, seedRow{
			site: "site-page", ip: fmt.Sprintf("203.0.113.%d", i), at: base,
			rate: float64(i), score: int16(i * 10),
		})
	}
	store := newTestStore(t, marker, rows)
	ctx := context.Background()
	from, to := base.Add(-time.Minute), base.Add(time.Minute)

	first, total, err := store.TopIPs(ctx, "site-page", from, to, 2, 0)
	if err != nil {
		t.Fatalf("TopIPs page 1: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5 (the full count, not the page size)", total)
	}
	second, _, err := store.TopIPs(ctx, "site-page", from, to, 2, 2)
	if err != nil {
		t.Fatalf("TopIPs page 2: %v", err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("page sizes = %d and %d, want 2 each", len(first), len(second))
	}
	// Pages must not overlap - which needs a total ordering, hence the
	// tie-breaking ORDER BY column in the query.
	for _, a := range first {
		for _, b := range second {
			if a.IP == b.IP {
				t.Errorf("IP %s appears on both pages; the ordering isn't total", a.IP)
			}
		}
	}
}

func TestStore_RealTimescaleDB_TopIPsOrdersBySuspicion(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	marker := "t13d_api_topips"

	store := newTestStore(t, marker, []seedRow{
		{site: "site-top", ip: "203.0.113.1", at: base, rate: 1, score: 20, country: "US", asn: 111, asnOrg: "AAA"},
		{site: "site-top", ip: "203.0.113.2", at: base, rate: 5, score: 95, country: "DE", asn: 222, asnOrg: "BBB", botJA4: true, botASN: true},
		{site: "site-top", ip: "203.0.113.3", at: base, rate: 2, score: 60, country: "FR", asn: 333, asnOrg: "CCC"},
	})

	ips, _, err := store.TopIPs(context.Background(), "site-top", base.Add(-time.Minute), base.Add(time.Minute), 10, 0)
	if err != nil {
		t.Fatalf("TopIPs: %v", err)
	}
	if len(ips) != 3 {
		t.Fatalf("got %d IPs, want 3", len(ips))
	}
	if ips[0].IP != "203.0.113.2" || ips[1].IP != "203.0.113.3" || ips[2].IP != "203.0.113.1" {
		t.Errorf("order = %s, %s, %s; want highest score first", ips[0].IP, ips[1].IP, ips[2].IP)
	}
	if !ips[0].IsKnownBotJA4 || !ips[0].IsKnownBotASN {
		t.Errorf("top IP lost its known-bot flags: %+v", ips[0])
	}
	if ips[0].Country != "DE" || ips[0].ASN != 222 || ips[0].ASNName != "BBB" {
		t.Errorf("top IP enrichment = %q/%d/%q, want DE/222/BBB", ips[0].Country, ips[0].ASN, ips[0].ASNName)
	}
}

func TestStore_RealTimescaleDB_LimitIsHonoured(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	marker := "t13d_api_limit"

	rows := make([]seedRow, 0, 10)
	for i := 1; i <= 10; i++ {
		rows = append(rows, seedRow{
			site: "site-limit", ip: fmt.Sprintf("203.0.113.%d", i), at: base,
			rate: float64(i), score: int16(i), country: fmt.Sprintf("C%d", i),
		})
	}
	store := newTestStore(t, marker, rows)

	ips, _, err := store.TopIPs(context.Background(), "site-limit", base.Add(-time.Minute), base.Add(time.Minute), 3, 0)
	if err != nil {
		t.Fatalf("TopIPs: %v", err)
	}
	if len(ips) != 3 {
		t.Errorf("TopIPs returned %d rows, want 3", len(ips))
	}
}

func TestStore_RealTimescaleDB_TimeseriesBucketsByInterval(t *testing.T) {
	// Anchored to a whole hour so the rows fall into predictable
	// time_bucket boundaries rather than straddling them by chance.
	base := time.Now().UTC().Truncate(time.Hour).Add(-6 * time.Hour)
	marker := "t13d_api_timeseries"

	store := newTestStore(t, marker, []seedRow{
		{site: "site-ts", ip: "203.0.113.1", at: base.Add(1 * time.Minute), rate: 1, score: 10},
		{site: "site-ts", ip: "203.0.113.2", at: base.Add(2 * time.Minute), rate: 2, score: 90},
		{site: "site-ts", ip: "203.0.113.3", at: base.Add(1*time.Hour + 1*time.Minute), rate: 3, score: 10},
	})

	buckets, err := store.Timeseries(context.Background(), "site-ts", base, base.Add(2*time.Hour), "1 hour", DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("Timeseries: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2 (two distinct hours have data): %+v", len(buckets), buckets)
	}
	if buckets[0].UniqueIPs != 2 || buckets[0].BotIPs != 1 {
		t.Errorf("first bucket = %+v, want 2 unique IPs and 1 bot", buckets[0])
	}
	if buckets[1].UniqueIPs != 1 || buckets[1].BotIPs != 0 {
		t.Errorf("second bucket = %+v, want 1 unique IP and 0 bots", buckets[1])
	}
	if buckets[0].PeakRequestRate != 2 {
		t.Errorf("first bucket PeakRequestRate = %v, want 2", buckets[0].PeakRequestRate)
	}
}

func TestStore_RealTimescaleDB_EmptyRangeReturnsEmptyNotError(t *testing.T) {
	// A site with no traffic in the window is an ordinary answer, not an
	// error - and the slices must serialise as [] rather than null, so
	// the panel doesn't have to special-case them.
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	store := newTestStore(t, "t13d_api_empty", nil)
	ctx := context.Background()
	from, to := base, base.Add(time.Minute)

	summary, err := store.Summary(ctx, "site-nonexistent", from, to, DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary.UniqueIPs != 0 || summary.Snapshots != 0 || summary.PeakRequestRate != 0 {
		t.Errorf("empty summary = %+v, want zeroed counters", summary)
	}
	if summary.PeakWindowRequests != 0 {
		t.Errorf("PeakWindowRequests = %d, want 0 for an empty range", summary.PeakWindowRequests)
	}

	if b, err := store.Timeseries(ctx, "site-nonexistent", from, to, "1 hour", DefaultBotScoreMin); err != nil || b == nil {
		t.Errorf("Timeseries = (%v, %v), want an empty non-nil slice and no error", b, err)
	}
	if ips, _, err := store.TopIPs(ctx, "site-nonexistent", from, to, 10, 0); err != nil || ips == nil {
		t.Errorf("TopIPs = (%v, %v), want an empty non-nil slice and no error", ips, err)
	}
	if c, _, err := store.Countries(ctx, "site-nonexistent", from, to, 10, 0, DefaultBotScoreMin); err != nil || c == nil {
		t.Errorf("Countries = (%v, %v), want an empty non-nil slice and no error", c, err)
	}
	if a, _, err := store.ASNs(ctx, "site-nonexistent", from, to, 10, 0, DefaultBotScoreMin); err != nil || a == nil {
		t.Errorf("ASNs = (%v, %v), want an empty non-nil slice and no error", a, err)
	}
}

func TestStore_RealTimescaleDB_SitesListsDistinctSites(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	marker := "t13d_api_sites"
	store := newTestStore(t, marker, []seedRow{
		{site: "zzz-site-one", ip: "203.0.113.1", at: base, rate: 1},
		{site: "zzz-site-one", ip: "203.0.113.2", at: base, rate: 1},
		{site: "zzz-site-two", ip: "203.0.113.3", at: base, rate: 1},
	})

	sites, err := store.Sites(context.Background())
	if err != nil {
		t.Fatalf("Sites: %v", err)
	}
	seen := map[string]int{}
	for _, s := range sites {
		seen[s]++
	}
	if seen["zzz-site-one"] != 1 || seen["zzz-site-two"] != 1 {
		t.Errorf("Sites() = %v, want each seeded site exactly once", sites)
	}
}
