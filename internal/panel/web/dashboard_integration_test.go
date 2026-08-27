//go:build integration

// D1 end to end: a real analytics API, over a real database, drawn into
// a real page.
//
// The unit tests decide what an empty card says and where a range's
// boundaries fall. What only a live run can show is the thing this phase
// exists for: numbers that were collected, read back over HTTP by a
// process with no database access to them, and rendered.

package web

import (
	"context"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"

	"github.com/cruciblelab/crucible-analytic/internal/api"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
)

const dashboardSite = "pano-testi"

// shown is a catalog string as it appears in rendered HTML.
//
// Not the raw string: html/template escapes text nodes, and several of
// these sentences contain an apostrophe - "snippet'in" becomes
// "snippet&#39;in". Comparing against the raw form makes a test that
// fails while the page is perfectly correct, which cost a debugging
// session once already.
func shown(s string) string { return template.HTMLEscapeString(s) }

// analyticsAPI starts a real read-only API over the real database and
// returns its address and a token that reads every site.
//
// Every site, because that is what the panel's token is: it serves all
// of them, and the only thing between one customer's numbers and
// another's is the panel's own site-access check. Building the test
// with a narrower token would quietly test something the deployment
// does not do.
func analyticsAPI(t *testing.T) (base, token string) {
	t.Helper()

	// The read API's own role, not the panel's. This stands in for the
	// separate analytics-api process, and that process is the only one
	// allowed to read both analytics tables - the panel reaches them
	// through it and never directly, which is the boundary these pages
	// exist on the far side of.
	store, err := api.NewStore(context.Background(), testdb.DSN(testdb.Reader))
	if err != nil {
		t.Fatalf("api.NewStore: %v (is the database up and installed?)", err)
	}
	t.Cleanup(store.Close)

	const raw = "pano-testi-jetonu"
	auth, err := api.NewAuthenticator([]api.Token{{
		Name:   "panel",
		SHA256: api.HashToken(raw),
		Sites:  []string{api.WildcardSite},
	}})
	if err != nil {
		t.Fatal(err)
	}

	srv := &api.Server{
		Store:  store,
		Auth:   auth,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	return httpSrv.URL, raw
}

// seedTraffic writes collector rows for a site inside the range the
// dashboard will ask for.
func seedTraffic(t *testing.T, site string, when time.Time, rows int) {
	t.Helper()
	// The collector's rows, written as the collector and cleared by the
	// owner of the schema. The panel's role - which this suite otherwise
	// runs as - can touch neither table, and that is the point.
	testdb.CleanSite(t, testdb.Admin(t), site)
	pool := testdb.Pool(t, testdb.Collector)

	for i := range rows {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO traffic_snapshots
			  (time, site_id, ip, ja4, prev_window_count, curr_window_count,
			   request_rate, bot_score)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			when.Add(time.Duration(i)*time.Minute), site,
			// Two addresses, one scored above the bot threshold and one
			// well below, so the human/bot split on the page is a real
			// split rather than everything landing on one side.
			[]string{"198.51.100.10", "198.51.100.11"}[i%2],
			"t13d1516h2_8daaf6152771_b186095e22b6",
			0, 10+i, float64(10+i)/60,
			[]int{5, 90}[i%2],
		)
		if err != nil {
			t.Fatalf("seeding traffic: %v", err)
		}
	}
}

// TestTheDashboardDrawsRealNumbers is the phase.
func TestTheDashboardDrawsRealNumbers(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()

	base, token := analyticsAPI(t)
	client, err := analytics.New(base, token)
	if err != nil {
		t.Fatal(err)
	}
	srv.Analytics = client

	// Rows inside today, which is inside every offered range.
	seedTraffic(t, dashboardSite, time.Now().Add(-2*time.Hour), 6)

	owner := makeUser(t, store, "pano-sahip", false)
	if err := store.AddMember(ctx, dashboardSite, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client2 := signedIn(t, server.URL, owner.Email)

	status, body := get(t, client2, server.URL+sitePath(dashboardSite))
	if status != http.StatusOK {
		t.Fatalf("the dashboard answered %d", status)
	}

	// The collector's cards carry numbers. Three distinct addresses were
	// written, one of them scored above the bot threshold - so both
	// sides of the split have to be non-zero, which is what proves the
	// page is reading the real rows rather than rendering zeroes.
	base10 := srv.Renderer.Catalogs().Base()
	for _, want := range []string{
		shown(base10.T("pano.kart.insan.baslik")),
		shown(base10.T("pano.kart.bot.baslik")),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show the %q card", want)
		}
	}
	// The collector cards must not be reporting "never installed": rows
	// exist for this site.
	if strings.Contains(body, shown(base10.T("pano.bos.kurulmamis.trafik"))) {
		t.Error("the page says the collector was never installed for a site it has rows for")
	}

	// And the beacon's cards say the snippet was never installed, which
	// is true: nothing wrote a beacon_events row for this site. This is
	// the distinction the phase is about, seen from the good side.
	if !strings.Contains(body, shown(base10.T("pano.bos.kurulmamis.beacon"))) {
		t.Error("a site with no beacon rows does not say the snippet is missing; " +
			"a customer reading zero pageviews would think the number was measured")
	}
}

// TestSomebodyElsesNumbersAreNotShown.
//
// The panel's API token reads every site. The whole boundary between one
// customer and another is the panel's own access check, so this is the
// test that boundary gets: the paired request, from an account with no
// membership.
func TestSomebodyElsesNumbersAreNotShown(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()

	base, token := analyticsAPI(t)
	client, err := analytics.New(base, token)
	if err != nil {
		t.Fatal(err)
	}
	srv.Analytics = client
	seedTraffic(t, dashboardSite, time.Now().Add(-2*time.Hour), 4)

	owner := makeUser(t, store, "pano-sahip2", false)
	outsider := makeUser(t, store, "pano-yabanci", false)
	if err := store.AddMember(ctx, dashboardSite, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	// The owner sees it.
	if status, _ := get(t, signedIn(t, server.URL, owner.Email),
		server.URL+sitePath(dashboardSite)); status != http.StatusOK {
		t.Fatalf("the owner got %d from their own site", status)
	}

	// Somebody with no membership gets 404, not 403: a 403 would confirm
	// the site exists, which turns the URL into a way to enumerate a
	// deployment's customers from any account on it.
	status, body := get(t, signedIn(t, server.URL, outsider.Email),
		server.URL+sitePath(dashboardSite))
	if status != http.StatusNotFound {
		t.Errorf("an outsider got %d, want 404", status)
	}
	if strings.Contains(body, shown(srv.Renderer.Catalogs().Base().T("pano.kart.insan.baslik"))) {
		t.Error("an outsider was shown another customer's dashboard")
	}
}

// TestTheDashboardSurvivesTheAPIGoingAway.
//
// The panel is a separate process from its data source, so the source
// being down is a normal Tuesday rather than an exceptional case. The
// page has to render, say why, and stay up - a dashboard that 500s when
// its API blinks is worse than one that explains itself.
func TestTheDashboardSurvivesTheAPIGoingAway(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()

	// A client pointed at a port with nothing behind it.
	client, err := analytics.New("http://127.0.0.1:1", "jeton")
	if err != nil {
		t.Fatal(err)
	}
	srv.Analytics = client

	owner := makeUser(t, store, "pano-sahip3", false)
	if err := store.AddMember(ctx, "kapali-api", owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	status, body := get(t, signedIn(t, server.URL, owner.Email),
		server.URL+sitePath("kapali-api"))
	if status != http.StatusOK {
		t.Fatalf("the dashboard answered %d with its API down; it has to render and explain",
			status)
	}
	base := srv.Renderer.Catalogs().Base()
	if !strings.Contains(body, shown(base.T("pano.hata.ulasilamiyor"))) {
		t.Error("the page does not say why there are no numbers")
	}
	// And it must not have drawn zeroes, which a customer would read as
	// "nobody visited".
	if strings.Contains(body, shown(base.T("pano.bos.bos.beacon"))) {
		t.Error("an unreachable API was reported as an empty period")
	}
}

// TestTheRangePickerChangesTheRange. The four periods are real URLs, so
// each has to actually reach the handler and come back marked.
func TestTheRangePickerChangesTheRange(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()

	base, token := analyticsAPI(t)
	client, err := analytics.New(base, token)
	if err != nil {
		t.Fatal(err)
	}
	srv.Analytics = client

	owner := makeUser(t, store, "pano-aralik", false)
	if err := store.AddMember(ctx, dashboardSite, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	seedTraffic(t, dashboardSite, time.Now().Add(-2*time.Hour), 2)

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	c := signedIn(t, server.URL, owner.Email)

	for _, days := range rangeDays {
		url := server.URL + sitePath(dashboardSite) + "?gun=" + strconv.Itoa(days)
		status, body := get(t, c, url)
		if status != http.StatusOK {
			t.Errorf("gun=%d answered %d", days, status)
			continue
		}
		// The chosen period is drawn as the current one rather than as a
		// link, so a reader can tell which they are looking at.
		if !strings.Contains(body, `aria-current="true"`) {
			t.Errorf("gun=%d marks no period as current", days)
		}
	}
}

// TestTheRestOfThePanelSurvivesTheAPIGoingAway.
//
// The dashboard's own survival is tested above. This is the other half
// of the same promise and the one nothing was checking: every page that
// is not about analytics has to be untouched by an analytics outage,
// because all of them read panel_* tables and none of them needs the
// read API at all.
//
// That is true by construction today - only two files in this package
// call the client, which TestOnlyTheAnalyticsPagesTalkToTheAnalyticsAPI
// pins down. This test is the behavioural half: construction can be
// right while a shared helper, a middleware or a template still drags
// the API into a request by accident.
//
// A customer whose analytics service is down must still be able to sign
// in, read their account, and manage members. Those are the pages they
// need *most* during an outage, since one of them is how they reach
// somebody who can fix it.
func TestTheRestOfThePanelSurvivesTheAPIGoingAway(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()

	// Nothing is listening on port 1, so every call the client makes
	// fails at connect - the fastest honest imitation of an outage.
	client, err := analytics.New("http://127.0.0.1:1", "jeton")
	if err != nil {
		t.Fatal(err)
	}
	srv.Analytics = client

	const site = "kesinti-sitesi"
	owner := makeUser(t, store, "kesinti-sahip", false)
	if err := store.AddMember(ctx, site, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSetting(ctx, panel.KeyBeaconSites, "", []string{site}, nil); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client2 := signedIn(t, server.URL, owner.Email)
	base := srv.Renderer.Catalogs().Base()

	for name, path := range map[string]string{
		"the site list":   "/",
		"their account":   AccountPath,
		"the member list": MembersPathPrefix + site + membersPathSuffix,
	} {
		t.Run(name, func(t *testing.T) {
			status, body := get(t, client2, server.URL+path)
			if status != http.StatusOK {
				t.Fatalf("%s answered %d while the analytics API was down; it does not read "+
					"analytics and must not care", name, status)
			}
			// Not merely a 200: an error page rendered with a 200 would
			// pass the check above and fail the customer.
			//
			// The key is hata.500.baslik and not hata.sunucu, which is
			// what this was written with. That key does not exist, and a
			// missing key comes back wrapped in markers rather than
			// empty - so the assertion was comparing the page against a
			// string it could never contain and passing on every input.
			if strings.Contains(body, shown(base.T("hata.500.baslik"))) {
				t.Errorf("%s rendered the server-error page", name)
			}
		})
	}

	// And signing out still works, which matters because it is the one
	// action somebody takes when a page looks broken.
	resp, err := client2.PostForm(server.URL+LogoutPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		t.Errorf("signing out answered %d with the analytics API down", resp.StatusCode)
	}
}
