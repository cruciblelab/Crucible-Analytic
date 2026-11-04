//go:build integration

// W1, through the pages: on a deployment shared by two customers, the
// owner of one site is neither shown the other customer's site nor given
// a page that acts on it.

package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

const (
	w1Own   = "w1-kendi"
	w1Other = "w1-baska"
)

// diagnosticSites is beacon.sites as one reader's diagnostic file
// carries it.
func diagnosticSites(t *testing.T, c *http.Client, base string) []string {
	t.Helper()
	status, body := get(t, c, base+DiagnosticPath)
	if status != http.StatusOK {
		t.Fatalf("the diagnostic file answered %d", status)
	}
	var file diagnosticBundle
	if err := json.Unmarshal([]byte(body), &file); err != nil {
		t.Fatalf("the diagnostic file does not decode: %v", err)
	}
	for _, v := range file.Settings.Values {
		if v.Key != panel.KeyBeaconSites {
			continue
		}
		var out []string
		raw, _ := json.Marshal(v.Value)
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("beacon.sites in the file is not a list: %s", raw)
		}
		return out
	}
	t.Fatalf("the diagnostic file carries no %s", panel.KeyBeaconSites)
	return nil
}

// TestATenantReachesNothingThatActsOnTheOtherCustomer.
//
// Found on the live rig, with two customers on one deployment. The owner
// of one site:
//
//   - downloaded a diagnostic file whose beacon.sites named the other
//     customer's site, and read the same on the settings page;
//   - went through the technical door into the setup wizard, saved the
//     site list with her own site alone (200) - after which the beacon
//     refused the other site's events as unknown (403) - and was drawn
//     the other site's visibility controls;
//   - posted the handover form the wizard no longer draws once accounts
//     exist, received an owner link, and the account it made was owner of
//     the other customer's site, its members page open;
//   - pointed the deployment's mail server at a host of her own (200),
//     which every invitation the other customer sends goes through.
//
// Each is asked here, through the page and then through the store, and
// then the other customer hands her their site: the same requests are
// accepted, so the other customer's site was the reason and nothing else.
func TestATenantReachesNothingThatActsOnTheOtherCustomer(t *testing.T) {
	srv, store := setupSoleTenantServer(t)
	ctx := context.Background()
	restoreGlobal(t, store, panel.KeyBeaconSites)
	clearSiteSettings(t, store, w1Own, w1Other)
	const claimEmail = "w1-ikinci-hesap" + testEmailSuffix
	clearClaims := func() {
		if _, err := store.Pool().Exec(ctx, `DELETE FROM panel_owner_claims WHERE email = $1`, claimEmail); err != nil {
			t.Fatal(err)
		}
	}
	clearClaims()
	t.Cleanup(clearClaims)
	// The mail account is one row for the deployment and the refused
	// save below must not leave one; the mail suites clear it the same way.
	if _, err := store.Pool().Exec(ctx, `DELETE FROM panel_smtp`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = store.Pool().Exec(context.Background(), `DELETE FROM panel_smtp`) })

	owner := makeUser(t, store, "w1-sahip", false)
	other := makeUser(t, store, "w1-oteki", false)
	operator := makeUser(t, store, "w1-isletmeci", true)
	if err := store.AddMember(ctx, w1Own, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, w1Other, other.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	both := []string{w1Own, w1Other}
	if err := store.SetSetting(ctx, panel.KeyBeaconSites, "", both, nil); err != nil {
		t.Fatal(err)
	}
	// beacon.sites is a developer setting: the page draws it only with
	// the mode on, so without this the page half could not leak at all.
	if err := store.SetDeveloperMode(ctx, owner.ID, true); err != nil {
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
	claims := func() int {
		t.Helper()
		var n int
		if err := store.Pool().QueryRow(ctx,
			`SELECT count(*) FROM panel_owner_claims WHERE email = $1`, claimEmail).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	mailRows := func() int {
		t.Helper()
		var n int
		if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM panel_smtp`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	c := signedIn(t, server.URL, owner.Email)
	token := sessionToken(t, c, server.URL)

	// What she is shown.
	if got := diagnosticSites(t, c, server.URL); !slices.Equal(got, []string{w1Own}) {
		t.Errorf("the owner of %s's diagnostic file carries beacon.sites = %v, want [%s]", w1Own, got, w1Own)
	}
	_, settings := get(t, c, server.URL+settingsPath(w1Own))
	row := settingBlock(t, settings, panel.KeyBeaconSites)
	if strings.Contains(settings, w1Other) {
		t.Errorf("the owner of %s's settings page names %s:\n%s", w1Own, w1Other, row)
	}
	if !strings.Contains(row, w1Own) {
		t.Errorf("the owner of %s is not shown her own site on the list:\n%s", w1Own, row)
	}
	if !strings.Contains(row, shown(panel.LockNoticeShared)) {
		t.Errorf("the narrowed list does not say why it has no control:\n%s", row)
	}

	// What she is given: every page that acts on the deployment refuses,
	// GET and POST alike - a page that is not drawn is not a guard.
	for _, path := range []string{TechnicalDoorPath, SetupPathPrefix + "siteler", SetupPathPrefix + "gorunum",
		SetupPathPrefix + "devir", MailPath, DevAccessRequestsPath} {
		if status, _ := get(t, c, server.URL+path); status != http.StatusForbidden {
			t.Errorf("GET %s answered %d to the owner of one site, want %d", path, status, http.StatusForbidden)
		}
	}
	for path, form := range map[string]url.Values{
		TechnicalDoorPath:           {},
		SetupPathPrefix + "siteler": {"siteler": {w1Own}},
		SetupPathPrefix + "gorunum": {"kart:" + w1Other: {}},
		SetupPathPrefix + "devir":   {"eposta": {claimEmail}, "ad": {"İkinci"}},
		MailPath:                    {"islem": {"kaydet"}, "sunucu": {"smtp.w1.example"}, "port": {"587"}, "sifreleme": {"starttls"}, "gonderen": {"panel@w1.example"}, "acik": {"on"}},
		DevAccessRequestsPath:       {},
	} {
		if status, _ := postWithToken(t, c, server.URL+path, token, form); status != http.StatusForbidden {
			t.Errorf("POST %s answered %d to the owner of one site, want %d", path, status, http.StatusForbidden)
		}
	}
	if got := allowed(); !slices.Equal(got, both) {
		t.Errorf("beacon.sites = %v after the owner of one site's forms, want %v", got, both)
	}
	if n := claims(); n != 0 {
		t.Errorf("the owner of one site minted %d owner link(s); each makes an owner of every site", n)
	}
	if n := mailRows(); n != 0 {
		t.Errorf("the owner of one site saved a mail server (%d row)", n)
	}

	// The health page is still hers to read, and the navigation says so;
	// the mail page is not, and the navigation does not offer it.
	status, home := get(t, c, server.URL+HealthPath)
	if status != http.StatusOK {
		t.Errorf("the owner of one site was refused the health page: %d", status)
	}
	if !strings.Contains(home, `href="`+HealthPath+`"`) {
		t.Error("the owner of one site is not offered the health page in the navigation")
	}
	for _, path := range []string{MailPath, DevAccessRequestsPath} {
		if strings.Contains(home, `href="`+path+`"`) {
			t.Errorf("the owner of one site is offered %s, which refuses her", path)
		}
	}
	// Read, not run (W1b): every button on the health page acts on the
	// whole deployment - backups, their check and restore, the range
	// refresh, restarts, upgrades - so none is drawn for her. The store
	// refuses them if posted (TestNoOperationOnTheMachineIsOneCustomersToRun).
	if strings.Contains(home, `name="eylem"`) {
		t.Error("the owner of one site among others is drawn an operation on the health page")
	}

	// The store refuses the same writes, whatever page asks.
	p := panel.Principal{Kind: panel.PrincipalUser, UserID: owner.ID, Label: owner.Email}
	if err := store.RecordSetting(ctx, p, panel.KeyBeaconSites, "", []string{w1Own}); !errors.Is(err, panel.ErrSettingShared) {
		t.Errorf("RecordSetting(beacon.sites) by the owner of one site = %v, want %v", err, panel.ErrSettingShared)
	}
	if err := store.RecordSetting(ctx, p, panel.KeyVisibleCards, w1Other, []string{ViewNone}); !errors.Is(err, panel.ErrSettingNotWritable) {
		t.Errorf("RecordSetting(%s, %s) by the owner of %s = %v, want %v",
			panel.KeyVisibleCards, w1Other, w1Own, err, panel.ErrSettingNotWritable)
	}
	if err := store.RecordSetting(ctx, p, panel.KeyVisibleCards, w1Own, []string{ViewNone}); err != nil {
		t.Errorf("the owner of %s could not set her own site's cards: %v", w1Own, err)
	}
	if got := allowed(); !slices.Equal(got, both) {
		t.Errorf("beacon.sites = %v after the store's refusals, want %v", got, both)
	}
	// Not even the operator writes a setting that needs the developer
	// password through the path that does not ask for it.
	op := panel.Principal{Kind: panel.PrincipalUser, UserID: operator.ID, Label: operator.Email, Superadmin: true}
	before, err := store.GetSetting(ctx, panel.KeyCampaignStoreClickID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RecordSetting(ctx, op, panel.KeyCampaignStoreClickID, "", true); !errors.Is(err, panel.ErrSettingNotWritable) {
		t.Errorf("RecordSetting(%s) by the operator = %v, want %v: the developer password was never asked",
			panel.KeyCampaignStoreClickID, err, panel.ErrSettingNotWritable)
	}
	if after, _ := store.GetSetting(ctx, panel.KeyCampaignStoreClickID, ""); after != before {
		t.Errorf("%s became %v without the developer password", panel.KeyCampaignStoreClickID, after)
	}

	// The operator reads the whole list.
	oc := signedIn(t, server.URL, operator.Email)
	if got := diagnosticSites(t, oc, server.URL); !slices.Equal(got, both) {
		t.Errorf("the operator's diagnostic file carries beacon.sites = %v, want %v", got, both)
	}

	// The other customer hands her their site. Every site is now hers,
	// and what was refused is hers to do.
	if err := store.AddMember(ctx, w1Other, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	if got := diagnosticSites(t, c, server.URL); !slices.Equal(got, both) {
		t.Errorf("the owner of every site's diagnostic file carries beacon.sites = %v, want %v", got, both)
	}
	for _, path := range []string{TechnicalDoorPath, MailPath, DevAccessRequestsPath} {
		if status, _ := get(t, c, server.URL+path); status != http.StatusOK {
			t.Errorf("GET %s answered %d to the owner of every site, want 200", path, status)
		}
	}
	if _, page := get(t, c, server.URL+HealthPath); !strings.Contains(page, `name="eylem"`) {
		t.Error("the owner of every site is drawn no operation on the health page")
	}
	if err := store.RecordSetting(ctx, p, panel.KeyBeaconSites, "", both); err != nil {
		t.Errorf("RecordSetting(beacon.sites) by the owner of every site: %v", err)
	}
}

// TestARestoreReportIsShownOnlyToWhoeverMayRunOne (W1b).
//
// A restore's report counts every table's rows, and on a deployment
// shared by two customers those are both customers' rows: the owner of
// one site, who knows her own volume, reads the other's off the
// difference. The report is drawn for whoever may run the work, which is
// whoever speaks for the deployment.
//
// A finished restore with a report is seeded, and the health page is read
// by the owner of one site among others and then, once she owns every
// site, by the same person.
func TestARestoreReportIsShownOnlyToWhoeverMayRunOne(t *testing.T) {
	// BackupQueueLock before setupSoleTenantServer's AccountsLock: the
	// order testdb declares and backupPage takes them in.
	testdb.Lock(t, testdb.Pool(t, testdb.SchemaAdmin), testdb.BackupQueueLock)
	sweep := func() {
		if _, err := testdb.Pool(t, testdb.Panel).Exec(context.Background(),
			`DELETE FROM panel_backup_requests`); err != nil {
			t.Errorf("sweeping the backup queue: %v", err)
		}
	}
	sweep()
	t.Cleanup(sweep)

	srv, store := setupSoleTenantServer(t)
	ctx := context.Background()
	owner := makeUser(t, store, "w1b-sahip", false)
	other := makeUser(t, store, "w1b-oteki", false)
	if err := store.AddMember(ctx, w1Own, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, w1Other, other.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}

	const report = "beacon_events 424242"
	if _, err := testdb.Admin(t).Exec(ctx, `
		INSERT INTO panel_backup_requests
		  (actor_kind, actor_label, operation_id, kind, sets, target_id, state, finished_at, result)
		VALUES ('developer', 'Geliştirici', '', 'geri_yukle', '{}', 1, 'succeeded', now(), $1)`,
		"veritabanı: yan\n1 satır, 1 tablo: "+report); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	c := signedIn(t, server.URL, owner.Email)

	status, page := get(t, c, server.URL+HealthPath)
	if status != http.StatusOK {
		t.Fatalf("the owner of one site was refused the health page: %d", status)
	}
	if strings.Contains(page, report) {
		t.Error("the owner of one site among others reads a restore report that counts every customer's rows")
	}

	if err := store.AddMember(ctx, w1Other, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	if _, page := get(t, c, server.URL+HealthPath); !strings.Contains(page, report) {
		t.Error("the owner of every site is not shown the restore report; the seeded row did not reach the page")
	}
}
