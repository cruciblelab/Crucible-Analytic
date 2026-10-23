//go:build integration

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/siteid"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// A site that cannot exist is not found - for an operator too (PLAN §H8).
//
// The identifier comes from the path, and an operator's access does not
// depend on membership: before H8, whatever a URL said became the
// site_id of a setting, an audit entry, an invitation - columns in
// indexes that assume a site id's 64 bytes, and PostgreSQL refuses an
// entry past 2704 along with the statement. siteAccess is every site
// page's door, so it is asked there.
//
// The controls are identifiers the deployment does not know, two of them
// outside the configuration files' pattern: the operator still reaches
// all three. So the 404s above are the bound's and not "unknown site" -
// and a site a deployment already has members under, dotted or not, is
// not lost to the rule (internal/siteid says why that matters).
func TestASiteThatCannotExistIsNotFoundEvenForAnOperator(t *testing.T) {
	srv, store := setupTestServer(t)
	operator := makeUser(t, store, "h8-isletmeci", true)
	server := httptest.NewServer(srv.Handler())
	t.Cleanup(server.Close)
	client := signedIn(t, server.URL, operator.Email)

	page := func(site string) string {
		return server.URL + MembersPathPrefix + url.PathEscape(site) + membersPathSuffix
	}
	for name, site := range map[string]string{
		"257 characters":  strings.Repeat("x", 257),
		"3000 characters": strings.Repeat("x", 3000),
		"a control":       "site\x01",
	} {
		if status, _ := get(t, client, page(site)); status != http.StatusNotFound {
			t.Errorf("%s: the members page answered %d, want 404", name, status)
		}
	}
	for _, site := range []string{"h8-bilinmeyen", "bir site", "site.tr"} {
		if status, _ := get(t, client, page(site)); status == http.StatusNotFound {
			t.Errorf("%q answered 404 for an operator; the cases above cannot tell the "+
				"bound's refusal from an unknown site's", site)
		}
	}
}

// No site the panel writes rows under can fail to be a key.
//
// checkSiteList refuses an unkeyable entry when the setting is saved -
// but only since H8, and a list saved before then can hold any string.
// The panel writes rows under every configured site (site names in the
// welcome wizard, an owner's memberships when a claim is redeemed), into
// indexes PostgreSQL refuses an entry past 2704 bytes in.
//
// What keeps such an entry out of those rows is not a filter of its own:
// the panel re-validates every stored setting as it reads it, and a list
// the check refuses reads as the registry's default - an empty list, for
// the whole of it. The beacon, which does not share that reader, leaves
// only the bad entry out - see TestTheLiveSiteListKeepsOnlySiteIdentifiers
// (NOTES, H8, says what the difference costs). So the assertion is the
// property, not the mechanism: nothing configuredSites returns is
// unkeyable.
//
// The row is written as a panel of that age would have written it - past
// the validator, straight into the table - and put back afterwards.
func TestTheConfiguredSitesLeaveOutWhatNoSiteCanBe(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	admin := testdb.Admin(t)

	var before []byte
	err := admin.QueryRow(ctx, `SELECT value::text FROM panel_settings
		WHERE scope = 'global' AND site_id = '' AND key = $1`, string(panel.KeyBeaconSites)).Scan(&before)
	had := err == nil
	t.Cleanup(func() {
		bg := context.Background()
		if had {
			_, err = admin.Exec(bg, `UPDATE panel_settings SET value = $2::jsonb
				WHERE scope = 'global' AND site_id = '' AND key = $1`, string(panel.KeyBeaconSites), string(before))
		} else {
			_, err = admin.Exec(bg, `DELETE FROM panel_settings
				WHERE scope = 'global' AND site_id = '' AND key = $1`, string(panel.KeyBeaconSites))
		}
		if err != nil {
			t.Errorf("putting beacon.sites back: %v", err)
		}
	})

	long := strings.Repeat("x", 3000)
	if _, err := admin.Exec(ctx, `
		INSERT INTO panel_settings (scope, site_id, key, value)
		VALUES ('global', '', $1, jsonb_build_array('h8-gecerli', 'bir site', $2::text, 'h8-ikinci'))
		ON CONFLICT (scope, site_id, key) DO UPDATE SET value = EXCLUDED.value`,
		string(panel.KeyBeaconSites), long); err != nil {
		t.Fatal(err)
	}
	// And the validator refuses the long entry today, which is what makes
	// the row above one only an older panel could have saved.
	if err := store.SetSetting(ctx, panel.KeyBeaconSites, "", []string{"h8-gecerli", long}, nil); err == nil {
		t.Error("the setting accepted an entry that cannot be a site's key")
	}

	got, err := srv.configuredSites(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, site := range got {
		if !siteid.Keyable(site) {
			t.Errorf("configuredSites returned %.40q..., which cannot be a site's key - the "+
				"panel would write rows under it", site)
		}
	}
	if slices.Contains(got, long) {
		t.Error("the over-long entry reached the panel's writers")
	}
}
