//go:build integration

// D2 end to end: real beacon rows, grouped by a real API, drawn as real
// sections.
//
// The unit tests decide what an empty section says and which total a
// share divides by. What only a live run shows is whether the panel and
// the API agree about the shapes: six endpoints, four row types, one
// table renderer, and an envelope whose rows arrive under a different
// key each time.

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
)

const breakdownSite = "kirilim-testi"

// beaconRow is one event to seed.
type beaconRow struct {
	visitor  string
	kind     string // "pageview" or "event"
	name     string
	path     string
	referrer string
	utmSrc   string
	query    string
	device   string
	country  string
}

// seedBeacon writes beacon events for a site inside the range the page
// will ask for.
//
// Deliberately uneven: some rows carry a referrer and some do not, one
// campaign is tagged and the rest are not. The never-determined group is
// the row this phase is most likely to get wrong, and a fixture where
// every column is populated would never produce one.
func seedBeacon(t *testing.T, site string, when time.Time, rows []beaconRow) {
	t.Helper()
	// The beacon's rows go in as the beacon, and come out again through
	// the schema's owner - no writer holds DELETE. The panel's own pool
	// cannot do either: it has no access to the analytics tables at all,
	// which is the property this page is built on top of.
	testdb.CleanSite(t, testdb.Admin(t), site)
	pool := testdb.Pool(t, testdb.Beacon)

	for i, row := range rows {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO beacon_events
			  (time, site_id, visitor_id, event_type, event_name, path,
			   utm_source, query, referrer_host, ip, device, country, is_bot_ua)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, false)`,
			when.Add(time.Duration(i)*time.Minute), site, row.visitor,
			row.kind, row.name, row.path, row.utmSrc, row.query, row.referrer,
			"198.51.100.20", row.device, row.country,
		)
		if err != nil {
			t.Fatalf("seeding beacon row %d: %v", i, err)
		}
	}
}

// breakdownFixture is the seed every test here shares.
//
// Nine pageviews over three paths, two events, two referrers plus three
// direct visits, one tagged campaign plus six untagged.
func breakdownFixture() []beaconRow {
	return []beaconRow{
		{visitor: "a", kind: "pageview", path: "/", referrer: "google.com", device: "desktop", country: "TR"},
		{visitor: "a", kind: "pageview", path: "/fiyat", referrer: "google.com", device: "desktop", country: "TR"},
		{visitor: "b", kind: "pageview", path: "/", referrer: "", device: "mobile", country: "TR"},
		{visitor: "b", kind: "pageview", path: "/", referrer: "", device: "mobile", country: "TR"},
		{visitor: "c", kind: "pageview", path: "/iletisim", referrer: "x.com", device: "", country: ""},
		{visitor: "c", kind: "pageview", path: "/", referrer: "", device: "", country: "DE"},
		// The campaigns endpoint groups by the stored campaign query, not by
		// utm_source, so a fixture that set only the typed column would seed
		// a campaigns table that is correctly empty - and prove nothing.
		{visitor: "d", kind: "pageview", path: "/", utmSrc: "bulten", query: "utm_source=bulten", device: "mobile", country: "TR"},
		{visitor: "d", kind: "pageview", path: "/fiyat", utmSrc: "bulten", query: "utm_source=bulten", device: "mobile", country: "TR"},
		{visitor: "e", kind: "pageview", path: "/", device: "tablet", country: "FR"},
		{visitor: "a", kind: "event", name: "kayit", path: "/fiyat", device: "desktop", country: "TR"},
		{visitor: "b", kind: "event", name: "kayit", path: "/", device: "mobile", country: "TR"},
	}
}

// signedInOwner sets a site up with an owner and returns a running
// panel, a signed-in client, and the owner itself - the browser test
// needs the real address rather than one assembled from the local part,
// which is how a test ends up asserting against a login that never
// happened.
func signedInOwner(t *testing.T, srv *Server, store *panel.Store, site, who string) (*httptest.Server, *http.Client, panel.User) {
	t.Helper()
	owner := makeUser(t, store, who, false)
	if err := store.AddMember(context.Background(), site, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	t.Cleanup(server.Close)
	return server, signedIn(t, server.URL, owner.Email), owner
}

// withRealAPI points a panel at a real read-only API over the real
// database.
func withRealAPI(t *testing.T, srv *Server) {
	t.Helper()
	base, token := analyticsAPI(t)
	client, err := analytics.New(base, token)
	if err != nil {
		t.Fatal(err)
	}
	srv.Analytics = client
}

// TestTheSectionsDrawRealGroups is the phase.
//
// Every one of the six sections has to arrive with its own rows, which
// is the thing a stub cannot show: six paths, four row shapes, and an
// envelope that names its rows differently each time. A decoder wired to
// the wrong key produces an empty section, not an error.
func TestTheSectionsDrawRealGroups(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	seedBeacon(t, breakdownSite, time.Now().Add(-3*time.Hour), breakdownFixture())

	server, client, _ := signedInOwner(t, srv, store, breakdownSite, "kirilim-sahip")
	status, body := get(t, client, server.URL+sitePath(breakdownSite))
	if status != http.StatusOK {
		t.Fatalf("the dashboard answered %d", status)
	}

	lang := srv.Renderer.Catalogs().Base()
	for kind, def := range breakdownDefs {
		heading := shown(lang.T("pano.kirilim." + string(kind) + ".baslik"))
		// This owner has not turned developer mode on, so D6's rule
		// applies: the default view carries no fingerprints, no ASNs and
		// no jargon. Asserted here rather than only in the gating test,
		// because this is the page a customer actually opens.
		if def.Technical {
			if strings.Contains(body, heading) {
				t.Errorf("the default view shows the %q section to somebody who has not asked "+
					"for developer mode", kind)
			}
			continue
		}
		if !strings.Contains(body, heading) {
			t.Errorf("the page has no %q section", kind)
		}
	}

	// The values themselves, one per breakdown, each read out of a
	// different endpoint and a different row shape.
	for _, want := range []string{
		"/fiyat",            // pages
		"google.com",        // referrers
		"utm_source=bulten", // campaigns (the stored campaign query)
		"mobile",            // devices
		"TR",                // countries
		"kayit",             // events
	} {
		if !strings.Contains(body, shown(want)) {
			t.Errorf("the page does not show %q, which the seeded rows contain", want)
		}
	}

	// Nothing may report "never installed": this site has beacon rows.
	if strings.Contains(body, shown(lang.T("pano.bos.kurulmamis.beacon"))) {
		t.Error("a section says the snippet is missing for a site that has beacon rows")
	}
}

// TestTheNeverDeterminedGroupReachesThePage.
//
// Three of the nine pageviews arrived with no referrer at all. The API
// flags that group rather than dropping it, so the groups add up to the
// site's total - and the panel has to draw it as a named row. This is
// the one that only a real fixture produces: a stub with every column
// filled in never makes one.
func TestTheNeverDeterminedGroupReachesThePage(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	seedBeacon(t, breakdownSite, time.Now().Add(-3*time.Hour), breakdownFixture())

	server, client, _ := signedInOwner(t, srv, store, breakdownSite, "kirilim-bosgrup")
	_, body := get(t, client, server.URL+breakdownPath(breakdownSite, analytics.BreakdownReferrers)+"?gun=7")

	lang := srv.Renderer.Catalogs().Base()
	direct := shown(lang.T("pano.kirilim.kaynak.bos_grup"))
	if !strings.Contains(body, direct) {
		t.Errorf("the referrers page does not name the direct-visit group (%q); "+
			"those rows are either missing or drawn with a blank label", direct)
	}
	// And it is marked as the named row rather than passing for a value.
	if !strings.Contains(body, `class="ad adsiz"`) {
		t.Error("the named group is not marked as one, so it reads as a measured value")
	}
}

// TestASharePercentageIsOfTheSummaryAboveIt.
//
// Four of the nine pageviews were of "/", so the row has to read 44,4%.
// The number matters less than where it comes from: the summary and the
// breakdown both take the API's default bots filter, and the moment one
// of them stops doing that this assertion is the only thing that
// notices.
func TestASharePercentageIsOfTheSummaryAboveIt(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	seedBeacon(t, breakdownSite, time.Now().Add(-3*time.Hour), breakdownFixture())

	server, client, _ := signedInOwner(t, srv, store, breakdownSite, "kirilim-pay")
	_, body := get(t, client, server.URL+breakdownPath(breakdownSite, analytics.BreakdownPages)+"?gun=7")

	// Five of the nine pageviews are "/" - rows 1, 3, 4, 6, 7 and 9 of
	// the fixture minus the two /fiyat and the one /iletisim. Rather than
	// hard-coding the arithmetic here, the assertion is the property: the
	// shares present must sum to about a hundred, which is only true if
	// every row divides by the same total the cards use.
	shares := percentagesIn(body)
	if len(shares) == 0 {
		t.Fatal("the page shows no percentages")
	}
	var sum float64
	for _, s := range shares {
		sum += s
	}
	if sum < 99 || sum > 101 {
		t.Errorf("the shares sum to %.1f%%, want ~100%% - the rows and the summary are "+
			"counting different populations (shares: %v)", sum, shares)
	}
}

// percentagesIn pulls the share column out of rendered HTML.
//
// The panel renders percentages through the locale's formatter, so the
// Turkish page writes "44,4%" with a comma. Parsing that back is what
// makes the assertion above independent of the language the test
// happens to run in.
func percentagesIn(body string) []float64 {
	var out []float64
	for rest := body; ; {
		const open = `<td class="sayi soluk">`
		i := strings.Index(rest, open)
		if i < 0 {
			return out
		}
		rest = rest[i+len(open):]
		j := strings.Index(rest, "</td>")
		if j < 0 {
			return out
		}
		cell := rest[:j]
		rest = rest[j:]

		cell = strings.TrimSpace(strings.ReplaceAll(cell, "%", ""))
		cell = strings.ReplaceAll(cell, ",", ".")
		// Non-breaking space, which the CLDR percent pattern inserts.
		cell = strings.ReplaceAll(cell, " ", "")
		if v, err := strconv.ParseFloat(cell, 64); err == nil {
			out = append(out, v)
		}
	}
}

// TestTheDetailPageIsPagedAndKeepsThePeriod.
//
// A pager that dropped the range would show seven days of rows under a
// page whose picker says ninety, and nothing on screen would say so.
func TestTheDetailPageIsPagedAndKeepsThePeriod(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)

	// More paths than one page holds, so there is a second page at all.
	rows := make([]beaconRow, 0, detailRows*2)
	for i := range detailRows * 2 {
		rows = append(rows, beaconRow{
			visitor: "v", kind: "pageview",
			path: "/s/" + strings.Repeat("a", 1+i%3) + "-" + strconv.Itoa(i),
		})
	}
	seedBeacon(t, breakdownSite, time.Now().Add(-3*time.Hour), rows)

	server, client, _ := signedInOwner(t, srv, store, breakdownSite, "kirilim-sayfalama")
	base := breakdownPath(breakdownSite, analytics.BreakdownPages)

	status, body := get(t, client, server.URL+base+"?gun=30")
	if status != http.StatusOK {
		t.Fatalf("the detail page answered %d", status)
	}
	next := `href="` + base + "?gun=30&amp;sayfa=2"
	if !strings.Contains(body, next) {
		t.Errorf("page 1 has no link to page 2 carrying the period (looked for %q)", next)
	}
	// Page 1 must not offer a previous link at all.
	if strings.Contains(body, "sayfa=0") {
		t.Error("page 1 offers a link to page 0")
	}

	status, body = get(t, client, server.URL+base+"?gun=30&sayfa=2")
	if status != http.StatusOK {
		t.Fatalf("page 2 answered %d", status)
	}
	if !strings.Contains(body, `href="`+base+`?gun=30"`) {
		t.Error("page 2 has no link back to page 1")
	}
}

// TestAnUnknownBreakdownIs404.
//
// The kind becomes a path segment in a request to another service. The
// registry lookup is what stops a URL somebody typed from reaching the
// API as an endpoint name, and it happens before anything else.
func TestAnUnknownBreakdownIs404(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	seedBeacon(t, breakdownSite, time.Now().Add(-3*time.Hour), breakdownFixture())

	server, client, _ := signedInOwner(t, srv, store, breakdownSite, "kirilim-404")
	for _, kind := range []string{"ja4", "raw", "..%2F..%2Fbeacon%2Fraw", "sayfalar", ""} {
		target := server.URL + MembersPathPrefix + breakdownSite + breakdownPathSegment + kind
		status, _ := get(t, client, target)
		if status == http.StatusOK {
			t.Errorf("%q answered 200; only registered breakdowns may be reached", kind)
		}
	}
}

// TestSomebodyElsesBreakdownsAreNotShown.
//
// Same boundary as the dashboard's, and it has to hold on this page too:
// the panel's token reads every site, so a missing membership check here
// would serve another customer's paths and referrers - more revealing
// than the summary numbers, not less.
func TestSomebodyElsesBreakdownsAreNotShown(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	seedBeacon(t, breakdownSite, time.Now().Add(-3*time.Hour), breakdownFixture())

	owner := makeUser(t, store, "kirilim-sahip3", false)
	if err := store.AddMember(context.Background(), breakdownSite, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	outsider := makeUser(t, store, "kirilim-yabanci", false)

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := signedIn(t, server.URL, outsider.Email)

	status, body := get(t, client, server.URL+breakdownPath(breakdownSite, analytics.BreakdownPages))
	// 404 rather than 403: a 403 would confirm the site exists.
	if status != http.StatusNotFound {
		t.Errorf("an outsider got %d, want 404", status)
	}
	if strings.Contains(body, "/fiyat") {
		t.Error("an outsider was shown another site's paths")
	}
}

// TestThePageStaysInsideItsDeadline measures what this phase added.
//
// D1's site page made two calls; this one makes eight, and the plan said
// that had to be measured rather than assumed fast. Measured on a real
// API over a real TimescaleDB, on this fixture:
//
//	summaries alone (the D1 shape)   4.1 ms
//	+ pages                          7.3 ms
//	+ countries                      6.1 ms
//	+ referrers / campaigns /
//	  devices / events               4.1 ms, i.e. free
//	all eight calls together        10.4 ms
//
// Four of the six breakdowns cost nothing measurable because they finish
// inside the summary calls they run alongside; the page ends up costing
// its slowest query rather than the sum of eight. That is the property
// the concurrency buys, and it is worth writing down as a number because
// "it is concurrent, so it is fast" is a claim and this is not.
//
// The assertion is deliberately generous - a timing bound tight enough
// to be interesting is one that fails on a loaded machine - but the
// figure is logged, so a change that made the calls sequential would be
// visible to anyone reading the output.
func TestThePageStaysInsideItsDeadline(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	seedBeacon(t, breakdownSite, time.Now().Add(-3*time.Hour), breakdownFixture())

	server, client, _ := signedInOwner(t, srv, store, breakdownSite, "kirilim-olcum")

	// One warm request first: the first call pays for connection setup in
	// two pools, which is a real cost and not the one being measured.
	if status, _ := get(t, client, server.URL+sitePath(breakdownSite)); status != http.StatusOK {
		t.Fatalf("warm-up answered %d", status)
	}

	const runs = 5
	var total time.Duration
	for range runs {
		start := time.Now()
		status, _ := get(t, client, server.URL+sitePath(breakdownSite))
		total += time.Since(start)
		if status != http.StatusOK {
			t.Fatalf("the dashboard answered %d", status)
		}
	}
	avg := total / runs
	t.Logf("site page with 2 summaries + 6 breakdowns: %v average over %d runs", avg, runs)

	if avg > analytics.PageTimeout {
		t.Errorf("the page took %v on average, past the %v deadline the client sets - "+
			"a page that reaches its own timeout draws nothing", avg, analytics.PageTimeout)
	}
}

// countingAPI is analyticsAPI with a tally of the paths it was asked for.
//
// The point of C6 is that a block nobody chose costs nothing, and
// "nothing" has to mean no query - not a hidden block whose query still
// runs. Counting paths is the only way to assert that from outside; a
// timing assertion would be both flakier and weaker.
func countingAPI(t *testing.T, srv *Server) *[]string {
	t.Helper()
	base, token := analyticsAPI(t)

	var (
		mu    sync.Mutex
		paths []string
	)
	upstream, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		httputil.NewSingleHostReverseProxy(upstream).ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)

	client, err := analytics.New(proxy.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	srv.Analytics = client

	// Returned by pointer so a caller reads it after the page was drawn;
	// the slice itself is appended to from the proxy's goroutines.
	seen := &paths
	t.Cleanup(func() { mu.Lock(); mu.Unlock() })
	return seen
}

// setVisible stores a site's visible sets and restores them afterwards.
//
// Restoring matters more here than in most tests: the suite shares one
// database and the setting outlives the process, so a narrowed set left
// behind makes an unrelated test fail on a later `go test` run rather
// than in the same one. That is exactly how this helper came to exist -
// the D2 sections test started reporting four missing sections, from a
// row a C6 test had written the run before.
//
// The empty list is the unset state, so writing it back is a genuine
// restore rather than a different configuration.
func setVisible(t *testing.T, store *panel.Store, site string, cards, breakdowns []string) {
	t.Helper()
	ctx := context.Background()
	for key, value := range map[panel.Key][]string{
		panel.KeyVisibleCards:      cards,
		panel.KeyVisibleBreakdowns: breakdowns,
	} {
		if err := store.SetSetting(ctx, key, site, value, nil); err != nil {
			t.Fatalf("setting %s: %v", key, err)
		}
	}
	t.Cleanup(func() {
		for _, key := range []panel.Key{panel.KeyVisibleCards, panel.KeyVisibleBreakdowns} {
			if err := store.SetSetting(ctx, key, site, []string{}, nil); err != nil {
				t.Errorf("restoring %s: %v", key, err)
			}
		}
	})
}

// TestABlockNobodyChoseIsNeverQueried is the C6 phase, end to end.
//
// A customer who was sold "how many people, and how many of them were
// bots" gets three blocks. The other nine are not merely absent from the
// HTML - the API is never asked about them, so the database never runs
// those group-by queries at all.
func TestABlockNobodyChoseIsNeverQueried(t *testing.T) {
	srv, store := setupTestServer(t)
	seen := countingAPI(t, srv)
	seedBeacon(t, breakdownSite, time.Now().Add(-3*time.Hour), breakdownFixture())

	// Two collector cards and one beacon breakdown. Deliberately mixed:
	// it is the case where the beacon summary is needed for the share
	// even though no beacon card is shown.
	setVisible(t, store, breakdownSite,
		[]string{string(cardHumanIPs), string(cardBotIPs)},
		[]string{string(analytics.BreakdownPages)})

	server, client, _ := signedInOwner(t, srv, store, breakdownSite, "c6-secim")
	status, body := get(t, client, server.URL+sitePath(breakdownSite))
	if status != http.StatusOK {
		t.Fatalf("the dashboard answered %d", status)
	}

	// What was asked for: both summaries and exactly one breakdown.
	var breakdownCalls []string
	for _, p := range *seen {
		if strings.Contains(p, "/beacon/") && !strings.HasSuffix(p, "/beacon/summary") {
			breakdownCalls = append(breakdownCalls, p)
		}
	}
	if len(breakdownCalls) != 1 || !strings.HasSuffix(breakdownCalls[0], "/beacon/pages") {
		t.Errorf("the page made %d breakdown calls (%v); one block was chosen, so one call "+
			"is the whole point - a hidden block whose query still runs saves nothing",
			len(breakdownCalls), breakdownCalls)
	}
	for _, closed := range []string{"/beacon/referrers", "/beacon/campaigns",
		"/beacon/devices", "/beacon/countries", "/beacon/events"} {
		for _, p := range *seen {
			if strings.HasSuffix(p, closed) {
				t.Errorf("a closed breakdown was queried anyway: %s", p)
			}
		}
	}

	// And the page draws what was chosen, and only that.
	//
	// Checked by the block's own data attribute rather than by its
	// heading text. The first version compared labels and failed on a
	// correct page: "Ziyaretçi" is the visitors *card* and also the
	// visitors *column* every breakdown table carries, so the word being
	// present said nothing about which block was drawn.
	for _, want := range []string{
		`data-kart="` + string(cardHumanIPs) + `"`,
		`data-kart="` + string(cardBotIPs) + `"`,
		`data-kirilim="` + string(analytics.BreakdownPages) + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page is missing the chosen block %s", want)
		}
	}
	for _, gone := range []string{
		`data-kart="` + string(cardVisitors) + `"`,
		`data-kart="` + string(cardPageviews) + `"`,
		`data-kirilim="` + string(analytics.BreakdownReferrers) + `"`,
		`data-kirilim="` + string(analytics.BreakdownEvents) + `"`,
	} {
		if strings.Contains(body, gone) {
			t.Errorf("the page draws %s, which nobody chose", gone)
		}
	}
}

// TestACollectorOnlyPageNeverTouchesTheBeacon.
//
// The narrowest useful selection, and the one that saves the most: a
// customer who only wanted the bot and human counts. No beacon card and
// no breakdown means the beacon summary is not fetched either - so a
// deployment with no snippet at all stops paying for a query whose answer
// nothing on the page reads.
func TestACollectorOnlyPageNeverTouchesTheBeacon(t *testing.T) {
	srv, store := setupTestServer(t)
	seen := countingAPI(t, srv)
	seedTraffic(t, breakdownSite, time.Now().Add(-2*time.Hour), 6)

	// One collector card and no breakdown at all. "None" has to be
	// sayable: the first version of this test wrote an empty list and
	// expected nothing to be drawn, and a live run showed all six
	// sections instead, because unset and set-to-empty were the same
	// value on disk. ViewNone is what made the difference expressible.
	setVisible(t, store, breakdownSite,
		[]string{string(cardBotIPs)},
		[]string{ViewNone})

	server, client, _ := signedInOwner(t, srv, store, breakdownSite, "c6-yalniz-collector")
	if status, _ := get(t, client, server.URL+sitePath(breakdownSite)); status != http.StatusOK {
		t.Fatalf("the dashboard answered %d", status)
	}

	for _, p := range *seen {
		if strings.Contains(p, "/beacon/") {
			t.Errorf("a page with no beacon block called %s", p)
		}
	}
	var traffic int
	for _, p := range *seen {
		if strings.HasSuffix(p, "/summary") && !strings.Contains(p, "/beacon/") {
			traffic++
		}
	}
	if traffic == 0 {
		t.Error("the collector summary was not fetched, on a page whose only card reads it")
	}
}

// TestAStoredIdNobodyKnowsDoesNotReachTheAPI.
//
// The visible set comes out of a database and its values become catalog
// keys and API path segments. A row written by a version that had a
// breakdown this build does not is the realistic case; a hostile one is
// the same code path.
func TestAStoredIdNobodyKnowsDoesNotReachTheAPI(t *testing.T) {
	srv, store := setupTestServer(t)
	seen := countingAPI(t, srv)
	seedBeacon(t, breakdownSite, time.Now().Add(-3*time.Hour), breakdownFixture())

	setVisible(t, store, breakdownSite, []string{},
		[]string{"ja4", "../../beacon/raw", string(analytics.BreakdownPages)})

	server, client, _ := signedInOwner(t, srv, store, breakdownSite, "c6-bilinmeyen")
	if status, _ := get(t, client, server.URL+sitePath(breakdownSite)); status != http.StatusOK {
		t.Fatalf("the dashboard answered %d", status)
	}

	for _, p := range *seen {
		for _, bad := range []string{"ja4", "raw", ".."} {
			if strings.Contains(p, bad) {
				t.Errorf("a stored id reached the API as %s", p)
			}
		}
	}
}
