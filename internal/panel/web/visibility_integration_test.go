//go:build integration

// C6 end to end: the installer picks what the customer sees, and the
// choice survives a round trip through the database into the page.

package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
)

const visibilitySite = "gorunum-testi"

// storedSet reads one of the two visible-set settings back.
func storedSet(t *testing.T, store *panel.Store, key panel.Key, site string) []string {
	t.Helper()
	value, err := store.GetSetting(context.Background(), key, site)
	if err != nil {
		t.Fatalf("reading %s: %v", key, err)
	}
	list, ok := value.([]string)
	if !ok {
		t.Fatalf("%s is %T, not a string list", key, value)
	}
	return list
}

// postForm fetches the step for its CSRF token and posts the form back.
//
// Two round trips rather than one because the token is per-session and
// lives in the page: a test that posted without it would exercise the
// CSRF refusal instead of the handler, and pass for the wrong reason.
func postForm(t *testing.T, c *http.Client, target string, form url.Values) (int, string) {
	t.Helper()
	_, page := get(t, c, target)

	values := url.Values{"csrf_token": {csrfFrom(t, page)}}
	for k, vs := range form {
		values[k] = vs
	}
	resp, err := c.PostForm(target, values)
	if err != nil {
		t.Fatalf("posting %s: %v", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// signedInDeveloper starts a panel and signs a superadmin into it.
//
// The wizard is not an owner's page: developerAccess opens it to a
// superadmin outright and sends a mere site owner to the technical door
// first. A test that signed in as an owner would be redirected, which is
// how the first run of this file reported a 303 rather than a form.
func signedInDeveloper(t *testing.T, srv *Server, store *panel.Store, who string) (string, *http.Client) {
	t.Helper()
	dev := makeUser(t, store, who, true)
	server := httptest.NewServer(srv.Handler())
	t.Cleanup(server.Close)
	return server.URL, signedIn(t, server.URL, dev.Email)
}

// visibilityWizard signs a developer into the wizard with one site
// configured, and returns the running panel and that session.
func visibilityWizard(t *testing.T, srv *Server, store *panel.Store) (string, *http.Client) {
	t.Helper()
	if err := store.SetSetting(context.Background(), panel.KeyBeaconSites, "",
		[]string{visibilitySite}, nil); err != nil {
		t.Fatal(err)
	}
	return signedInDeveloper(t, srv, store, "c6-sihirbaz")
}

// TestTheStepOffersEveryBlockPreTicked.
//
// Pre-ticked with what the site shows today, which for an unconfigured
// deployment is the default set. An installer who opens this step and
// saves without touching anything has to get what was already there -
// unticking everything is how you say "none", so a blank form saved by
// accident would be the worst possible misreading of a click.
func TestTheStepOffersEveryBlockPreTicked(t *testing.T) {
	srv, store := setupTestServer(t)
	base, client := visibilityWizard(t, srv, store)

	status, body := get(t, client, base+SetupPathPrefix+"gorunum")
	if status != http.StatusOK {
		t.Fatalf("the step answered %d", status)
	}

	// Every card and every breakdown is offered, not only the default
	// ones: the point of the step is turning the others on too.
	for id := range cards {
		if !strings.Contains(body, `value="`+string(id)+`"`) {
			t.Errorf("the step does not offer the %q card", id)
		}
	}
	for kind := range breakdownDefs {
		if !strings.Contains(body, `value="`+string(kind)+`"`) {
			t.Errorf("the step does not offer the %q breakdown", kind)
		}
	}

	// And the defaults arrive ticked. Counted rather than matched by
	// position: the attribute order html/template emits is its business.
	if got := strings.Count(body, " checked"); got != len(defaultCards)+len(defaultBreakdowns) {
		t.Errorf("%d boxes are ticked, want %d - an unconfigured site shows the default set, "+
			"so the form has to open on it", got, len(defaultCards)+len(defaultBreakdowns))
	}
}

// TestSavingASubsetStoresExactlyThat.
func TestSavingASubsetStoresExactlyThat(t *testing.T) {
	srv, store := setupTestServer(t)
	base, client := visibilityWizard(t, srv, store)

	form := url.Values{
		visibilityField(cardFieldPrefix, visibilitySite): {
			string(cardHumanIPs), string(cardBotIPs),
		},
		visibilityField(breakdownFieldPrefix, visibilitySite): {
			string(analytics.BreakdownPages),
		},
	}
	if status, _ := postForm(t, client, base+SetupPathPrefix+"gorunum", form); status != http.StatusOK {
		t.Fatalf("saving answered %d", status)
	}

	if got := storedSet(t, store, panel.KeyVisibleCards, visibilitySite); !slices.Equal(got,
		[]string{string(cardHumanIPs), string(cardBotIPs)}) {
		t.Errorf("stored cards = %v", got)
	}
	if got := storedSet(t, store, panel.KeyVisibleBreakdowns, visibilitySite); !slices.Equal(got,
		[]string{string(analytics.BreakdownPages)}) {
		t.Errorf("stored breakdowns = %v", got)
	}
}

// TestUntickingEverythingIsStoredAsNone.
//
// The case the first draft of this phase could not express. An empty
// list is what every deployment that predates the setting already has,
// so storing one would have meant "the default" - and an installer who
// deliberately cleared the form would have found it all back on the next
// page load with nothing on screen to explain why.
func TestUntickingEverythingIsStoredAsNone(t *testing.T) {
	srv, store := setupTestServer(t)
	base, client := visibilityWizard(t, srv, store)

	// A form with the CSRF token and no checkboxes at all: exactly what a
	// browser sends when every box is cleared.
	if status, _ := postForm(t, client, base+SetupPathPrefix+"gorunum", url.Values{}); status != http.StatusOK {
		t.Fatalf("saving answered %d", status)
	}

	for _, key := range []panel.Key{panel.KeyVisibleCards, panel.KeyVisibleBreakdowns} {
		if got := storedSet(t, store, key, visibilitySite); !slices.Equal(got, []string{ViewNone}) {
			t.Errorf("%s = %v, want the reserved none", key, got)
		}
	}

	// And the page draws the sentence rather than blank space.
	status, body := get(t, client, base+sitePath(visibilitySite))
	if status != http.StatusOK {
		t.Fatalf("the dashboard answered %d", status)
	}
	if !strings.Contains(body, shown(srv.Renderer.Catalogs().Base().T("pano.hicbiri"))) {
		t.Error("a page with nothing selected says nothing about why it is empty")
	}
	if strings.Contains(body, `data-kart=`) || strings.Contains(body, `data-kirilim=`) {
		t.Error("a page with nothing selected still drew a block")
	}
}

// TestAPostedIdNobodyKnowsIsDropped.
//
// The form only ever offers real ids, so anything else was not typed by
// the person at the screen. It must not be stored, because a stored id
// becomes a catalog key and an API path segment on every later page load.
func TestAPostedIdNobodyKnowsIsDropped(t *testing.T) {
	srv, store := setupTestServer(t)
	base, client := visibilityWizard(t, srv, store)

	form := url.Values{
		visibilityField(cardFieldPrefix, visibilitySite): {
			"ja4", string(cardBotIPs), "../../etc/passwd",
		},
		visibilityField(breakdownFieldPrefix, visibilitySite): {"raw"},
	}
	if status, _ := postForm(t, client, base+SetupPathPrefix+"gorunum", form); status != http.StatusOK {
		t.Fatalf("saving answered %d", status)
	}

	if got := storedSet(t, store, panel.KeyVisibleCards, visibilitySite); !slices.Equal(got,
		[]string{string(cardBotIPs)}) {
		t.Errorf("stored cards = %v, want only the real one", got)
	}
	// Every breakdown id was rubbish, so nothing was ticked - which is
	// the same thing as unticking them all.
	if got := storedSet(t, store, panel.KeyVisibleBreakdowns, visibilitySite); !slices.Equal(got,
		[]string{ViewNone}) {
		t.Errorf("stored breakdowns = %v", got)
	}
}

// TestTheChoiceIsPerSite. Two customers on one deployment must be able to
// want different things; a setting that leaked between them would show
// one of them somebody else's idea of a dashboard.
func TestTheChoiceIsPerSite(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const other = "gorunum-testi-2"

	if err := store.SetSetting(ctx, panel.KeyBeaconSites, "",
		[]string{visibilitySite, other}, nil); err != nil {
		t.Fatal(err)
	}
	base, client := signedInDeveloper(t, srv, store, "c6-iki-site")

	form := url.Values{
		visibilityField(cardFieldPrefix, visibilitySite):      {string(cardBotIPs)},
		visibilityField(breakdownFieldPrefix, visibilitySite): {string(analytics.BreakdownPages)},
		visibilityField(cardFieldPrefix, other):               {string(cardVisitors)},
		visibilityField(breakdownFieldPrefix, other):          {string(analytics.BreakdownEvents)},
	}
	if status, _ := postForm(t, client, base+SetupPathPrefix+"gorunum", form); status != http.StatusOK {
		t.Fatalf("saving answered %d", status)
	}

	if got := storedSet(t, store, panel.KeyVisibleCards, visibilitySite); !slices.Equal(got,
		[]string{string(cardBotIPs)}) {
		t.Errorf("site one's cards = %v", got)
	}
	if got := storedSet(t, store, panel.KeyVisibleCards, other); !slices.Equal(got,
		[]string{string(cardVisitors)}) {
		t.Errorf("site two's cards = %v", got)
	}
}

// TestTheStepSaysSoWhenThereAreNoSites, rather than drawing an empty
// form that reads as a page which failed to load.
func TestTheStepSaysSoWhenThereAreNoSites(t *testing.T) {
	srv, store := setupTestServer(t)
	// Cleared explicitly. The suite shares one database, so "no sites" is
	// a state this test has to create rather than assume - the first run
	// of it failed against sites another test had left behind.
	if err := store.SetSetting(context.Background(), panel.KeyBeaconSites, "",
		[]string{}, nil); err != nil {
		t.Fatal(err)
	}
	base, client := signedInDeveloper(t, srv, store, "c6-sitesiz")

	_, body := get(t, client, base+SetupPathPrefix+"gorunum")
	want := shown(srv.Renderer.Catalogs().Base().T("kurulum.adim.gorunum.site_yok"))
	if !strings.Contains(body, want) {
		t.Errorf("the step with no sites configured does not say so")
	}
}
