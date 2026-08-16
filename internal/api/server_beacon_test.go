package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// errStoreFailed stands in for a database failure. Its text looks like a
// real driver error on purpose: the point of the test using it is that
// none of this reaches the client.
var errStoreFailed = errors.New(`pq: permission denied for table beacon_events`)

// The beacon half of fakeStore. Split into its own file alongside the
// handlers it fakes rather than growing server_test.go, but the same
// type - the Server takes one Querier.

func (f *fakeStore) BeaconSites(ctx context.Context) ([]string, error) {
	f.gotCall = "BeaconSites"
	return f.sites, f.err
}

func (f *fakeStore) BeaconSummary(ctx context.Context, siteID string, from, to time.Time, bots BotFilter, campaign campaignFilter) (BeaconSummary, error) {
	f.gotCampaign = campaign
	f.gotCall, f.gotSite, f.gotFrom, f.gotTo, f.gotBots = "BeaconSummary", siteID, from, to, bots
	if f.err != nil {
		return BeaconSummary{}, f.err
	}
	return BeaconSummary{SiteID: siteID, From: from, To: to, Bots: string(bots), Pageviews: 9, Visitors: 4, Sessions: 5}, nil
}

func (f *fakeStore) BeaconTimeseries(ctx context.Context, siteID string, from, to time.Time, interval string, bots BotFilter, campaign campaignFilter) ([]BeaconBucket, error) {
	f.gotCampaign = campaign
	f.gotCall, f.gotSite, f.gotFrom, f.gotTo, f.gotInterval, f.gotBots = "BeaconTimeseries", siteID, from, to, interval, bots
	if f.err != nil {
		return nil, f.err
	}
	return []BeaconBucket{{Time: from, Pageviews: 3, Visitors: 2, Sessions: 2}}, nil
}

// captureBeacon records the shared list parameters and the calling
// method, so every breakdown fake stays one line.
func (f *fakeStore) captureBeacon(call, siteID string, p beaconParams) {
	f.gotCall, f.gotSite = call, siteID
	f.gotFrom, f.gotTo, f.gotLimit, f.gotOffset, f.gotBots = p.from, p.to, p.limit, p.offset, p.bots
	f.gotCampaign = p.campaign
}

func (f *fakeStore) group(call, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	f.captureBeacon(call, siteID, p)
	if f.err != nil {
		return nil, 0, f.err
	}
	return []BeaconGroupStat{{Key: call, Pageviews: 7, Visitors: 3}}, 1, nil
}

// Each breakdown returns its own method name as the row key, so a test
// can prove which query a route actually reached.
func (f *fakeStore) BeaconPages(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return f.group("BeaconPages", siteID, p)
}

func (f *fakeStore) BeaconReferrers(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return f.group("BeaconReferrers", siteID, p)
}

func (f *fakeStore) BeaconBrowsers(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return f.group("BeaconBrowsers", siteID, p)
}

func (f *fakeStore) BeaconOperatingSystems(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return f.group("BeaconOperatingSystems", siteID, p)
}

func (f *fakeStore) BeaconDevices(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return f.group("BeaconDevices", siteID, p)
}

func (f *fakeStore) BeaconLanguages(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return f.group("BeaconLanguages", siteID, p)
}

func (f *fakeStore) BeaconTitles(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return f.group("BeaconTitles", siteID, p)
}

func (f *fakeStore) BeaconUTMSources(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return f.group("BeaconUTMSources", siteID, p)
}

func (f *fakeStore) BeaconUTMMediums(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return f.group("BeaconUTMMediums", siteID, p)
}

func (f *fakeStore) BeaconUTMCampaigns(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return f.group("BeaconUTMCampaigns", siteID, p)
}

func (f *fakeStore) BeaconUTMTerms(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return f.group("BeaconUTMTerms", siteID, p)
}

func (f *fakeStore) BeaconUTMContents(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return f.group("BeaconUTMContents", siteID, p)
}

func (f *fakeStore) BeaconRefs(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return f.group("BeaconRefs", siteID, p)
}

func (f *fakeStore) BeaconClickSources(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return f.group("BeaconClickSources", siteID, p)
}

func (f *fakeStore) BeaconCountries(ctx context.Context, siteID string, p beaconParams) ([]BeaconGroupStat, int, error) {
	return f.group("BeaconCountries", siteID, p)
}

func (f *fakeStore) BeaconEntryPages(ctx context.Context, siteID string, p beaconParams) ([]SessionPathStat, int, error) {
	f.captureBeacon("BeaconEntryPages", siteID, p)
	if f.err != nil {
		return nil, 0, f.err
	}
	return []SessionPathStat{{Path: "/landing", Sessions: 4, Visitors: 3}}, 1, nil
}

func (f *fakeStore) BeaconExitPages(ctx context.Context, siteID string, p beaconParams) ([]SessionPathStat, int, error) {
	f.captureBeacon("BeaconExitPages", siteID, p)
	if f.err != nil {
		return nil, 0, f.err
	}
	return []SessionPathStat{{Path: "/checkout", Sessions: 2, Visitors: 2}}, 1, nil
}

func (f *fakeStore) BeaconCampaigns(ctx context.Context, siteID string, p beaconParams) ([]CampaignStat, int, error) {
	f.captureBeacon("BeaconCampaigns", siteID, p)
	if f.err != nil {
		return nil, 0, f.err
	}
	return []CampaignStat{{Key: "utm_source=newsletter", Params: map[string]string{"utm_source": "newsletter"}, Pageviews: 5}}, 1, nil
}

func (f *fakeStore) BeaconEvents(ctx context.Context, siteID string, p beaconParams) ([]EventStat, int, error) {
	f.captureBeacon("BeaconEvents", siteID, p)
	if f.err != nil {
		return nil, 0, f.err
	}
	return []EventStat{{Name: "signup", Count: 6, Visitors: 4}}, 1, nil
}

func (f *fakeStore) BeaconRaw(ctx context.Context, siteID string, p beaconParams) ([]BeaconEvent, int, error) {
	f.captureBeacon("BeaconRaw", siteID, p)
	if f.err != nil {
		return nil, 0, f.err
	}
	return []BeaconEvent{{Path: "/", EventType: "pageview", IP: "203.0.113.1"}}, 1, nil
}

func (f *fakeStore) CrossoverSummary(ctx context.Context, siteID string, from, to time.Time) (CrossoverSummary, error) {
	f.gotCall, f.gotSite, f.gotFrom, f.gotTo = "CrossoverSummary", siteID, from, to
	if f.err != nil {
		return CrossoverSummary{}, f.err
	}
	return CrossoverSummary{SiteID: siteID, From: from, To: to, IPsSeen: 10, IPsRanJS: 4, IPsSilent: 6, JSCoverage: 0.4}, nil
}

func (f *fakeStore) SilentIPs(ctx context.Context, siteID string, from, to time.Time, limit, offset int) ([]IPStat, int, error) {
	f.gotCall, f.gotSite, f.gotFrom, f.gotTo, f.gotLimit, f.gotOffset = "SilentIPs", siteID, from, to, limit, offset
	if f.err != nil {
		return nil, 0, f.err
	}
	return []IPStat{{IP: "203.0.113.9", PeakScore: 80}}, 1, nil
}

func (f *fakeStore) JSBots(ctx context.Context, siteID string, from, to time.Time, limit, offset, botScoreMin int) ([]JSBot, int, error) {
	f.gotCall, f.gotSite, f.gotFrom, f.gotTo, f.gotLimit, f.gotOffset, f.gotBotScoreMin = "JSBots", siteID, from, to, limit, offset, botScoreMin
	if f.err != nil {
		return nil, 0, f.err
	}
	return []JSBot{{IP: "203.0.113.7", PeakScore: 70, IsBotUA: true, Pageviews: 12}}, 1, nil
}

// beaconRoutes is every beacon and crossover route, kept exhaustive
// rather than sampled for the same reason the collector-side lists are:
// one route forgetting an authorization check leaks another customer's
// data, and a sampled list is exactly how that gets missed.
var beaconRoutes = []string{
	"/api/v1/sites/site-a/beacon/summary",
	"/api/v1/sites/site-a/beacon/timeseries",
	"/api/v1/sites/site-a/beacon/pages",
	"/api/v1/sites/site-a/beacon/entry-pages",
	"/api/v1/sites/site-a/beacon/exit-pages",
	"/api/v1/sites/site-a/beacon/referrers",
	"/api/v1/sites/site-a/beacon/campaigns",
	"/api/v1/sites/site-a/beacon/browsers",
	"/api/v1/sites/site-a/beacon/operating-systems",
	"/api/v1/sites/site-a/beacon/devices",
	"/api/v1/sites/site-a/beacon/languages",
	"/api/v1/sites/site-a/beacon/countries",
	"/api/v1/sites/site-a/beacon/events",
	"/api/v1/sites/site-a/beacon/raw",
	"/api/v1/sites/site-a/crossover/summary",
	"/api/v1/sites/site-a/crossover/silent-ips",
	"/api/v1/sites/site-a/crossover/js-bots",
}

func TestServer_BeaconRoutesRequireAuth(t *testing.T) {
	h := newTestServer(t, &fakeStore{})
	for _, route := range append(beaconRoutes, "/api/v1/beacon/sites") {
		t.Run(route, func(t *testing.T) {
			if w := do(h, http.MethodGet, route, ""); w.Code != http.StatusUnauthorized {
				t.Errorf("no token -> %d, want 401", w.Code)
			}
			if w := do(h, http.MethodGet, route, "wrong-secret"); w.Code != http.StatusUnauthorized {
				t.Errorf("bad token -> %d, want 401", w.Code)
			}
		})
	}
}

func TestServer_BeaconRoutesCheckSiteAuthorization(t *testing.T) {
	h := newTestServer(t, &fakeStore{})
	for _, route := range beaconRoutes {
		// "ahmet" may read site-a only; every route must refuse site-b.
		other := strings.Replace(route, "site-a", "site-b", 1)
		t.Run(other, func(t *testing.T) {
			if w := do(h, http.MethodGet, other, "ahmet-secret"); w.Code != http.StatusForbidden {
				t.Errorf("cross-site read -> %d, want 403", w.Code)
			}
			if w := do(h, http.MethodGet, route, "ahmet-secret"); w.Code != http.StatusOK {
				t.Errorf("own-site read -> %d, want 200", w.Code)
			}
		})
	}
}

func TestServer_BeaconRoutesReturnJSON(t *testing.T) {
	h := newTestServer(t, &fakeStore{})
	for _, route := range append(beaconRoutes, "/api/v1/beacon/sites") {
		t.Run(route, func(t *testing.T) {
			w := do(h, http.MethodGet, route, "panel-secret")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q", ct)
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Errorf("body is not a JSON object: %v", err)
			}
		})
	}
}

func TestServer_BeaconRoutesRejectNonGET(t *testing.T) {
	h := newTestServer(t, &fakeStore{})
	for _, route := range beaconRoutes {
		if w := do(h, http.MethodPost, route, "panel-secret"); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s -> %d, want 405", route, w.Code)
		}
	}
}

// The seven breakdown routes share one handler and are wired from a map,
// so nothing but this would catch /browsers being pointed at the devices
// query.
func TestServer_EachBreakdownRouteReachesItsOwnQuery(t *testing.T) {
	cases := map[string]string{
		"pages":             "BeaconPages",
		"referrers":         "BeaconReferrers",
		"browsers":          "BeaconBrowsers",
		"operating-systems": "BeaconOperatingSystems",
		"devices":           "BeaconDevices",
		"languages":         "BeaconLanguages",
		"countries":         "BeaconCountries",
		"titles":            "BeaconTitles",
		"utm-sources":       "BeaconUTMSources",
		"utm-mediums":       "BeaconUTMMediums",
		"utm-campaigns":     "BeaconUTMCampaigns",
		"utm-terms":         "BeaconUTMTerms",
		"utm-contents":      "BeaconUTMContents",
		"refs":              "BeaconRefs",
		"click-sources":     "BeaconClickSources",
	}
	for path, wantCall := range cases {
		t.Run(path, func(t *testing.T) {
			store := &fakeStore{}
			h := newTestServer(t, store)
			w := do(h, http.MethodGet, "/api/v1/sites/site-a/beacon/"+path, "panel-secret")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d", w.Code)
			}
			if store.gotCall != wantCall {
				t.Errorf("/beacon/%s reached %s, want %s", path, store.gotCall, wantCall)
			}
		})
	}
}

// Each breakdown must also be published under a key naming what it is;
// a panel keying off the response would otherwise read the wrong field.
func TestServer_BreakdownResponseKeys(t *testing.T) {
	cases := map[string]string{
		"pages":             "pages",
		"referrers":         "referrers",
		"browsers":          "browsers",
		"operating-systems": "operating_systems",
		"devices":           "devices",
		"languages":         "languages",
		"countries":         "countries",
	}
	for path, wantKey := range cases {
		t.Run(path, func(t *testing.T) {
			h := newTestServer(t, &fakeStore{})
			w := do(h, http.MethodGet, "/api/v1/sites/site-a/beacon/"+path, "panel-secret")

			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("bad JSON: %v", err)
			}
			if _, ok := body[wantKey]; !ok {
				t.Errorf("response has no %q key; got keys %v", wantKey, keysOf(body))
			}
		})
	}
}

func keysOf(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func TestServer_BotFilterDefaultsToExcludeAndReachesTheStore(t *testing.T) {
	store := &fakeStore{}
	h := newTestServer(t, store)

	do(h, http.MethodGet, "/api/v1/sites/site-a/beacon/pages", "panel-secret")
	if store.gotBots != BotsExclude {
		t.Errorf("default bots = %q, want %q", store.gotBots, BotsExclude)
	}

	for _, want := range []BotFilter{BotsInclude, BotsOnly, BotsExclude} {
		do(h, http.MethodGet, "/api/v1/sites/site-a/beacon/pages?bots="+string(want), "panel-secret")
		if store.gotBots != want {
			t.Errorf("bots=%s reached the store as %q", want, store.gotBots)
		}
	}
}

func TestServer_InvalidBotFilterIs400(t *testing.T) {
	h := newTestServer(t, &fakeStore{})
	w := do(h, http.MethodGet, "/api/v1/sites/site-a/beacon/pages?bots=maybe", "panel-secret")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// bot_score_min is the collector's behavioral score and has no
// counterpart on a beacon event. Accepting and ignoring it would hand
// back an unfiltered number to a caller who believed it was filtered -
// quietly wrong, which is the failure mode this project has already paid
// for once.
func TestServer_BotScoreMinOnABeaconRouteIs400(t *testing.T) {
	h := newTestServer(t, &fakeStore{})
	for _, route := range beaconRoutes {
		if !strings.Contains(route, "/beacon/") {
			continue // crossover routes legitimately use the collector's score
		}
		w := do(h, http.MethodGet, route+"?bot_score_min=50", "panel-secret")
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s?bot_score_min=50 -> %d, want 400", route, w.Code)
		}
		if !strings.Contains(w.Body.String(), "bots=") {
			t.Errorf("%s: the error should point at the parameter that does work; got %s", route, w.Body)
		}
	}
}

// The crossover endpoints read the collector's table too, so
// bot_score_min is meaningful there and must be accepted.
func TestServer_CrossoverAcceptsBotScoreMin(t *testing.T) {
	store := &fakeStore{}
	h := newTestServer(t, store)

	w := do(h, http.MethodGet, "/api/v1/sites/site-a/crossover/js-bots?bot_score_min=70", "panel-secret")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", w.Code, w.Body)
	}
	if store.gotBotScoreMin != 70 {
		t.Errorf("bot_score_min reached the store as %d, want 70", store.gotBotScoreMin)
	}
}

func TestServer_BeaconPaginationReachesTheStoreAndIsEchoedBack(t *testing.T) {
	store := &fakeStore{}
	h := newTestServer(t, store)

	w := do(h, http.MethodGet, "/api/v1/sites/site-a/beacon/pages?limit=7&offset=14", "panel-secret")
	if store.gotLimit != 7 || store.gotOffset != 14 {
		t.Errorf("store got limit=%d offset=%d, want 7 and 14", store.gotLimit, store.gotOffset)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if body["limit"] != float64(7) || body["offset"] != float64(14) || body["total"] != float64(1) {
		t.Errorf("envelope = %v", body)
	}
	// Every beacon response carries the filter that produced it, so a
	// stored or shared response can always explain its own numbers.
	if body["bots"] != string(BotsExclude) {
		t.Errorf("bots not echoed back: %v", body["bots"])
	}
}

func TestServer_BeaconSitesScopesToTheToken(t *testing.T) {
	store := &fakeStore{sites: []string{"site-a", "site-b", "site-c"}}
	h := newTestServer(t, store)

	var wildcard map[string][]string
	w := do(h, http.MethodGet, "/api/v1/beacon/sites", "panel-secret")
	if err := json.Unmarshal(w.Body.Bytes(), &wildcard); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(wildcard["sites"]) != 3 {
		t.Errorf("wildcard token saw %v, want all three", wildcard["sites"])
	}

	var scoped map[string][]string
	w = do(h, http.MethodGet, "/api/v1/beacon/sites", "ahmet-secret")
	if err := json.Unmarshal(w.Body.Bytes(), &scoped); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if len(scoped["sites"]) != 1 || scoped["sites"][0] != "site-a" {
		t.Errorf("scoped token saw %v, want only site-a - it must not learn other sites exist", scoped["sites"])
	}
}

func TestServer_BeaconStoreErrorsAre500WithoutLeakingDetail(t *testing.T) {
	h := newTestServer(t, &fakeStore{err: errStoreFailed})
	for _, route := range beaconRoutes {
		w := do(h, http.MethodGet, route, "panel-secret")
		if w.Code != http.StatusInternalServerError {
			t.Errorf("%s -> %d, want 500", route, w.Code)
		}
		if strings.Contains(w.Body.String(), errStoreFailed.Error()) {
			t.Errorf("%s leaked the database error: %s", route, w.Body)
		}
	}
}
