//go:build integration

// Real coverage of the campaign dimensions against a live TimescaleDB.
// These are the tests that matter most for this change: the unit tests
// prove a URL is split correctly, and only these prove the split is
// actually groupable and filterable in SQL. Run with:
//
//	docker compose up -d
//	psql "$DSN" -f internal/beacon/schema.sql
//	go test ./internal/api/ -tags integration

package api

import (
	"context"
	"testing"
	"time"
)

// campaignSeed builds the scenario every test in this file reads:
// one source (instagram) spread across two campaigns, a second source,
// a paid click, and traffic with no acquisition context at all.
//
// The spread matters. It is the exact shape the old storage could not
// answer: grouping by the stored query string put instagram/bahar and
// instagram/kis in separate rows, so "how much did Instagram bring"
// had no answer.
func campaignSeed(t *testing.T, site string) (*Store, beaconParams) {
	t.Helper()
	base := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)

	store := seedBeacon(t, []beaconSeed{
		{site: site, visitor: "v1", at: base, path: "/a",
			utmSource: "instagram", utmMedium: "social", utmCampaign: "bahar"},
		{site: site, visitor: "v2", at: base.Add(time.Minute), path: "/b",
			utmSource: "instagram", utmMedium: "social", utmCampaign: "bahar"},
		{site: site, visitor: "v3", at: base.Add(2 * time.Minute), path: "/c",
			utmSource: "instagram", utmMedium: "social", utmCampaign: "kis"},
		{site: site, visitor: "v4", at: base.Add(3 * time.Minute), path: "/a",
			utmSource: "bulten", utmMedium: "email", utmCampaign: "haftalik"},
		{site: site, visitor: "v5", at: base.Add(4 * time.Minute), path: "/a",
			clickSource: "google", utmSource: "google", utmMedium: "cpc"},
		// No campaign at all - the overwhelming majority of real traffic.
		{site: site, visitor: "v6", at: base.Add(5 * time.Minute), path: "/a"},
	})

	p := beaconParams{
		from:  base.Add(-time.Hour),
		to:    base.Add(time.Hour),
		limit: 50,
		bots:  BotsExclude,
	}
	return store, p
}

func groupCount(t *testing.T, stats []BeaconGroupStat, key string) (pageviews, visitors int, found bool) {
	t.Helper()
	for _, s := range stats {
		if s.Key == key {
			return s.Pageviews, s.Visitors, true
		}
	}
	return 0, 0, false
}

// The headline reason typed columns exist.
func TestBeaconUTMSources_AggregatesOneSourceAcrossItsCampaigns(t *testing.T) {
	store, p := campaignSeed(t, "camp-sources")

	stats, total, err := store.BeaconUTMSources(context.Background(), "camp-sources", p)
	if err != nil {
		t.Fatalf("BeaconUTMSources: %v", err)
	}

	pv, visitors, found := groupCount(t, stats, "instagram")
	if !found {
		t.Fatalf("no instagram group in %+v", stats)
	}
	// Three events across two different campaigns, counted as one source.
	if pv != 3 || visitors != 3 {
		t.Errorf("instagram = %d pageviews / %d visitors, want 3/3 (bahar+bahar+kis aggregated)", pv, visitors)
	}

	// The empty group is the no-campaign traffic, and it must be present
	// rather than silently dropped: "how much traffic had no campaign"
	// is a real question.
	if _, _, ok := groupCount(t, stats, ""); !ok {
		t.Error("the empty (no campaign) group is missing")
	}
	if total < 4 {
		t.Errorf("total distinct sources = %d, want at least 4 (instagram, bulten, google, empty)", total)
	}
}

func TestBeaconUTMCampaigns_SeparatesCampaignsOfOneSource(t *testing.T) {
	store, p := campaignSeed(t, "camp-names")

	stats, _, err := store.BeaconUTMCampaigns(context.Background(), "camp-names", p)
	if err != nil {
		t.Fatalf("BeaconUTMCampaigns: %v", err)
	}

	if pv, _, found := groupCount(t, stats, "bahar"); !found || pv != 2 {
		t.Errorf("bahar = %d pageviews (found=%v), want 2", pv, found)
	}
	if pv, _, found := groupCount(t, stats, "kis"); !found || pv != 1 {
		t.Errorf("kis = %d pageviews (found=%v), want 1", pv, found)
	}
}

func TestBeaconUTMMediums_GroupsByChannel(t *testing.T) {
	store, p := campaignSeed(t, "camp-mediums")

	stats, _, err := store.BeaconUTMMediums(context.Background(), "camp-mediums", p)
	if err != nil {
		t.Fatalf("BeaconUTMMediums: %v", err)
	}
	if pv, _, found := groupCount(t, stats, "social"); !found || pv != 3 {
		t.Errorf("social = %d pageviews (found=%v), want 3", pv, found)
	}
	if pv, _, found := groupCount(t, stats, "email"); !found || pv != 1 {
		t.Errorf("email = %d pageviews (found=%v), want 1", pv, found)
	}
}

// Paid clicks group by network, which is the analytic value; the click
// identifier itself is never a grouping key.
func TestBeaconClickSources_GroupsPaidTrafficByNetwork(t *testing.T) {
	store, p := campaignSeed(t, "camp-clicks")

	stats, _, err := store.BeaconClickSources(context.Background(), "camp-clicks", p)
	if err != nil {
		t.Fatalf("BeaconClickSources: %v", err)
	}
	if pv, _, found := groupCount(t, stats, "google"); !found || pv != 1 {
		t.Errorf("google = %d pageviews (found=%v), want 1", pv, found)
	}
}

// The second half of "works everywhere": having found that Instagram
// brought traffic, you must be able to ask what that traffic did.
func TestCampaignFilter_NarrowsEveryOtherView(t *testing.T) {
	store, p := campaignSeed(t, "camp-filter")
	ctx := context.Background()

	unfiltered, _, err := store.BeaconPages(ctx, "camp-filter", p)
	if err != nil {
		t.Fatalf("BeaconPages unfiltered: %v", err)
	}
	unfilteredTotal := 0
	for _, s := range unfiltered {
		unfilteredTotal += s.Pageviews
	}
	if unfilteredTotal != 6 {
		t.Fatalf("unfiltered pageviews = %d, want 6", unfilteredTotal)
	}

	filtered := p
	filtered.campaign = campaignFilter{source: "instagram"}
	pages, _, err := store.BeaconPages(ctx, "camp-filter", filtered)
	if err != nil {
		t.Fatalf("BeaconPages filtered: %v", err)
	}
	filteredTotal := 0
	for _, s := range pages {
		filteredTotal += s.Pageviews
	}
	if filteredTotal != 3 {
		t.Errorf("instagram-filtered pageviews = %d, want 3 (of 6): %+v", filteredTotal, pages)
	}

	// Narrowing further to one campaign of that source.
	narrower := p
	narrower.campaign = campaignFilter{source: "instagram", campaign: "kis"}
	pages, _, err = store.BeaconPages(ctx, "camp-filter", narrower)
	if err != nil {
		t.Fatalf("BeaconPages narrowed: %v", err)
	}
	narrowTotal := 0
	for _, s := range pages {
		narrowTotal += s.Pageviews
	}
	if narrowTotal != 1 {
		t.Errorf("instagram/kis pageviews = %d, want 1: %+v", narrowTotal, pages)
	}
}

// The filter has to reach the session-based queries too, which use a
// different parameter layout ($8 timeout) - the exact place a renumbering
// mistake would hide.
func TestCampaignFilter_AppliesToSummaryAndSessionQueries(t *testing.T) {
	store, p := campaignSeed(t, "camp-sessions")
	ctx := context.Background()

	all, err := store.BeaconSummary(ctx, "camp-sessions", p.from, p.to, BotsExclude, campaignFilter{})
	if err != nil {
		t.Fatalf("BeaconSummary unfiltered: %v", err)
	}
	if all.Pageviews != 6 {
		t.Fatalf("unfiltered summary pageviews = %d, want 6", all.Pageviews)
	}

	only, err := store.BeaconSummary(ctx, "camp-sessions", p.from, p.to, BotsExclude,
		campaignFilter{source: "instagram"})
	if err != nil {
		t.Fatalf("BeaconSummary filtered: %v", err)
	}
	if only.Pageviews != 3 || only.Visitors != 3 {
		t.Errorf("instagram summary = %d pageviews / %d visitors, want 3/3", only.Pageviews, only.Visitors)
	}

	// Entry pages run beaconFilterCTE + sessionCTEs + a boundary CTE,
	// the deepest parameter stack in the package.
	entry := p
	entry.campaign = campaignFilter{source: "instagram"}
	entries, _, err := store.BeaconEntryPages(ctx, "camp-sessions", entry)
	if err != nil {
		t.Fatalf("BeaconEntryPages filtered: %v", err)
	}
	sessions := 0
	for _, e := range entries {
		sessions += e.Sessions
	}
	if sessions != 3 {
		t.Errorf("instagram entry-page sessions = %d, want 3: %+v", sessions, entries)
	}

	// And the timeseries, which binds an interval after the timeout.
	buckets, err := store.BeaconTimeseries(ctx, "camp-sessions", p.from, p.to, "1 hour",
		BotsExclude, campaignFilter{source: "instagram"})
	if err != nil {
		t.Fatalf("BeaconTimeseries filtered: %v", err)
	}
	bucketed := 0
	for _, b := range buckets {
		bucketed += b.Pageviews
	}
	if bucketed != 3 {
		t.Errorf("instagram timeseries pageviews = %d, want 3", bucketed)
	}
}

// An unmatched filter must return nothing rather than everything - the
// failure mode where a predicate silently does not apply.
func TestCampaignFilter_UnmatchedValueReturnsNothing(t *testing.T) {
	store, p := campaignSeed(t, "camp-nomatch")

	p.campaign = campaignFilter{source: "does-not-exist"}
	pages, total, err := store.BeaconPages(context.Background(), "camp-nomatch", p)
	if err != nil {
		t.Fatalf("BeaconPages: %v", err)
	}
	if len(pages) != 0 || total != 0 {
		t.Errorf("unmatched filter returned %d rows (total %d), want none: %+v", len(pages), total, pages)
	}
}

func TestBeaconTitles_IsItsOwnDimension(t *testing.T) {
	base := time.Date(2026, 4, 2, 10, 0, 0, 0, time.UTC)
	store := seedBeacon(t, []beaconSeed{
		{site: "camp-titles", visitor: "v1", at: base, path: "/c/1042?v=3"},
		{site: "camp-titles", visitor: "v2", at: base.Add(time.Minute), path: "/c/1042"},
	})
	p := beaconParams{from: base.Add(-time.Hour), to: base.Add(time.Hour), limit: 50, bots: BotsExclude}

	// seedBeacon writes an empty title, so this asserts the dimension
	// exists and groups rather than asserting a particular title - the
	// point is that the route and column resolve at all.
	stats, _, err := store.BeaconTitles(context.Background(), "camp-titles", p)
	if err != nil {
		t.Fatalf("BeaconTitles: %v", err)
	}
	if len(stats) == 0 {
		t.Fatal("BeaconTitles returned no groups")
	}
	if pv, _, found := groupCount(t, stats, ""); !found || pv != 2 {
		t.Errorf("empty-title group = %d pageviews (found=%v), want 2", pv, found)
	}
}

// Every read method must honour the campaign filter. This is the
// behavioural half of the guard in store_beacon_guard_test.go: that one
// checks the source still follows the parameter convention, this one
// checks the convention still produces the right answer.
//
// Exhaustive on purpose. The failure this defends against is a query
// whose filter silently stops applying - which returns *more* rows than
// asked for, looks entirely plausible, and would be found by a customer
// rather than by us.
func TestCampaignFilter_IsHonouredByEveryReadMethod(t *testing.T) {
	store, p := campaignSeed(t, "camp-every")
	ctx := context.Background()

	// A filter that matches nothing at all. Any method that ignores it
	// returns rows.
	nothing := p
	nothing.campaign = campaignFilter{source: "no-such-source"}

	groups := map[string]func(context.Context, string, beaconParams) ([]BeaconGroupStat, int, error){
		"BeaconPages":            store.BeaconPages,
		"BeaconTitles":           store.BeaconTitles,
		"BeaconReferrers":        store.BeaconReferrers,
		"BeaconBrowsers":         store.BeaconBrowsers,
		"BeaconOperatingSystems": store.BeaconOperatingSystems,
		"BeaconDevices":          store.BeaconDevices,
		"BeaconLanguages":        store.BeaconLanguages,
		"BeaconCountries":        store.BeaconCountries,
		"BeaconUTMSources":       store.BeaconUTMSources,
		"BeaconUTMMediums":       store.BeaconUTMMediums,
		"BeaconUTMCampaigns":     store.BeaconUTMCampaigns,
		"BeaconUTMTerms":         store.BeaconUTMTerms,
		"BeaconUTMContents":      store.BeaconUTMContents,
		"BeaconRefs":             store.BeaconRefs,
		"BeaconClickSources":     store.BeaconClickSources,
	}
	for name, query := range groups {
		t.Run(name, func(t *testing.T) {
			// Sanity: unfiltered, this method returns something. Without
			// this the empty assertion below would pass for a method that
			// never returns anything.
			if rows, _, err := query(ctx, "camp-every", p); err != nil {
				t.Fatalf("unfiltered: %v", err)
			} else if len(rows) == 0 {
				t.Fatalf("unfiltered returned nothing; the assertion below would be vacuous")
			}

			rows, total, err := query(ctx, "camp-every", nothing)
			if err != nil {
				t.Fatalf("filtered: %v", err)
			}
			if len(rows) != 0 || total != 0 {
				t.Errorf("ignored the campaign filter: %d rows (total %d), want none: %+v", len(rows), total, rows)
			}
		})
	}

	// The differently-shaped methods, checked individually.
	t.Run("BeaconEntryPages", func(t *testing.T) {
		rows, total, err := store.BeaconEntryPages(ctx, "camp-every", nothing)
		if err != nil {
			t.Fatalf("BeaconEntryPages: %v", err)
		}
		if len(rows) != 0 || total != 0 {
			t.Errorf("ignored the filter: %+v", rows)
		}
	})
	t.Run("BeaconExitPages", func(t *testing.T) {
		rows, total, err := store.BeaconExitPages(ctx, "camp-every", nothing)
		if err != nil {
			t.Fatalf("BeaconExitPages: %v", err)
		}
		if len(rows) != 0 || total != 0 {
			t.Errorf("ignored the filter: %+v", rows)
		}
	})
	t.Run("BeaconCampaigns", func(t *testing.T) {
		rows, total, err := store.BeaconCampaigns(ctx, "camp-every", nothing)
		if err != nil {
			t.Fatalf("BeaconCampaigns: %v", err)
		}
		if len(rows) != 0 || total != 0 {
			t.Errorf("ignored the filter: %+v", rows)
		}
	})
	t.Run("BeaconEvents", func(t *testing.T) {
		rows, total, err := store.BeaconEvents(ctx, "camp-every", nothing)
		if err != nil {
			t.Fatalf("BeaconEvents: %v", err)
		}
		if len(rows) != 0 || total != 0 {
			t.Errorf("ignored the filter: %+v", rows)
		}
	})
	t.Run("BeaconRaw", func(t *testing.T) {
		rows, total, err := store.BeaconRaw(ctx, "camp-every", nothing)
		if err != nil {
			t.Fatalf("BeaconRaw: %v", err)
		}
		if len(rows) != 0 || total != 0 {
			t.Errorf("ignored the filter: %+v", rows)
		}
	})
	t.Run("BeaconSummary", func(t *testing.T) {
		s, err := store.BeaconSummary(ctx, "camp-every", p.from, p.to, p.bots, nothing.campaign)
		if err != nil {
			t.Fatalf("BeaconSummary: %v", err)
		}
		if s.Pageviews != 0 || s.Visitors != 0 || s.Sessions != 0 {
			t.Errorf("ignored the filter: %+v", s)
		}
	})
	t.Run("BeaconTimeseries", func(t *testing.T) {
		buckets, err := store.BeaconTimeseries(ctx, "camp-every", p.from, p.to, "1 hour", p.bots, nothing.campaign)
		if err != nil {
			t.Fatalf("BeaconTimeseries: %v", err)
		}
		if len(buckets) != 0 {
			t.Errorf("ignored the filter: %+v", buckets)
		}
	})
}
