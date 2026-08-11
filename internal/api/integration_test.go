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
	"testing"
	"time"

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
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM traffic_snapshots WHERE ja4 = $1`, marker); err != nil {
			t.Logf("cleanup: delete failed: %v", err)
		}
	})

	for _, r := range rows {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO traffic_snapshots
			  (time, site_id, ip, ja4, prev_window_count, curr_window_count, request_rate,
			   bot_score, is_known_bot_ja4, country, asn, asn_org, is_known_bot_asn)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			r.at, r.site, r.ip, marker, r.prevWin, r.currWin, r.rate,
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

	countries, err := store.Countries(ctx, "site-a", from, to, 10, DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("Countries: %v", err)
	}
	for _, c := range countries {
		if c.Key == "DE" {
			t.Errorf("site-a countries include DE, which only site-b has: %+v", countries)
		}
	}

	asns, err := store.ASNs(ctx, "site-a", from, to, 10, DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("ASNs: %v", err)
	}
	for _, a := range asns {
		if a.Key == "222" {
			t.Errorf("site-a ASNs include 222, which only site-b has: %+v", asns)
		}
	}

	ips, err := store.TopIPs(ctx, "site-a", from, to, 10)
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
}

func TestStore_RealTimescaleDB_TopIPsOrdersBySuspicion(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	marker := "t13d_api_topips"

	store := newTestStore(t, marker, []seedRow{
		{site: "site-top", ip: "203.0.113.1", at: base, rate: 1, score: 20, country: "US", asn: 111, asnOrg: "AAA"},
		{site: "site-top", ip: "203.0.113.2", at: base, rate: 5, score: 95, country: "DE", asn: 222, asnOrg: "BBB", botJA4: true, botASN: true},
		{site: "site-top", ip: "203.0.113.3", at: base, rate: 2, score: 60, country: "FR", asn: 333, asnOrg: "CCC"},
	})

	ips, err := store.TopIPs(context.Background(), "site-top", base.Add(-time.Minute), base.Add(time.Minute), 10)
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

	ips, err := store.TopIPs(context.Background(), "site-limit", base.Add(-time.Minute), base.Add(time.Minute), 3)
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
	if ips, err := store.TopIPs(ctx, "site-nonexistent", from, to, 10); err != nil || ips == nil {
		t.Errorf("TopIPs = (%v, %v), want an empty non-nil slice and no error", ips, err)
	}
	if c, err := store.Countries(ctx, "site-nonexistent", from, to, 10, DefaultBotScoreMin); err != nil || c == nil {
		t.Errorf("Countries = (%v, %v), want an empty non-nil slice and no error", c, err)
	}
	if a, err := store.ASNs(ctx, "site-nonexistent", from, to, 10, DefaultBotScoreMin); err != nil || a == nil {
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
