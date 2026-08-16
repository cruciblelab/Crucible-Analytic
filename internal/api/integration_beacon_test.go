//go:build integration

// Real coverage of the beacon and cross-source query layers against a
// live TimescaleDB, gated behind the "integration" build tag like the
// rest. These exercise what server_beacon_test.go's fake store cannot:
// that the SQL is valid, that sessionization actually splits on the
// timeout, that the country fallback join fires, and that site_id
// isolates one customer's client-side data from another's. Run with:
//
//	docker compose up -d
//	psql "$DSN" -f internal/storage/schema.sql
//	psql "$DSN" -f internal/beacon/schema.sql
//	go test -tags integration ./internal/api/... -v

package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// beaconSeed is one beacon_events row to insert for a test.
type beaconSeed struct {
	site      string
	visitor   string
	at        time.Time
	eventType string
	eventName string
	path      string
	query     string
	referrer  string
	ip        string
	browser   string
	os        string
	device    string
	botUA     bool
	language  string
	country   string

	// Campaign dimensions. Left empty by tests that do not care, which
	// is the same "no acquisition context" a plain visit produces.
	utmSource   string
	utmMedium   string
	utmCampaign string
	utmTerm     string
	utmContent  string
	ref         string
	clickSource string
}

// seedBeacon opens a Store, inserts beacon rows, and deletes exactly the
// site_ids it seeded afterwards. Every test here uses a site_id unique
// to itself, so cleanup can be exact.
func seedBeacon(t *testing.T, rows []beaconSeed) *Store {
	t.Helper()

	store, err := NewStore(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("NewStore: %v (is docker compose up, with both schemas applied?)", err)
	}
	t.Cleanup(store.Close)

	pool, err := pgxpool.New(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	seeded := map[string]struct{}{}
	for _, r := range rows {
		seeded[r.site] = struct{}{}
	}
	t.Cleanup(func() {
		for site := range seeded {
			if _, err := pool.Exec(context.Background(), `DELETE FROM beacon_events WHERE site_id = $1`, site); err != nil {
				t.Logf("cleanup: deleting beacon rows for %s failed: %v", site, err)
			}
			if _, err := pool.Exec(context.Background(), `DELETE FROM traffic_snapshots WHERE site_id = $1`, site); err != nil {
				t.Logf("cleanup: deleting snapshot rows for %s failed: %v", site, err)
			}
		}
	})

	for _, r := range rows {
		eventType := r.eventType
		if eventType == "" {
			eventType = "pageview"
		}
		path := r.path
		if path == "" {
			path = "/"
		}
		// INET has no empty value, so a test that doesn't care which
		// address an event came from still needs one. Tests that do care
		// - the crossover ones, which join on it - always set it.
		ip := r.ip
		if ip == "" {
			ip = "203.0.113.1"
		}
		_, err := pool.Exec(context.Background(), `
			INSERT INTO beacon_events
			  (time, site_id, visitor_id, event_type, event_name, path, query, title,
			   referrer_host, referrer_path, ip, browser, os, device, is_bot_ua,
			   screen_w, screen_h, language, country, asn, asn_org,
			   utm_source, utm_medium, utm_campaign, utm_term, utm_content, ref, click_source)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'',$8,'',$9,$10,$11,$12,$13,1920,1080,$14,$15,0,'',
			        $16,$17,$18,$19,$20,$21,$22)`,
			r.at, r.site, r.visitor, eventType, r.eventName, path, r.query,
			r.referrer, ip, r.browser, r.os, r.device, r.botUA, r.language, r.country,
			r.utmSource, r.utmMedium, r.utmCampaign, r.utmTerm, r.utmContent, r.ref, r.clickSource)
		if err != nil {
			t.Fatalf("seeding beacon row %+v: %v", r, err)
		}
	}
	return store
}

// seedSnapshotsFor inserts traffic_snapshots rows into a test that
// seedBeacon already set up cleanup for.
func seedSnapshotsFor(t *testing.T, rows []seedRow) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()

	for _, r := range rows {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO traffic_snapshots
			  (time, site_id, ip, ja4, prev_window_count, curr_window_count, request_rate,
			   bot_score, is_known_bot_ja4, country, asn, asn_org, is_known_bot_asn)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
			r.at, r.site, r.ip, r.ja4, r.prevWin, r.currWin, r.rate,
			r.score, r.botJA4, r.country, r.asn, r.asnOrg, r.botASN)
		if err != nil {
			t.Fatalf("seeding snapshot row %+v: %v", r, err)
		}
	}
}

func testBeaconParams(from, to time.Time) beaconParams {
	return beaconParams{from: from, to: to, limit: 50, offset: 0, bots: BotsExclude}
}

// The core of the whole read layer: a session is one visitor's events
// with gaps under the timeout, and nothing but a real window function
// over real rows proves the split happens where it should.
func TestStore_RealTimescaleDB_SessionsSplitOnTheTimeout(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-4 * time.Hour)
	site := "api-beacon-sessions"

	store := seedBeacon(t, []beaconSeed{
		// One visitor, three events. The first two are 5 minutes apart
		// (one session); the third is 40 minutes later (a second).
		{site: site, visitor: "v1", at: base, path: "/a"},
		{site: site, visitor: "v1", at: base.Add(5 * time.Minute), path: "/b"},
		{site: site, visitor: "v1", at: base.Add(45 * time.Minute), path: "/c"},
		// A second visitor with a single event: one session, and a
		// bounce.
		{site: site, visitor: "v2", at: base.Add(time.Minute), path: "/a"},
	})

	got, err := store.BeaconSummary(context.Background(), site, base.Add(-time.Minute), base.Add(2*time.Hour), BotsExclude, campaignFilter{})
	if err != nil {
		t.Fatalf("BeaconSummary: %v", err)
	}

	if got.Pageviews != 4 {
		t.Errorf("Pageviews = %d, want 4", got.Pageviews)
	}
	if got.Visitors != 2 {
		t.Errorf("Visitors = %d, want 2", got.Visitors)
	}
	if got.Sessions != 3 {
		t.Errorf("Sessions = %d, want 3 (v1 split by the 40-minute gap, plus v2)", got.Sessions)
	}
	// v1's second session (one pageview) and v2's only session bounce;
	// v1's first session has two pageviews and does not.
	if got.BouncedSessions != 2 {
		t.Errorf("BouncedSessions = %d, want 2", got.BouncedSessions)
	}
	if want := 2.0 / 3.0; got.BounceRate < want-0.001 || got.BounceRate > want+0.001 {
		t.Errorf("BounceRate = %v, want %v", got.BounceRate, want)
	}
	// Durations: 300s (v1 first), 0 (v1 second), 0 (v2) -> mean 100.
	if got.AvgSessionSeconds < 99.9 || got.AvgSessionSeconds > 100.1 {
		t.Errorf("AvgSessionSeconds = %v, want 100", got.AvgSessionSeconds)
	}
}

func TestStore_RealTimescaleDB_CustomEventsAreCountedSeparately(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-beacon-events"

	store := seedBeacon(t, []beaconSeed{
		{site: site, visitor: "v1", at: base, path: "/pricing"},
		{site: site, visitor: "v1", at: base.Add(time.Minute), eventType: "event", eventName: "signup", path: "/pricing"},
		{site: site, visitor: "v2", at: base.Add(2 * time.Minute), eventType: "event", eventName: "signup", path: "/pricing"},
		{site: site, visitor: "v2", at: base.Add(3 * time.Minute), eventType: "event", eventName: "share", path: "/pricing"},
	})
	from, to := base.Add(-time.Minute), base.Add(time.Hour)

	summary, err := store.BeaconSummary(context.Background(), site, from, to, BotsExclude, campaignFilter{})
	if err != nil {
		t.Fatalf("BeaconSummary: %v", err)
	}
	if summary.Pageviews != 1 {
		t.Errorf("Pageviews = %d, want 1 - custom events must not inflate it", summary.Pageviews)
	}
	if summary.Events != 3 {
		t.Errorf("Events = %d, want 3", summary.Events)
	}

	events, total, err := store.BeaconEvents(context.Background(), site, testBeaconParams(from, to))
	if err != nil {
		t.Fatalf("BeaconEvents: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2 distinct event names", total)
	}
	if len(events) != 2 || events[0].Name != "signup" || events[0].Count != 2 || events[0].Visitors != 2 {
		t.Errorf("events = %+v, want signup first with 2 raises from 2 visitors", events)
	}
}

// bots=exclude is the default, so an error here would silently change
// every headline number a panel shows.
func TestStore_RealTimescaleDB_BotFilterSelectsThePopulation(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-beacon-botfilter"

	store := seedBeacon(t, []beaconSeed{
		{site: site, visitor: "human1", at: base, path: "/a"},
		{site: site, visitor: "human2", at: base.Add(time.Minute), path: "/a"},
		{site: site, visitor: "crawler", at: base.Add(2 * time.Minute), path: "/a", botUA: true},
	})
	from, to := base.Add(-time.Minute), base.Add(time.Hour)

	for _, tc := range []struct {
		bots          BotFilter
		wantPageviews int
	}{
		{BotsExclude, 2},
		{BotsInclude, 3},
		{BotsOnly, 1},
	} {
		got, err := store.BeaconSummary(context.Background(), site, from, to, tc.bots, campaignFilter{})
		if err != nil {
			t.Fatalf("BeaconSummary(%s): %v", tc.bots, err)
		}
		if got.Pageviews != tc.wantPageviews {
			t.Errorf("bots=%s -> Pageviews %d, want %d", tc.bots, got.Pageviews, tc.wantPageviews)
		}
		if got.Bots != string(tc.bots) {
			t.Errorf("bots=%s not echoed back (got %q)", tc.bots, got.Bots)
		}
	}
}

func TestStore_RealTimescaleDB_BreakdownsGroupAndRank(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-beacon-breakdowns"

	store := seedBeacon(t, []beaconSeed{
		{site: site, visitor: "v1", at: base, path: "/popular", browser: "Chrome", os: "Windows", device: "desktop", language: "tr-TR", referrer: "google.com"},
		{site: site, visitor: "v2", at: base.Add(time.Minute), path: "/popular", browser: "Chrome", os: "Windows", device: "desktop", language: "tr-TR", referrer: "google.com"},
		{site: site, visitor: "v3", at: base.Add(2 * time.Minute), path: "/rare", browser: "Firefox", os: "Linux", device: "desktop", language: "en-US"},
	})
	p := testBeaconParams(base.Add(-time.Minute), base.Add(time.Hour))
	ctx := context.Background()

	pages, total, err := store.BeaconPages(ctx, site, p)
	if err != nil {
		t.Fatalf("BeaconPages: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2 distinct paths", total)
	}
	if len(pages) != 2 || pages[0].Key != "/popular" || pages[0].Pageviews != 2 || pages[0].Visitors != 2 {
		t.Errorf("pages = %+v, want /popular first with 2 views from 2 visitors", pages)
	}

	browsers, _, err := store.BeaconBrowsers(ctx, site, p)
	if err != nil {
		t.Fatalf("BeaconBrowsers: %v", err)
	}
	if len(browsers) != 2 || browsers[0].Key != "Chrome" || browsers[0].Pageviews != 2 {
		t.Errorf("browsers = %+v", browsers)
	}

	// The empty group must be present and flagged, not dropped - v3 had
	// no referrer, and losing that row would stop the numbers adding up.
	referrers, _, err := store.BeaconReferrers(ctx, site, p)
	if err != nil {
		t.Fatalf("BeaconReferrers: %v", err)
	}
	var sawEmpty bool
	sum := 0
	for _, r := range referrers {
		sum += r.Pageviews
		if r.Key == "" {
			sawEmpty = true
			if !r.Empty {
				t.Error("the empty referrer group is not flagged Empty")
			}
		}
	}
	if !sawEmpty {
		t.Errorf("no empty referrer group in %+v - the direct visit was dropped", referrers)
	}
	if sum != 3 {
		t.Errorf("referrer pageviews sum to %d, want 3", sum)
	}

	for name, query := range map[string]func(context.Context, string, beaconParams) ([]BeaconGroupStat, int, error){
		"operating systems": store.BeaconOperatingSystems,
		"devices":           store.BeaconDevices,
		"languages":         store.BeaconLanguages,
	} {
		stats, _, err := query(ctx, site, p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := 0
		for _, s := range stats {
			got += s.Pageviews
		}
		if got != 3 {
			t.Errorf("%s pageviews sum to %d, want 3", name, got)
		}
	}
}

func TestStore_RealTimescaleDB_EntryAndExitPages(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-beacon-entryexit"

	store := seedBeacon(t, []beaconSeed{
		// One session: landing -> middle -> checkout.
		{site: site, visitor: "v1", at: base, path: "/landing"},
		{site: site, visitor: "v1", at: base.Add(time.Minute), path: "/middle"},
		{site: site, visitor: "v1", at: base.Add(2 * time.Minute), path: "/checkout"},
		// A second visitor entering and leaving on the same page.
		{site: site, visitor: "v2", at: base.Add(time.Minute), path: "/landing"},
	})
	p := testBeaconParams(base.Add(-time.Minute), base.Add(time.Hour))
	ctx := context.Background()

	entries, total, err := store.BeaconEntryPages(ctx, site, p)
	if err != nil {
		t.Fatalf("BeaconEntryPages: %v", err)
	}
	if total != 1 || len(entries) != 1 || entries[0].Path != "/landing" || entries[0].Sessions != 2 {
		t.Errorf("entry pages = %+v (total %d), want /landing with 2 sessions", entries, total)
	}

	exits, _, err := store.BeaconExitPages(ctx, site, p)
	if err != nil {
		t.Fatalf("BeaconExitPages: %v", err)
	}
	// v1 left on /checkout, v2 on /landing. If the ordering direction
	// were wrong, both would report /landing.
	byPath := map[string]int{}
	for _, e := range exits {
		byPath[e.Path] = e.Sessions
	}
	if byPath["/checkout"] != 1 || byPath["/landing"] != 1 {
		t.Errorf("exit pages = %+v, want one session each on /checkout and /landing", exits)
	}
	if _, ok := byPath["/middle"]; ok {
		t.Error("/middle is neither an entry nor an exit; it must not appear")
	}
}

// The recommended deployment leaves the beacon's own geo lookup off,
// so this fallback is the normal path rather than an edge case: without
// it the countries endpoint would return one large empty group for
// every correctly-configured install.
func TestStore_RealTimescaleDB_CountriesFallBackToTheCollector(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-beacon-geo"

	store := seedBeacon(t, []beaconSeed{
		// No country of its own - beacon asn_lookup disabled.
		{site: site, visitor: "v1", at: base, ip: "203.0.113.10", path: "/a"},
		{site: site, visitor: "v2", at: base.Add(time.Minute), ip: "203.0.113.11", path: "/a"},
		// This one carries its own country and must keep it.
		{site: site, visitor: "v3", at: base.Add(2 * time.Minute), ip: "203.0.113.12", path: "/a", country: "DE"},
	})
	seedSnapshotsFor(t, []seedRow{
		{site: site, ip: "203.0.113.10", at: base, rate: 1, score: 5, country: "TR"},
		{site: site, ip: "203.0.113.11", at: base, rate: 1, score: 5, country: "TR"},
		// Deliberately contradicts the beacon's own value, which must win.
		{site: site, ip: "203.0.113.12", at: base, rate: 1, score: 5, country: "FR"},
	})

	countries, _, err := store.BeaconCountries(context.Background(), site, testBeaconParams(base.Add(-time.Minute), base.Add(time.Hour)))
	if err != nil {
		t.Fatalf("BeaconCountries: %v", err)
	}

	byKey := map[string]int{}
	for _, c := range countries {
		byKey[c.Key] = c.Pageviews
	}
	if byKey["TR"] != 2 {
		t.Errorf("TR = %d, want 2 recovered from traffic_snapshots; got %+v", byKey["TR"], countries)
	}
	if byKey["DE"] != 1 {
		t.Errorf("DE = %d, want 1 - the beacon's own value must take precedence over the collector's", byKey["DE"])
	}
	if byKey["FR"] != 0 {
		t.Error("the collector's country overrode the beacon's own; the beacon's must win")
	}
}

func TestStore_RealTimescaleDB_CampaignsDecodeTheirParameters(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-beacon-campaigns"

	store := seedBeacon(t, []beaconSeed{
		{site: site, visitor: "v1", at: base, path: "/", query: "utm_medium=email&utm_source=newsletter"},
		{site: site, visitor: "v2", at: base.Add(time.Minute), path: "/", query: "utm_medium=email&utm_source=newsletter"},
		{site: site, visitor: "v3", at: base.Add(2 * time.Minute), path: "/", query: "utm_source=twitter"},
		// No campaign at all: must not appear as an enormous empty row.
		{site: site, visitor: "v4", at: base.Add(3 * time.Minute), path: "/"},
	})

	campaigns, total, err := store.BeaconCampaigns(context.Background(), site, testBeaconParams(base.Add(-time.Minute), base.Add(time.Hour)))
	if err != nil {
		t.Fatalf("BeaconCampaigns: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2 distinct campaigns (the uncampaigned visit excluded)", total)
	}
	if len(campaigns) != 2 {
		t.Fatalf("campaigns = %+v, want 2", campaigns)
	}
	if campaigns[0].Pageviews != 2 {
		t.Errorf("busiest campaign has %d pageviews, want 2", campaigns[0].Pageviews)
	}
	if got := campaigns[0].Params["utm_source"]; got != "newsletter" {
		t.Errorf("utm_source decoded as %q, want newsletter (params: %v)", got, campaigns[0].Params)
	}
	if got := campaigns[0].Params["utm_medium"]; got != "email" {
		t.Errorf("utm_medium decoded as %q, want email", got)
	}
	for _, c := range campaigns {
		if c.Key == "" {
			t.Error("an empty campaign row leaked into the results")
		}
	}
}

func TestStore_RealTimescaleDB_TimeseriesBucketsSessionsByTheirStart(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Hour).Add(-6 * time.Hour)
	site := "api-beacon-timeseries"

	store := seedBeacon(t, []beaconSeed{
		{site: site, visitor: "v1", at: base.Add(10 * time.Minute), path: "/a"},
		{site: site, visitor: "v1", at: base.Add(20 * time.Minute), path: "/b"},
		{site: site, visitor: "v2", at: base.Add(90 * time.Minute), path: "/a"},
	})

	buckets, err := store.BeaconTimeseries(context.Background(), site, base, base.Add(4*time.Hour), "1 hour", BotsExclude, campaignFilter{})
	if err != nil {
		t.Fatalf("BeaconTimeseries: %v", err)
	}
	if len(buckets) != 2 {
		t.Fatalf("got %d buckets, want 2: %+v", len(buckets), buckets)
	}
	if buckets[0].Pageviews != 2 || buckets[0].Visitors != 1 || buckets[0].Sessions != 1 {
		t.Errorf("first bucket = %+v, want 2 pageviews / 1 visitor / 1 session", buckets[0])
	}
	if buckets[1].Pageviews != 1 || buckets[1].Sessions != 1 {
		t.Errorf("second bucket = %+v, want 1 pageview / 1 session", buckets[1])
	}
}

// The endpoint that justifies running two collectors: of everything that
// reached the site, how much actually rendered a page.
func TestStore_RealTimescaleDB_CrossoverMeasuresJSCoverage(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-crossover-coverage"

	store := seedBeacon(t, []beaconSeed{
		// Two of the four addresses below ran JavaScript.
		{site: site, visitor: "v1", at: base, ip: "203.0.113.20", path: "/"},
		{site: site, visitor: "v2", at: base, ip: "203.0.113.21", path: "/"},
	})
	seedSnapshotsFor(t, []seedRow{
		{site: site, ip: "203.0.113.20", at: base, rate: 1, score: 5},   // human, ran JS
		{site: site, ip: "203.0.113.21", at: base, rate: 1, score: 15},  // human, ran JS
		{site: site, ip: "203.0.113.22", at: base, rate: 50, score: 95}, // scraper, silent
		{site: site, ip: "203.0.113.23", at: base, rate: 20, score: 85}, // scraper, silent
	})

	got, err := store.CrossoverSummary(context.Background(), site, base.Add(-time.Minute), base.Add(time.Hour))
	if err != nil {
		t.Fatalf("CrossoverSummary: %v", err)
	}

	if got.IPsSeen != 4 || got.IPsRanJS != 2 || got.IPsSilent != 2 {
		t.Errorf("seen/ranJS/silent = %d/%d/%d, want 4/2/2", got.IPsSeen, got.IPsRanJS, got.IPsSilent)
	}
	if got.JSCoverage != 0.5 {
		t.Errorf("JSCoverage = %v, want 0.5", got.JSCoverage)
	}
	if got.BeaconOnlyIPs != 0 {
		t.Errorf("BeaconOnlyIPs = %d, want 0 in a correct deployment", got.BeaconOnlyIPs)
	}
	if len(got.Bands) != 10 {
		t.Fatalf("got %d bands, want all 10 present including empty ones", len(got.Bands))
	}
	// Scores 5 and 15 fall in bands 0 and 1 with full coverage; 85 and 95
	// fall in bands 8 and 9 with none. That downward slope is the shape
	// the whole endpoint exists to show.
	if got.Bands[0].IPsRanJS != 1 || got.Bands[0].JSCoverage != 1 {
		t.Errorf("band 0-9 = %+v, want 1 IP with full JS coverage", got.Bands[0])
	}
	if got.Bands[9].IPsSeen != 1 || got.Bands[9].IPsRanJS != 0 || got.Bands[9].JSCoverage != 0 {
		t.Errorf("band 90-100 = %+v, want 1 IP with no JS coverage", got.Bands[9])
	}
	if got.Bands[9].Max != 100 {
		t.Errorf("top band Max = %d, want 100 so a perfect score has somewhere to go", got.Bands[9].Max)
	}
}

// A non-zero BeaconOnlyIPs is a real misconfiguration signal - usually
// the beacon recording a proxy's address instead of the visitor's - so
// it has to be reported rather than quietly folded away.
func TestStore_RealTimescaleDB_CrossoverFlagsBeaconOnlyAddresses(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-crossover-beacononly"

	store := seedBeacon(t, []beaconSeed{
		{site: site, visitor: "v1", at: base, ip: "198.51.100.5", path: "/"},
	})
	seedSnapshotsFor(t, []seedRow{
		{site: site, ip: "203.0.113.30", at: base, rate: 1, score: 5},
	})

	got, err := store.CrossoverSummary(context.Background(), site, base.Add(-time.Minute), base.Add(time.Hour))
	if err != nil {
		t.Fatalf("CrossoverSummary: %v", err)
	}
	if got.BeaconOnlyIPs != 1 {
		t.Errorf("BeaconOnlyIPs = %d, want 1", got.BeaconOnlyIPs)
	}
	if got.IPsRanJS != 0 {
		t.Errorf("IPsRanJS = %d, want 0 - the beacon address matched no collector address", got.IPsRanJS)
	}
}

func TestStore_RealTimescaleDB_SilentIPsListsWhatNeverRanJS(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-crossover-silent"

	store := seedBeacon(t, []beaconSeed{
		{site: site, visitor: "v1", at: base, ip: "203.0.113.40", path: "/"},
	})
	seedSnapshotsFor(t, []seedRow{
		{site: site, ip: "203.0.113.40", at: base, rate: 1, score: 10, ja4: "x"},
		{site: site, ip: "203.0.113.41", at: base, rate: 30, score: 92, ja4: "x", country: "US"},
		{site: site, ip: "203.0.113.42", at: base, rate: 10, score: 60, ja4: "x"},
	})

	silent, total, err := store.SilentIPs(context.Background(), site, base.Add(-time.Minute), base.Add(time.Hour), 50, 0)
	if err != nil {
		t.Fatalf("SilentIPs: %v", err)
	}
	if total != 2 || len(silent) != 2 {
		t.Fatalf("got %d silent IPs (total %d), want 2: %+v", len(silent), total, silent)
	}
	// Most suspicious first.
	if silent[0].IP != "203.0.113.41" || silent[0].PeakScore != 92 {
		t.Errorf("first silent IP = %+v, want 203.0.113.41 at score 92", silent[0])
	}
	for _, s := range silent {
		if s.IP == "203.0.113.40" {
			t.Error("an IP that did run JavaScript was listed as silent")
		}
	}
}

// The list a conventional analytics tool cannot produce: clients that
// render pages and are automated anyway.
func TestStore_RealTimescaleDB_JSBotsFindsAutomationThatRendersPages(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-crossover-jsbots"

	store := seedBeacon(t, []beaconSeed{
		// Ordinary visitor: low score, honest user agent. Must not appear.
		{site: site, visitor: "human", at: base, ip: "203.0.113.50", path: "/", browser: "Chrome"},
		// Headless browser: honest about itself.
		{site: site, visitor: "headless", at: base, ip: "203.0.113.51", path: "/", browser: "Headless Chrome", botUA: true},
		// The interesting one: claims to be a normal browser, but the
		// collector scored its behavior as automated.
		{site: site, visitor: "stealth", at: base, ip: "203.0.113.52", path: "/", browser: "Chrome"},
		{site: site, visitor: "stealth", at: base.Add(time.Minute), ip: "203.0.113.52", path: "/b", browser: "Chrome"},
	})
	seedSnapshotsFor(t, []seedRow{
		{site: site, ip: "203.0.113.50", at: base, rate: 1, score: 5, ja4: "x"},
		{site: site, ip: "203.0.113.51", at: base, rate: 2, score: 10, ja4: "x"},
		{site: site, ip: "203.0.113.52", at: base, rate: 40, score: 88, ja4: "x", country: "US", asn: 16509, asnOrg: "AMAZON"},
	})

	bots, total, err := store.JSBots(context.Background(), site, base.Add(-time.Minute), base.Add(time.Hour), 50, 0, DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("JSBots: %v", err)
	}
	if total != 2 || len(bots) != 2 {
		t.Fatalf("got %d js bots (total %d), want 2: %+v", len(bots), total, bots)
	}

	// Highest collector score first: the stealthy one, which is the
	// whole point - nothing client-side distinguished it.
	if bots[0].IP != "203.0.113.52" || bots[0].PeakScore != 88 {
		t.Errorf("first js bot = %+v, want 203.0.113.52 at score 88", bots[0])
	}
	if bots[0].IsBotUA {
		t.Error("the stealthy client's user agent should not be flagged; only its behavior gave it away")
	}
	if bots[0].Pageviews != 2 || bots[0].ASNName != "AMAZON" {
		t.Errorf("first js bot lost its beacon or collector detail: %+v", bots[0])
	}
	if bots[1].IP != "203.0.113.51" || !bots[1].IsBotUA {
		t.Errorf("second js bot = %+v, want the self-identified headless browser", bots[1])
	}
	for _, b := range bots {
		if b.IP == "203.0.113.50" {
			t.Error("an ordinary visitor was listed as a JS bot")
		}
	}
}

// The security property every per-site query has to hold: one
// customer's client-side data must never reach another's response, even
// when both sites saw the same visitor at the same instant.
func TestStore_RealTimescaleDB_BeaconSiteIsolation(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	siteA, siteB := "api-beacon-iso-a", "api-beacon-iso-b"

	store := seedBeacon(t, []beaconSeed{
		{site: siteA, visitor: "shared", at: base, ip: "203.0.113.60", path: "/a-only", browser: "Chrome", referrer: "a.example", language: "tr-TR", query: "utm_source=a"},
		{site: siteB, visitor: "shared", at: base, ip: "203.0.113.60", path: "/b-only", browser: "Firefox", referrer: "b.example", language: "en-US", query: "utm_source=b"},
		{site: siteB, visitor: "shared", at: base.Add(time.Minute), ip: "203.0.113.60", path: "/b-only", eventType: "event", eventName: "b-event"},
	})
	seedSnapshotsFor(t, []seedRow{
		{site: siteA, ip: "203.0.113.60", at: base, rate: 1, score: 5, ja4: "x"},
		{site: siteB, ip: "203.0.113.60", at: base, rate: 1, score: 5, ja4: "x"},
		{site: siteB, ip: "203.0.113.61", at: base, rate: 9, score: 99, ja4: "x"},
	})
	ctx := context.Background()
	from, to := base.Add(-time.Minute), base.Add(time.Hour)
	p := testBeaconParams(from, to)

	summary, err := store.BeaconSummary(ctx, siteA, from, to, BotsExclude, campaignFilter{})
	if err != nil {
		t.Fatalf("BeaconSummary: %v", err)
	}
	if summary.Pageviews != 1 || summary.Events != 0 {
		t.Errorf("site A summary = %+v, want 1 pageview and 0 events", summary)
	}

	pages, _, err := store.BeaconPages(ctx, siteA, p)
	if err != nil {
		t.Fatalf("BeaconPages: %v", err)
	}
	for _, page := range pages {
		if page.Key == "/b-only" {
			t.Error("site B's path leaked into site A's pages")
		}
	}

	for name, query := range map[string]func(context.Context, string, beaconParams) ([]BeaconGroupStat, int, error){
		"browsers":  store.BeaconBrowsers,
		"referrers": store.BeaconReferrers,
		"languages": store.BeaconLanguages,
		"countries": store.BeaconCountries,
	} {
		stats, _, err := query(ctx, siteA, p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		total := 0
		for _, s := range stats {
			total += s.Pageviews
		}
		if total != 1 {
			t.Errorf("site A %s sums to %d pageviews, want 1: %+v", name, total, stats)
		}
	}

	campaigns, _, err := store.BeaconCampaigns(ctx, siteA, p)
	if err != nil {
		t.Fatalf("BeaconCampaigns: %v", err)
	}
	if len(campaigns) != 1 || campaigns[0].Params["utm_source"] != "a" {
		t.Errorf("site A campaigns = %+v, want only utm_source=a", campaigns)
	}

	events, _, err := store.BeaconEvents(ctx, siteA, p)
	if err != nil {
		t.Fatalf("BeaconEvents: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("site A events = %+v, want none - the custom event belongs to site B", events)
	}

	raw, rawTotal, err := store.BeaconRaw(ctx, siteA, p)
	if err != nil {
		t.Fatalf("BeaconRaw: %v", err)
	}
	if rawTotal != 1 || len(raw) != 1 || raw[0].Path != "/a-only" {
		t.Errorf("site A raw = %+v (total %d), want only /a-only", raw, rawTotal)
	}

	// Crossover reads both tables, so it has two chances to leak.
	cross, err := store.CrossoverSummary(ctx, siteA, from, to)
	if err != nil {
		t.Fatalf("CrossoverSummary: %v", err)
	}
	if cross.IPsSeen != 1 || cross.IPsRanJS != 1 || cross.IPsSilent != 0 {
		t.Errorf("site A crossover = %+v, want 1 seen / 1 ran JS / 0 silent", cross)
	}

	silent, silentTotal, err := store.SilentIPs(ctx, siteA, from, to, 50, 0)
	if err != nil {
		t.Fatalf("SilentIPs: %v", err)
	}
	if silentTotal != 0 || len(silent) != 0 {
		t.Errorf("site A silent IPs = %+v, want none - 203.0.113.61 belongs to site B", silent)
	}
}

func TestStore_RealTimescaleDB_BeaconPaginationPagesThroughResults(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-beacon-paging"

	rows := []beaconSeed{}
	for i := range 5 {
		rows = append(rows, beaconSeed{
			site: site, visitor: fmt.Sprintf("v%d", i),
			at: base.Add(time.Duration(i) * time.Minute), path: fmt.Sprintf("/page-%d", i),
		})
	}
	store := seedBeacon(t, rows)
	ctx := context.Background()
	from, to := base.Add(-time.Minute), base.Add(time.Hour)

	first, total, err := store.BeaconPages(ctx, site, beaconParams{from: from, to: to, limit: 2, offset: 0, bots: BotsExclude})
	if err != nil {
		t.Fatalf("BeaconPages page 1: %v", err)
	}
	if total != 5 {
		t.Errorf("total = %d, want 5", total)
	}
	second, _, err := store.BeaconPages(ctx, site, beaconParams{from: from, to: to, limit: 2, offset: 2, bots: BotsExclude})
	if err != nil {
		t.Fatalf("BeaconPages page 2: %v", err)
	}

	seen := map[string]bool{}
	for _, p := range append(first, second...) {
		if seen[p.Key] {
			t.Errorf("%q appeared on both pages - the ordering is not a total order", p.Key)
		}
		seen[p.Key] = true
	}
	if len(seen) != 4 {
		t.Errorf("two pages of 2 yielded %d distinct paths, want 4", len(seen))
	}
}

func TestStore_RealTimescaleDB_BeaconSitesListsOnlyBeaconSites(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-beacon-siteslist"

	store := seedBeacon(t, []beaconSeed{{site: site, visitor: "v1", at: base, path: "/"}})

	sites, err := store.BeaconSites(context.Background())
	if err != nil {
		t.Fatalf("BeaconSites: %v", err)
	}
	found := false
	for _, s := range sites {
		if s == site {
			found = true
		}
	}
	if !found {
		t.Errorf("BeaconSites did not include %q; got %v", site, sites)
	}
}

func TestStore_RealTimescaleDB_EmptyRangeIsZeroNotAnError(t *testing.T) {
	// A site with no beacon data at all must produce zeroes and empty
	// lists, not errors or nulls - a panel showing a brand new site
	// should render an empty dashboard, not an error page.
	store := seedBeacon(t, nil)
	ctx := context.Background()
	from := time.Now().UTC().Add(-time.Hour)
	to := time.Now().UTC()
	p := testBeaconParams(from, to)

	summary, err := store.BeaconSummary(ctx, "api-beacon-nonexistent", from, to, BotsExclude, campaignFilter{})
	if err != nil {
		t.Fatalf("BeaconSummary on an empty site: %v", err)
	}
	if summary.Pageviews != 0 || summary.Sessions != 0 || summary.BounceRate != 0 || summary.AvgSessionSeconds != 0 {
		t.Errorf("empty summary = %+v, want all zeroes", summary)
	}

	pages, total, err := store.BeaconPages(ctx, "api-beacon-nonexistent", p)
	if err != nil {
		t.Fatalf("BeaconPages on an empty site: %v", err)
	}
	if total != 0 || len(pages) != 0 {
		t.Errorf("empty pages = %+v (total %d)", pages, total)
	}

	cross, err := store.CrossoverSummary(ctx, "api-beacon-nonexistent", from, to)
	if err != nil {
		t.Fatalf("CrossoverSummary on an empty site: %v", err)
	}
	if cross.IPsSeen != 0 || cross.JSCoverage != 0 || len(cross.Bands) != 10 {
		t.Errorf("empty crossover = %+v, want zeroes but all 10 bands", cross)
	}
}
