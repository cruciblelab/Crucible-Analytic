//go:build integration

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// H13. Every form that writes a setting leaves a row in the audit log
// saying who changed what from what. The settings page always did, through
// ApplySetting; the two wizards and the visibility step called
// SetSetting, which records nothing - so beacon.sites, the setting that
// decides whose events the beacon accepts, had no history at all. The
// live rig showed it: two writes, one of them after the handover, zero
// rows.

// changesRecorded counts the setting.changed rows for key and site that
// name actor.
func changesRecorded(t *testing.T, store *panel.Store, key panel.Key, site, actor string) int {
	t.Helper()
	var n int
	if err := store.Pool().QueryRow(context.Background(), `
		SELECT count(*) FROM panel_audit_log
		 WHERE action = $1 AND target = $2 AND coalesce(site_id, '') = $3 AND actor_label = $4`,
		string(panel.ActionSettingChanged), string(key), site, actor).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTheSetupWizardsSiteListIsRecorded(t *testing.T) {
	srv, store := setupTestServer(t)
	restoreGlobal(t, store, panel.KeyBeaconSites)
	dev := makeUser(t, store, "h13-kurulum", true)
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	c := signedIn(t, server.URL, dev.Email)

	if status, _ := postForm(t, c, server.URL+SetupPathPrefix+"siteler",
		url.Values{"siteler": {"h13-a\nh13-b"}}); status != http.StatusOK {
		t.Fatalf("saving the site list answered %d", status)
	}
	if n := changesRecorded(t, store, panel.KeyBeaconSites, "", dev.Email); n != 1 {
		t.Errorf("saving the site list left %d audit rows, want 1", n)
	}
}

func TestTheVisibilityStepIsRecorded(t *testing.T) {
	srv, store := setupTestServer(t)
	restoreGlobal(t, store, panel.KeyBeaconSites)
	clearSiteSettings(t, store, visibilitySite)
	if err := store.SetSetting(context.Background(), panel.KeyBeaconSites, "", []string{visibilitySite}, nil); err != nil {
		t.Fatal(err)
	}
	dev := makeUser(t, store, "h13-gorunum", true)
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	c := signedIn(t, server.URL, dev.Email)

	form := url.Values{visibilityField(cardFieldPrefix, visibilitySite): {string(cardBotIPs)}}
	if status, _ := postForm(t, c, server.URL+SetupPathPrefix+"gorunum", form); status != http.StatusOK {
		t.Fatalf("saving the visibility step answered %d", status)
	}
	for _, key := range []panel.Key{panel.KeyVisibleCards, panel.KeyVisibleBreakdowns} {
		if n := changesRecorded(t, store, key, visibilitySite, dev.Email); n != 1 {
			t.Errorf("%s: %d audit rows, want 1", key, n)
		}
	}
}

func TestTheWelcomeWizardsWritesAreRecorded(t *testing.T) {
	srv, store := setupSoleTenantServer(t)
	ctx := context.Background()
	restoreGlobal(t, store, panel.KeyPanelTimezone, panel.KeyBeaconSites)
	clearSiteSettings(t, store, tenantOwn)
	owner := makeUser(t, store, "h13-hosgeldin", false)
	if err := store.AddMember(ctx, tenantOwn, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSetting(ctx, panel.KeyBeaconSites, "", []string{tenantOwn}, nil); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	c := signedIn(t, server.URL, owner.Email)

	if status, _ := postForm(t, c, server.URL+WelcomePathPrefix+"site",
		url.Values{"ad:" + tenantOwn: {"Kahve Dükkânı"}}); status >= http.StatusBadRequest {
		t.Fatalf("saving the site name answered %d", status)
	}
	if n := changesRecorded(t, store, panel.KeySiteName, tenantOwn, owner.Email); n != 1 {
		t.Errorf("saving the site name left %d audit rows, want 1", n)
	}

	if status, _ := postForm(t, c, server.URL+WelcomePathPrefix+"saat",
		url.Values{"saat_dilimi": {"Europe/Berlin"}}); status >= http.StatusBadRequest {
		t.Fatalf("saving the time zone answered %d", status)
	}
	if n := changesRecorded(t, store, panel.KeyPanelTimezone, "", owner.Email); n != 1 {
		t.Errorf("saving the time zone left %d audit rows, want 1", n)
	}
}
