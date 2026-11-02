//go:build integration

// H10: a deployment-wide setting is changed only by somebody whose
// authority covers every site it changes.

package panel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// TestATenantChangesNoDeploymentWideSetting.
//
// The owner of one site among others, through the one write path every
// form takes: a global setting is refused with the shared refusal and
// stays as it was, written or cleared; a setting of their own site is
// still theirs; and the page they would read draws every global row
// without a control and with the sentence that says why.
//
// Measured before the fix through the settings page itself: an owner and
// an admin of one site each rewrote beacon.sites, blocked a country for
// the whole deployment and switched every site's disclosure page off,
// three 200s each.
func TestATenantChangesNoDeploymentWideSetting(t *testing.T) {
	store := settingsStore(t)
	ctx := context.Background()
	tenant := tenantAccess()

	before, err := store.GetIntSetting(ctx, KeyLogArchiveAfterDays, "")
	if err != nil {
		t.Fatal(err)
	}
	err = store.ApplySetting(ctx, tenant, KeyLogArchiveAfterDays, "", before+1, devgate.Authorization{}, nil)
	if !errors.Is(err, ErrSettingShared) {
		t.Fatalf("an owner of one site changed a deployment-wide setting (err = %v)", err)
	}
	// Still a refusal to every caller that knew only the general one.
	if !errors.Is(err, ErrSettingNotWritable) {
		t.Errorf("the shared refusal is not a refusal to a caller that asks for ErrSettingNotWritable: %v", err)
	}
	if got, _ := store.GetIntSetting(ctx, KeyLogArchiveAfterDays, ""); got != before {
		t.Errorf("%s = %d after the refusal, want %d", KeyLogArchiveAfterDays, got, before)
	}

	// Clearing is a change like any other: for beacon.sites, the default
	// is the empty list.
	if err := store.SetSetting(ctx, KeyBeaconSites, "", []string{"site-a", "site-b"}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.ResetSetting(context.Background(), KeyBeaconSites, "") })
	if err := store.ClearSetting(ctx, tenant, KeyBeaconSites, "", devgate.Authorization{}, nil); !errors.Is(err, ErrSettingShared) {
		t.Errorf("an owner of one site cleared a deployment-wide setting (err = %v)", err)
	}
	if got, _ := store.GetSetting(ctx, KeyBeaconSites, ""); len(toStrings(got)) != 2 {
		t.Errorf("beacon.sites = %v after the refused clear", got)
	}

	// Their own site's settings are untouched by the rule.
	if err := store.ApplySetting(ctx, tenant, KeySiteName, "site-a", "Dükkân", devgate.Authorization{}, nil); err != nil {
		t.Errorf("the owner could not name their own site: %v", err)
	}
	t.Cleanup(func() { _ = store.ResetSetting(context.Background(), KeySiteName, "site-a") })

	view, err := store.SettingsView(ctx, tenant, "site-a")
	if err != nil {
		t.Fatal(err)
	}
	shared := 0
	for _, row := range view {
		d := row.Definition
		if d.Scope != ScopeGlobal || d.RequiresDeveloperPassword || d.ConfigFileOnly {
			continue
		}
		shared++
		if row.Access != SettingShared || row.Lock != LockNoticeShared {
			t.Errorf("%s is drawn %q with %q, want %q with the shared sentence", d.Key, row.Access, row.Lock, SettingShared)
		}
	}
	if shared == 0 {
		t.Fatal("the view had no ordinary global row; the loop above asked nothing")
	}
}

// toStrings reads a stored string list.
func toStrings(v any) []string {
	out, _ := v.([]string)
	return out
}

// TestOwnsEverySiteAsksEverySiteTheDeploymentKnows is the question H10's
// rule rests on, asked of a deployment this test built row by row: the
// two tables are copies (testdb.SiteCopy), because the shared ones hold
// every site every suite ever made.
//
// Each step changes one row and says which way the answer must move; the
// steps that move it back are the ones that show the row was the reason.
func TestOwnsEverySiteAsksEverySiteTheDeploymentKnows(t *testing.T) {
	sites := testdb.NewSiteCopy(t, "ca_h10_owns_every")
	store := newTestStoreAt(t, "h10owns", sites.DSN(testDatabaseURL))
	sites.Reaches(store.Pool())
	ctx := context.Background()

	const me, other int64 = 9_100_001, 9_100_002
	step := func(what string, want bool) {
		t.Helper()
		got, err := store.OwnsEverySite(ctx, me)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if got != want {
			t.Errorf("%s: OwnsEverySite = %v, want %v", what, got, want)
		}
	}

	step("a deployment with no site at all", false)

	sites.Member("h10-a", me, "owner", time.Time{})
	step("the only site, owned", true)

	sites.Member("h10-b", other, "owner", time.Time{})
	step("a second site somebody else owns", false)
	sites.Member("h10-b", me, "owner", time.Time{})
	step("both sites owned, the other owner still there", true)

	// An admin is not an owner, even of the whole deployment.
	sites.Member("h10-b", me, "admin", time.Time{})
	step("an admin of one site", false)
	sites.Member("h10-b", me, "owner", time.Time{})

	// A site on the beacon's allowlist that nobody holds yet is one the
	// operator set up for somebody else.
	sites.Allow("h10-a", "h10-b", "h10-c")
	step("an allowed site with no member", false)
	sites.Allow("h10-a", "h10-b")
	step("the allowlist naming only owned sites", true)
	sites.Allow()

	// A site whose only membership has ended is still somebody's.
	sites.Member("h10-d", other, "viewer", time.Now().Add(-time.Hour))
	step("a site with only an ended membership", false)
	sites.Drop("h10-d", other)
	step("that site gone", true)

	// An ended membership does not own anything: an admin row of mine
	// that ran out stands for a site I do not own.
	sites.Member("h10-e", me, "admin", time.Now().Add(-time.Hour))
	step("my own ended admin membership", false)
	sites.Drop("h10-e", me)

	step("that membership gone", true)
	if every, err := store.OwnsEverySite(ctx, 0); err != nil || every {
		t.Errorf("account zero (a developer or system principal) owns every site = %v, %v", every, err)
	}

	// And AccessFor carries the answer, for an owner only.
	p := Principal{Kind: PrincipalUser, UserID: me}
	a, err := store.AccessFor(ctx, p, "h10-a")
	if err != nil {
		t.Fatal(err)
	}
	if !a.OwnsEverySite || a.AccessTo(registry[KeyBeaconSites]) != SettingWritable {
		t.Errorf("the sole owner's access: OwnsEverySite = %v, beacon.sites %q", a.OwnsEverySite, a.AccessTo(registry[KeyBeaconSites]))
	}
	sites.Member("h10-b", other, "admin", time.Time{})
	sites.Member("h10-f", other, "owner", time.Time{})
	sites.Member("h10-f", me, "admin", time.Time{})
	a, err = store.AccessFor(ctx, p, "h10-f")
	if err != nil {
		t.Fatal(err)
	}
	if a.OwnsEverySite {
		t.Error("AccessFor marked an admin as owning every site")
	}
	a, err = store.AccessFor(ctx, p, "h10-a")
	if err != nil {
		t.Fatal(err)
	}
	if a.OwnsEverySite || a.AccessTo(registry[KeyBeaconSites]) != SettingShared {
		t.Errorf("an owner of some sites among others: OwnsEverySite = %v, beacon.sites %q", a.OwnsEverySite, a.AccessTo(registry[KeyBeaconSites]))
	}
}
