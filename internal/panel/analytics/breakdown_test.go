package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// window is a range every test in this file uses.
func window() (time.Time, time.Time) {
	from := time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, 0, 7)
}

// emptyBody answers whatever the path asks for.
//
// One body for both call shapes is not possible: "events" is an integer
// on the beacon summary and an array on the events breakdown, so a stub
// that answered the same JSON to everything would make the summary fail
// to decode and quietly change what the test was measuring. Discovered
// exactly that way.
func emptyBody(path string) string {
	for _, spec := range breakdowns {
		if strings.HasSuffix(path, "/beacon/"+spec.path) {
			return `{"total":0,"` + spec.key + `":[]}`
		}
	}
	return `{"pageviews":0,"events":0,"visitors":0,"sessions":0,"snapshots":0}`
}

// clientFor builds a client pointed at a stub API.
func clientFor(t *testing.T, h http.Handler) *Client {
	t.Helper()
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)
	c, err := New(server.URL, "jeton")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestEveryBreakdownDecodesItsOwnRowShape.
//
// Four of the API's row types flatten into one Row, which is where a
// silent mistake would live: a decoder wired to the wrong shape does not
// fail, it produces rows whose counts are all zero. The table below is
// therefore per kind, with the real JSON each endpoint answers.
func TestEveryBreakdownDecodesItsOwnRowShape(t *testing.T) {
	bodies := map[BreakdownKind]string{
		BreakdownPages: `{"total":3,"pages":[
			{"key":"/","pageviews":40,"visitors":12},
			{"key":"/fiyat","pageviews":9,"visitors":7}]}`,
		BreakdownReferrers: `{"total":2,"referrers":[
			{"key":"google.com","pageviews":18,"visitors":11},
			{"key":"","pageviews":22,"visitors":14,"empty":true}]}`,
		BreakdownDevices: `{"total":2,"devices":[
			{"key":"mobile","pageviews":30,"visitors":20}]}`,
		BreakdownCountries: `{"total":2,"countries":[
			{"key":"TR","pageviews":25,"visitors":15}]}`,
		BreakdownCampaigns: `{"total":1,"campaigns":[
			{"key":"utm_source=bulten","params":{"utm_source":"bulten"},"pageviews":6,"visitors":5}]}`,
		BreakdownEvents: `{"total":1,"events":[
			{"name":"kayit","count":14,"visitors":9}]}`,
	}
	// What the first row of each must come out as, in the flattened shape.
	want := map[BreakdownKind]Row{
		BreakdownPages:     {Key: "/", Count: 40, Visitors: 12},
		BreakdownReferrers: {Key: "google.com", Count: 18, Visitors: 11},
		BreakdownDevices:   {Key: "mobile", Count: 30, Visitors: 20},
		BreakdownCountries: {Key: "TR", Count: 25, Visitors: 15},
		BreakdownCampaigns: {
			Key: "utm_source=bulten", Count: 6, Visitors: 5,
			Params: map[string]string{"utm_source": "bulten"},
		},
		// The events endpoint names its column "count" and its key
		// "name". Nothing about a wrongly wired decoder would look
		// broken; it would just report every event as having happened
		// zero times.
		BreakdownEvents: {Key: "kayit", Count: 14, Visitors: 9},
	}

	if len(bodies) != len(breakdowns) {
		t.Fatalf("this test covers %d breakdowns; the registry has %d", len(bodies), len(breakdowns))
	}

	from, to := window()
	for kind, body := range bodies {
		t.Run(string(kind), func(t *testing.T) {
			c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			got := c.breakdown(context.Background(), "site", BreakdownRequest{Kind: kind, Limit: 8}, from, to)
			if got.Err != nil {
				t.Fatalf("breakdown: %v", got.Err)
			}
			if len(got.Rows) == 0 {
				t.Fatal("no rows decoded")
			}
			first := got.Rows[0]
			exp := want[kind]
			if first.Key != exp.Key || first.Count != exp.Count || first.Visitors != exp.Visitors {
				t.Errorf("first row = %+v, want key=%q count=%d visitors=%d",
					first, exp.Key, exp.Count, exp.Visitors)
			}
			if len(exp.Params) > 0 && first.Params["utm_source"] != exp.Params["utm_source"] {
				t.Errorf("params = %v, want %v", first.Params, exp.Params)
			}
		})
	}
}

// TestTheNeverDeterminedGroupSurvivesTheHop.
//
// The API flags the group whose value was never determined rather than
// dropping it, so the groups still add up to the site's total. A client
// that lost that flag would hand the panel a row it could only draw
// blank - and a blank label is how the row ends up being deleted by the
// next person to look at the page.
func TestTheNeverDeterminedGroupSurvivesTheHop(t *testing.T) {
	cases := []struct {
		kind BreakdownKind
		body string
		// index of the row that must come back flagged
		at int
	}{
		{BreakdownReferrers, `{"total":2,"referrers":[
			{"key":"google.com","pageviews":1,"visitors":1},
			{"key":"","pageviews":22,"visitors":14,"empty":true}]}`, 1},
	}
	from, to := window()
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			got := c.breakdown(context.Background(), "site", BreakdownRequest{Kind: tc.kind}, from, to)
			if got.Err != nil {
				t.Fatalf("breakdown: %v", got.Err)
			}
			if len(got.Rows) != 2 {
				t.Fatalf("got %d rows, want 2 - the flagged group must not be dropped", len(got.Rows))
			}
			if !got.Rows[tc.at].Empty {
				t.Errorf("row %d is not flagged as the never-determined group: %+v", tc.at, got.Rows[tc.at])
			}
			if got.Rows[1-tc.at].Empty {
				t.Errorf("row %d is flagged and should not be: %+v", 1-tc.at, got.Rows[1-tc.at])
			}
		})
	}
}

// TestCampaignsHaveNoNeverDeterminedGroup, which is the endpoint's shape
// rather than an omission.
//
// BeaconCampaigns groups by the stored campaign query and excludes the
// empty one in SQL, so untagged traffic is not a flagged group - it is
// not returned at all. Inventing one here would produce a row the panel names
// and the numbers do not support; the honest handling is that these rows
// simply do not add up to the site's total, and the section's help text
// says so.
func TestCampaignsHaveNoNeverDeterminedGroup(t *testing.T) {
	// Even given a row the endpoint would never send, nothing is flagged:
	// the client does not second-guess the shape it was handed.
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"total":2,"campaigns":[
			{"key":"","params":{},"pageviews":90,"visitors":50},
			{"key":"utm_source=x","params":{"utm_source":"x"},"pageviews":2,"visitors":2}]}`))
	}))
	from, to := window()
	got := c.breakdown(context.Background(), "site", BreakdownRequest{Kind: BreakdownCampaigns}, from, to)
	if got.Err != nil {
		t.Fatal(got.Err)
	}
	for i, row := range got.Rows {
		if row.Empty {
			t.Errorf("row %d is flagged as a never-determined group; campaigns has none", i)
		}
	}
}

// TestTheBotsFilterIsNeverSent.
//
// The share a row shows is its percentage of the summary above it, and
// that only holds while both calls count the same population. The API
// defaults `bots` to exclude on the summary and on every breakdown
// alike, so the panel sends it on neither.
//
// Send it on one and not the other and the page still draws perfectly -
// with percentages that do not add to a hundred and nothing on screen
// saying why. That is the whole reason this is a test rather than a
// comment.
func TestTheBotsFilterIsNeverSent(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []url.Values
	)
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Query())
		mu.Unlock()
		_, _ = w.Write([]byte(emptyBody(r.URL.Path)))
	}))

	from, to := window()
	want := make([]BreakdownRequest, 0, len(breakdowns))
	for kind := range breakdowns {
		want = append(want, BreakdownRequest{Kind: kind, Limit: 8})
	}
	c.FetchSite(context.Background(), "site", from, to, want...)

	mu.Lock()
	defer mu.Unlock()
	// Two summaries plus every breakdown.
	if len(seen) != len(breakdowns)+2 {
		t.Fatalf("made %d calls, want %d", len(seen), len(breakdowns)+2)
	}
	for _, q := range seen {
		if q.Has(breakdownQuery) {
			t.Errorf("a call sent %s=%q; both sides must take the API default so a share and its "+
				"denominator count the same people", breakdownQuery, q.Get(breakdownQuery))
		}
	}
}

// TestARangeIsSentOnEveryCall. A call that forgot from/to would be
// answered for the API's own default period, and the page would draw a
// range picker saying one thing over numbers meaning another.
func TestARangeIsSentOnEveryCall(t *testing.T) {
	var (
		mu   sync.Mutex
		seen []url.Values
	)
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Query())
		mu.Unlock()
		_, _ = w.Write([]byte(`{"total":1,"pages":[{"key":"/","pageviews":1,"visitors":1}]}`))
	}))

	from, to := window()
	c.FetchSite(context.Background(), "site", from, to,
		BreakdownRequest{Kind: BreakdownPages, Limit: 8, Offset: 16})

	mu.Lock()
	defer mu.Unlock()
	for _, q := range seen {
		if q.Get("from") != from.UTC().Format(time.RFC3339) {
			t.Errorf("from = %q, want %q", q.Get("from"), from.UTC().Format(time.RFC3339))
		}
		if q.Get("to") != to.UTC().Format(time.RFC3339) {
			t.Errorf("to = %q, want %q", q.Get("to"), to.UTC().Format(time.RFC3339))
		}
	}
	// The breakdown call, and only it, carries the paging.
	var paged int
	for _, q := range seen {
		if q.Get("limit") == "8" && q.Get("offset") == "16" {
			paged++
		}
	}
	if paged != 1 {
		t.Errorf("%d calls carried limit=8&offset=16, want exactly 1", paged)
	}
}

// TestOneBreakdownFailingIsNotThePageFailing.
//
// Six sections are fetched for one page. A panel that surfaced the first
// error as the page's error would blank five working tables because one
// endpoint was slow, so the failure is carried per breakdown.
func TestOneBreakdownFailingIsNotThePageFailing(t *testing.T) {
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/sites/site/beacon/events" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"total":1,"pages":[{"key":"/","pageviews":5,"visitors":3}],
			"pageviews":5,"visitors":3}`))
	}))

	from, to := window()
	site := c.FetchSite(context.Background(), "site", from, to,
		BreakdownRequest{Kind: BreakdownPages, Limit: 8},
		BreakdownRequest{Kind: BreakdownEvents, Limit: 8},
	)

	if got := site.Breakdowns[BreakdownPages]; got.Err != nil {
		t.Errorf("the working breakdown carries an error: %v", got.Err)
	} else if len(got.Rows) != 1 {
		t.Errorf("the working breakdown has %d rows, want 1", len(got.Rows))
	}
	if got := site.Breakdowns[BreakdownEvents]; !errors.Is(got.Err, ErrUnavailable) {
		t.Errorf("the failing breakdown's error = %v, want ErrUnavailable", got.Err)
	}
	if site.BeaconErr != nil {
		t.Errorf("a failing breakdown took the summary down with it: %v", site.BeaconErr)
	}
}

// TestAnAnswerWithoutRowsIsNotAnEmptyBreakdown.
//
// A 200 whose body has no rows field is something other than this
// endpoint answering - a proxy's error page, a different service on that
// port. Reading it as "there is nothing to show" would draw an empty
// table over a misconfiguration.
func TestAnAnswerWithoutRowsIsNotAnEmptyBreakdown(t *testing.T) {
	for _, body := range []string{
		`{"total":0}`,
		`{"total":0,"sayfalar":[]}`,
		`{"total":0,"pages":{"not":"an array"}}`,
	} {
		c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		from, to := window()
		got := c.breakdown(context.Background(), "site", BreakdownRequest{Kind: BreakdownPages}, from, to)
		if got.Err == nil {
			t.Errorf("%s produced no error and %d rows", body, len(got.Rows))
		}
	}
}

// TestAnUnknownKindIsNotATransportFailure. Reporting it as unreachable
// would send an operator to check a service that is perfectly fine.
func TestAnUnknownKindIsNotATransportFailure(t *testing.T) {
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an unknown kind reached the API")
	}))
	from, to := window()
	got := c.breakdown(context.Background(), "site", BreakdownRequest{Kind: "ja4"}, from, to)
	switch {
	case got.Err == nil:
		t.Fatal("an unknown kind produced no error")
	case errors.Is(got.Err, ErrUnavailable), errors.Is(got.Err, ErrRefused), errors.Is(got.Err, ErrNoSite):
		t.Errorf("an unknown kind reported a transport error: %v", got.Err)
	}
	if KnownBreakdown("ja4") {
		t.Error(`KnownBreakdown("ja4") is true`)
	}
	for kind := range breakdowns {
		if !KnownBreakdown(kind) {
			t.Errorf("KnownBreakdown(%q) is false for a registered kind", kind)
		}
	}
}

// TestAPageIsOneRoundOfCalls.
//
// Eight calls for a site page: two summaries and six breakdowns. If they
// ran in sequence the page would cost eight round trips; if the
// summaries ran in their own round it would be bounded by twice
// PageTimeout while reading, in the code, as though it were one.
//
// Measured rather than asserted structurally: the handler blocks until
// every call has arrived, so the test can only pass if they overlap.
func TestAPageIsOneRoundOfCalls(t *testing.T) {
	const calls = 8
	var (
		mu      sync.Mutex
		arrived int
		all     = make(chan struct{})
	)
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrived++
		if arrived == calls {
			close(all)
		}
		mu.Unlock()
		select {
		case <-all:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(emptyBody(r.URL.Path)))
	}))

	want := make([]BreakdownRequest, 0, len(breakdowns))
	for kind := range breakdowns {
		want = append(want, BreakdownRequest{Kind: kind, Limit: 8})
	}
	from, to := window()
	site := c.FetchSite(context.Background(), "site", from, to, want...)

	for kind, b := range site.Breakdowns {
		if b.Err != nil {
			t.Fatalf("%s: %v - the calls did not overlap", kind, b.Err)
		}
	}
	if site.BeaconErr != nil || site.TrafficErr != nil {
		t.Fatalf("summaries: %v / %v", site.BeaconErr, site.TrafficErr)
	}
}

// TestAnUnconfiguredClientAnswersEveryBreakdown.
//
// A panel with no API address is a deployment mid-installation, not a
// crash. Every requested breakdown has to come back present and
// unavailable, because a missing map entry is a zero Breakdown - no
// rows, no error - which the panel would draw as an empty table.
func TestAnUnconfiguredClientAnswersEveryBreakdown(t *testing.T) {
	var c *Client
	from, to := window()
	site := c.FetchSite(context.Background(), "site", from, to,
		BreakdownRequest{Kind: BreakdownPages},
		BreakdownRequest{Kind: BreakdownEvents},
	)
	for _, kind := range []BreakdownKind{BreakdownPages, BreakdownEvents} {
		b, ok := site.Breakdowns[kind]
		if !ok {
			t.Fatalf("%s is missing from the result", kind)
		}
		if !errors.Is(b.Err, ErrUnavailable) {
			t.Errorf("%s: err = %v, want ErrUnavailable", kind, b.Err)
		}
	}
	if !errors.Is(site.BeaconErr, ErrUnavailable) {
		t.Errorf("beacon summary err = %v, want ErrUnavailable", site.BeaconErr)
	}
}

// TestEveryRegisteredPathAndKeyIsDistinct.
//
// Two kinds sharing a path would silently draw one breakdown twice;
// sharing an envelope key is how a decoder ends up reading somebody
// else's rows. Both are one careless copy-paste away, and neither fails
// anywhere else.
func TestEveryRegisteredPathAndKeyIsDistinct(t *testing.T) {
	paths := map[string]BreakdownKind{}
	keys := map[string]BreakdownKind{}
	for kind, spec := range breakdowns {
		if spec.path == "" || spec.key == "" || spec.decode == nil {
			t.Errorf("%s is registered incompletely: %+v", kind, spec)
		}
		if other, dup := paths[spec.path]; dup {
			t.Errorf("%s and %s share the path %q", kind, other, spec.path)
		}
		if other, dup := keys[spec.key]; dup {
			t.Errorf("%s and %s share the envelope key %q", kind, other, spec.key)
		}
		paths[spec.path] = kind
		keys[spec.key] = kind
	}
}

// TestABreakdownRequestSurvivesAHostileSiteID. The site id reaches a URL
// path, so it is escaped rather than concatenated.
func TestABreakdownRequestSurvivesAHostileSiteID(t *testing.T) {
	var got string
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		_, _ = w.Write([]byte(`{"total":0,"pages":[]}`))
	}))
	from, to := window()
	c.breakdown(context.Background(), "../../beacon/raw", BreakdownRequest{Kind: BreakdownPages}, from, to)
	if want := "/api/v1/sites/..%2F..%2Fbeacon%2Fraw/beacon/pages"; got != want {
		// http.ServeMux cleans paths, so an unescaped id would arrive
		// somewhere else entirely rather than 404ing here.
		t.Errorf("path = %q, want %q", got, want)
	}
}

// TestTotalIsGroupsNotRows. total is how many distinct groups exist, and
// it is what the pager and the "all 143" link read. Reading the length
// of the returned page instead would cap every list at one page.
func TestTotalIsGroupsNotRows(t *testing.T) {
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"total":143,"limit":2,"offset":0,"pages":[
			{"key":"/","pageviews":9,"visitors":4},
			{"key":"/b","pageviews":8,"visitors":3}]}`))
	}))
	from, to := window()
	got := c.breakdown(context.Background(), "site", BreakdownRequest{Kind: BreakdownPages, Limit: 2}, from, to)
	if got.Err != nil {
		t.Fatal(got.Err)
	}
	if got.Total != 143 {
		t.Errorf("Total = %d, want 143", got.Total)
	}
	if len(got.Rows) != 2 {
		t.Errorf("got %d rows, want 2", len(got.Rows))
	}
}

// TestTheRegistryCoversTheDocumentedSix keeps the plan and the code
// together: the phase is six breakdowns, named in PLAN.md §D2.
func TestTheRegistryCoversTheDocumentedSix(t *testing.T) {
	want := []string{"cihaz", "kampanya", "kaynak", "olay", "sayfa", "ulke"}
	got := make([]string, 0, len(breakdowns))
	for kind := range breakdowns {
		got = append(got, string(kind))
	}
	sort.Strings(got)
	if len(got) != len(want) {
		t.Fatalf("the registry has %d breakdowns, the phase is %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("registry = %v, want %v", got, want)
			break
		}
	}
}

// TestDecodersRejectRubbishRatherThanReturningNothing. A decoder that
// swallowed a malformed body would report an empty breakdown, which the
// panel draws as "nothing happened in this period".
func TestDecodersRejectRubbishRatherThanReturningNothing(t *testing.T) {
	for name, decode := range map[string]func(json.RawMessage) ([]Row, error){
		"groups":    decodeGroups,
		"campaigns": decodeCampaigns,
		"events":    decodeEvents,
	} {
		if _, err := decode(json.RawMessage(`{"not":"an array"}`)); err == nil {
			t.Errorf("%s accepted an object where an array belongs", name)
		}
		rows, err := decode(json.RawMessage(`[]`))
		if err != nil {
			t.Errorf("%s rejected an empty array: %v", name, err)
		}
		if len(rows) != 0 {
			t.Errorf("%s turned an empty array into %d rows", name, len(rows))
		}
	}
}
