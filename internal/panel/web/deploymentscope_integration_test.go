//go:build integration

// H10, through the pages: a deployment-wide setting is changed only by the
// operator or by somebody who owns every site on the deployment.

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

const (
	tenantOwn   = "h10-kendi"
	tenantOther = "h10-baska"
)

// settingBlock is one setting's block on the settings page, found by
// the label it is drawn under: a row without a control carries no key in
// its markup, so the label is the one thing every row has.
func settingBlock(t *testing.T, body string, key panel.Key) string {
	t.Helper()
	def, ok := panel.DefinitionFor(key)
	if !ok {
		t.Fatalf("no definition for %s", key)
	}
	m := regexp.MustCompile(`(?s)<section class="ayar">\s*<h2>` + regexp.QuoteMeta(shown(def.Label)) + `</h2>.*?</section>`).
		FindString(body)
	if m == "" {
		t.Fatalf("the settings page has no row for %s", key)
	}
	return m
}

// TestATenantCannotChangeTheWholeDeployment.
//
// Two customers on one deployment, each the owner of their own site. The
// first one's settings page draws a deployment-wide setting as a sentence
// without a control; a form for beacon.sites - which decides whose events
// the beacon accepts at all - is refused with the sentence that says why, recorded as a
// refusal, and the list is unchanged; their own site's name is still
// theirs; the admin of their site is refused the same way.
//
// Then the other site's owner hands them that site too, and the same form
// is accepted: the other customer's site was the reason, nothing else.
//
// Before the fix the owner's and the admin's forms each answered 200 and
// cut the list down to their own site.
func TestATenantCannotChangeTheWholeDeployment(t *testing.T) {
	srv, store := setupSoleTenantServer(t)
	ctx := context.Background()
	cleanOperations(t, store)
	restoreGlobal(t, store, panel.KeyBeaconSites)
	clearSiteSettings(t, store, tenantOwn)

	owner := makeUser(t, store, "h10-sahip", false)
	admin := makeUser(t, store, "h10-yonetici", false)
	other := makeUser(t, store, "h10-oteki", false)
	for _, m := range []struct {
		site string
		user panel.User
		role panel.Role
	}{{tenantOwn, owner, panel.RoleOwner}, {tenantOwn, admin, panel.RoleAdmin}, {tenantOther, other, panel.RoleOwner}} {
		if err := store.AddMember(ctx, m.site, m.user.ID, m.role, panel.Grant{}); err != nil {
			t.Fatal(err)
		}
	}
	both := []string{tenantOwn, tenantOther}
	if err := store.SetSetting(ctx, panel.KeyBeaconSites, "", both, nil); err != nil {
		t.Fatal(err)
	}
	allowed := func() []string {
		t.Helper()
		v, err := store.GetSetting(ctx, panel.KeyBeaconSites, "")
		if err != nil {
			t.Fatal(err)
		}
		list, _ := v.([]string)
		return list
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	page := server.URL + settingsPath(tenantOwn)
	lang := srv.Renderer.Catalogs().Base()
	cutDown := url.Values{"anahtar": {string(panel.KeyBeaconSites)}, "islem": {"kaydet"}, "deger": {tenantOwn}}

	for _, who := range []panel.User{owner, admin} {
		c := signedIn(t, server.URL, who.Email)

		// The row as drawn, for a deployment-wide setting this page shows
		// without developer mode (beacon.sites is behind it).
		_, body := get(t, c, page)
		row := settingBlock(t, body, panel.KeyPrivacyVisitorSurface)
		if strings.Contains(row, "<form") {
			t.Errorf("%s is drawn a control for %s:\n%s", who.Email, panel.KeyPrivacyVisitorSurface, row)
		}
		if !strings.Contains(row, shown(panel.LockNoticeShared)) {
			t.Errorf("%s's %s row does not say it reaches every site:\n%s", who.Email, panel.KeyPrivacyVisitorSurface, row)
		}
		// A switch without its control, in the panel's words: the first
		// screenshot of this row ended in "true".
		if !strings.Contains(row, `<p class="deger-salt-okunur">`+shown(lang.T("ayarlar.acik"))+`</p>`) ||
			strings.Contains(row, "true") {
			t.Errorf("%s's %s row does not say the switch is on in words:\n%s", who.Email, panel.KeyPrivacyVisitorSurface, row)
		}

		// The form the page did not draw, sent anyway.
		status, body := postWithToken(t, c, page, sessionToken(t, c, server.URL), cutDown)
		if status == http.StatusOK {
			t.Errorf("%s cut beacon.sites down to their own site: %d", who.Email, status)
		}
		if !strings.Contains(body, shown(lang.T("ayarlar.hata.paylasilan"))) {
			t.Errorf("%s's refusal does not say the setting reaches other sites: %q", who.Email, messageOf(body))
		}
		if got := allowed(); len(got) != 2 {
			t.Fatalf("beacon.sites = %v after %s's refused form, want %v", got, who.Email, both)
		}
		if op := lastOperation(t, store, string(panel.KeyBeaconSites)); op.outcome != string(panel.OutcomeRefused) {
			t.Errorf("%s's attempt was recorded %q, want %q", who.Email, op.outcome, panel.OutcomeRefused)
		}

		// Their own site's settings are theirs.
		status, body = post(t, c, page, url.Values{
			"anahtar": {string(panel.KeySiteName)}, "islem": {"kaydet"}, "deger": {"Dükkân " + who.Email[:4]},
		})
		if status != http.StatusOK {
			t.Errorf("%s could not name their own site: %d %q", who.Email, status, messageOf(body))
		}
	}

	// The other customer gives the owner their site: every site is now
	// the owner's, and the same form is accepted.
	if err := store.AddMember(ctx, tenantOther, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	c := signedIn(t, server.URL, owner.Email)
	_, body := get(t, c, page)
	if row := settingBlock(t, body, panel.KeyPrivacyVisitorSurface); !strings.Contains(row, "<form") {
		t.Errorf("the owner of every site is drawn no control for %s:\n%s", panel.KeyPrivacyVisitorSurface, row)
	}
	if status, body := post(t, c, page, cutDown); status != http.StatusOK {
		t.Fatalf("the owner of every site was refused: %d %q", status, messageOf(body))
	}
	if got := allowed(); len(got) != 1 || got[0] != tenantOwn {
		t.Errorf("beacon.sites = %v, want [%s]", got, tenantOwn)
	}
}

// TestATenantIsNotOfferedTheDeploymentsClock.
//
// The welcome wizard's time zone is panel.timezone, every site's clock.
// The owner of one site among others is shown why instead of the field,
// and a forged form leaves the zone where it was. The owner of every site
// gets the field.
func TestATenantIsNotOfferedTheDeploymentsClock(t *testing.T) {
	srv, store := setupSoleTenantServer(t)
	ctx := context.Background()
	restoreGlobal(t, store, panel.KeyPanelTimezone)

	owner := makeUser(t, store, "h10-saat", false)
	other := makeUser(t, store, "h10-saat-oteki", false)
	if err := store.AddMember(ctx, tenantOwn, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, tenantOther, other.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	page := server.URL + WelcomePathPrefix + "saat"
	lang := srv.Renderer.Catalogs().Base()
	c := signedIn(t, server.URL, owner.Email)

	_, body := get(t, c, page)
	if strings.Contains(body, `name="saat_dilimi"`) {
		t.Error("the owner of one site among others is offered the deployment's time zone")
	}
	if !strings.Contains(body, `id="saat-paylasilan"`) || !strings.Contains(body, shown(lang.T("hosgeldiniz.saat.yetki"))) {
		t.Error("the step does not say why the zone is not theirs")
	}
	status, _ := postWithToken(t, c, page, sessionToken(t, c, server.URL), url.Values{"saat_dilimi": {"Asia/Tokyo"}})
	if status == http.StatusOK {
		t.Errorf("a forged zone answered %d", status)
	}
	if v, _ := store.GetSetting(ctx, panel.KeyPanelTimezone, ""); v == "Asia/Tokyo" {
		t.Fatal("the owner of one site among others moved every site's clock")
	}

	if err := store.AddMember(ctx, tenantOther, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	_, body = get(t, c, page)
	if !strings.Contains(body, `name="saat_dilimi"`) {
		t.Error("the owner of every site is not offered the time zone")
	}
}

// TestAnUnansweredOwnershipQuestionRefuses.
//
// speaksForDeployment asks the database; when the database does
// not answer, the deployment-wide write is refused. Widening it for want
// of an answer is the one direction it must not err in, and nothing else
// in the suite makes the question fail.
func TestAnUnansweredOwnershipQuestionRefuses(t *testing.T) {
	dead, err := panel.NewStore(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	dead.Close()
	srv := &Server{Store: dead}
	p := panel.Principal{Kind: panel.PrincipalUser, UserID: 1}
	if _, err := dead.OwnsEverySite(context.Background(), p.UserID); err == nil {
		t.Fatal("a closed store answered; this test is not making the question fail")
	}
	if srv.speaksForDeployment(context.Background(), p) {
		t.Error("an ownership question the database did not answer was taken as yes")
	}
}

// TestTheWelcomeWizardShowsATenantOnlyItsOwnSites.
//
// Found on the live rig: the welcome wizard drew a name field for every
// configured site, so the owner of one customer's site read the other
// customer's site id and stored name, and - once H12 passed beacon_url on
// to the server - its snippet. The save refused the field; drawing it was
// the leak. The page now draws exactly what the save accepts.
func TestTheWelcomeWizardShowsATenantOnlyItsOwnSites(t *testing.T) {
	srv, store := setupSoleTenantServer(t)
	srv.BeaconURL = "https://olcum.example"
	ctx := context.Background()
	restoreGlobal(t, store, panel.KeyBeaconSites)
	clearSiteSettings(t, store, tenantOwn)
	clearSiteSettings(t, store, tenantOther)

	owner := makeUser(t, store, "h12-hosgeldin", false)
	other := makeUser(t, store, "h12-hosgeldin-oteki", false)
	if err := store.AddMember(ctx, tenantOwn, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, tenantOther, other.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSetting(ctx, panel.KeyBeaconSites, "", []string{tenantOwn, tenantOther}, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSetting(ctx, panel.KeySiteName, tenantOther, "Öteki Müşteri Blog", nil); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	c := signedIn(t, server.URL, owner.Email)

	_, names := get(t, c, server.URL+WelcomePathPrefix+"site")
	if !strings.Contains(names, `name="ad:`+tenantOwn+`"`) {
		t.Fatal("the owner is not offered their own site's name")
	}
	_, snippets := get(t, c, server.URL+WelcomePathPrefix+"olcum")
	if !strings.Contains(snippets, `data-site=&#34;`+tenantOwn+`&#34;`) && !strings.Contains(snippets, `data-site="`+tenantOwn+`"`) {
		t.Fatalf("the owner is not shown their own snippet:\n%s", messageOf(snippets))
	}
	for step, body := range map[string]string{"site": names, "olcum": snippets} {
		for _, leak := range []string{tenantOther, "Öteki Müşteri Blog"} {
			if strings.Contains(body, leak) {
				t.Errorf("step %q shows the owner of %s another customer's %q", step, tenantOwn, leak)
			}
		}
	}

	// A forged field for the other site names nothing.
	if status, _ := postWithToken(t, c, server.URL+WelcomePathPrefix+"site", sessionToken(t, c, server.URL),
		url.Values{"ad:" + tenantOwn: {"Kendi"}, "ad:" + tenantOther: {"Ele geçirildi"}}); status >= 500 {
		t.Fatalf("saving names answered %d", status)
	}
	if v, _ := store.GetSetting(ctx, panel.KeySiteName, tenantOther); v != "Öteki Müşteri Blog" {
		t.Errorf("the other customer's site name became %v", v)
	}
}
