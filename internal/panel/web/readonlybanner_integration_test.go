//go:build integration

package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// The read-only banner, and where it is entitled to appear.
//
// # What it is for
//
// A viewer opening a site's settings page finds a list of settings with
// no save buttons. The banner is the sentence that explains the absence,
// said once in the chrome so that every missing control below has a
// reason the reader has already met.
//
// # What it was doing
//
// Saying the same thing on pages that have nothing to do with a site.
// The chrome computes it from two per-site capabilities, and the pages
// that belong to an account or to the deployment - the account page, the
// mail wizard, the health page, the site list, the developer-access
// page, the technical door, the welcome wizard - hand the chrome an
// Access with no site in it, because there is no site. Every capability
// on an empty Access is false, so the banner appeared.
//
// Found in a screenshot. The account page carried "this account may only
// view, it cannot change any setting" above a form offering to change
// the display name, the password, two-factor authentication, the
// recovery codes and developer mode - five things that belong to the
// account and are not anybody's to withhold.
//
// It never showed for a superadmin, because Can() returns true for one
// whatever the site is. So the reader it was wrong for was every
// ordinary customer, and the reader who looks at the panel most was the
// one it could not reach.
//
// *Hakkında karar veremeyeceği bir sayfada bir iddia basan kabuk,
// iddiayı değil kendini anlatır.*

// bannerText is enough of the sentence to find it and not so much that a
// wording change breaks a test about placement.
const bannerText = "yalnızca görüntüleme yetkisine sahip"

// bannerSite is this file's own site. Its own, because the suite shares
// one database and a membership row left on somebody else's site is a
// role somebody else's test did not ask for.
const bannerSite = "rozet-testi"

// TestTheReadOnlyBannerStaysWhereItExplainsSomething.
//
// One server for the whole test, deliberately. setupTestServer holds a
// Postgres advisory lock until its test ends, so a second call while the
// first is alive waits for a lock that cannot be released yet - measured,
// by writing it the other way first and watching it hang.
func TestTheReadOnlyBannerStaysWhereItExplainsSomething(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	ctx := context.Background()
	signIn := func(local string, superadmin bool, role panel.Role) *http.Client {
		t.Helper()
		user := makeUser(t, store, local, superadmin)
		if err := store.AddMember(ctx, bannerSite, user.ID, role, nil); err != nil {
			t.Fatal(err)
		}
		return signedIn(t, server.URL, user.Email)
	}

	viewer := signIn("rozet-izleyici", false, panel.RoleViewer)
	owner := signIn("rozet-sahip", false, panel.RoleOwner)
	operator := signIn("rozet-operator", true, panel.RoleOwner)

	// The page the banner was written for, first: a fix that simply
	// stopped printing it anywhere would pass everything below.
	t.Run("a viewer on a site's settings page is told why the buttons are missing", func(t *testing.T) {
		body := getBody(t, viewer, server.URL+sitePath(bannerSite)+"/ayarlar")
		if !strings.Contains(body, bannerText) {
			t.Error("the settings page no longer explains itself to a viewer.\n" +
				"The page below has no save buttons for this reader, and without the " +
				"banner the absence has no stated reason")
		}
	})

	// And the pages that are not about a site at all.
	for _, tc := range []struct {
		name   string
		client *http.Client
		path   string
		why    string
	}{
		{
			name: "a viewer's own account page", client: viewer, path: AccountPath,
			why: "the display name, the password, two-factor and the recovery codes " +
				"belong to the account. A viewer on a site is not a viewer of themselves",
		},
		{
			name: "an owner's own account page", client: owner, path: AccountPath,
			why: "the same, and an owner is not short of any capability at all",
		},
		{
			name: "an owner on the outgoing mail page", client: owner, path: MailPath,
			why: "whoever owns a site here may configure outgoing mail - that is what " +
				"requireMailOwner decides - so the form on this page is theirs to submit",
		},
		{
			name: "an owner on the site list", client: owner, path: "/",
			why: "a list of sites withholds nothing from anybody",
		},
		{
			name: "a viewer on the site list", client: viewer, path: "/",
			why: "the same, and this is the first page anybody signing in meets",
		},
	} {
		t.Run(tc.name+" says nothing about being read-only", func(t *testing.T) {
			body := getBody(t, tc.client, server.URL+tc.path)
			if strings.Contains(body, bannerText) {
				t.Errorf("%s carries the read-only banner.\n%s\n"+
					"A banner that is wrong is worse than no banner: it teaches its "+
					"reader to stop reading banners", tc.path, tc.why)
			}
		})
	}

	// The bug hid behind a superadmin: Access.Can is true for one on
	// every site, so the operator never met the wrong banner and nobody
	// looking at the panel daily could have found it. This holds the two
	// readers to the same page rather than trusting one of them to
	// notice.
	t.Run("the customer and the operator read the same page", func(t *testing.T) {
		for _, path := range []string{AccountPath, MailPath, HealthPath, "/"} {
			mine := strings.Contains(getBody(t, owner, server.URL+path), bannerText)
			theirs := strings.Contains(getBody(t, operator, server.URL+path), bannerText)
			if mine != theirs {
				t.Errorf("%s shows the read-only banner to the customer (%t) and not to "+
					"the operator (%t).\nNeither page is about a site, so neither reader "+
					"is missing anything - and a difference here is a defect only the "+
					"customer can see", path, mine, theirs)
			}
		}
	})
}

func getBody(t *testing.T, client *http.Client, url string) string {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s answered %d, so nothing below is a measurement of the page",
			url, resp.StatusCode)
	}
	return string(body)
}
