//go:build integration

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/configreport"
	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// What the services report, on the wizard's steps (PLAN §W4, items 5 and
// 6), against real heartbeat rows.
//
// Measured on the live rig before: the "Yapılandırma dosyaları" step said
// "panele bildirilmemiş" beside 10 of 13 rows and knew 0; with the panel's
// site list removed the beacon kept accepting both sites in its file, the
// two site steps said there were none, and the handover form was open -
// the account it would have made owns every site on the panel's list,
// which was empty.
func TestTheWizardShowsWhatTheServicesReport(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	admin := testdb.Admin(t)
	// After AccountsLock, which setupTestServer holds: the declared order.
	testdb.Lock(t, admin, testdb.HeartbeatLock)
	lang := srv.Renderer.Catalogs().Base()

	// No site list in the panel, put back afterwards (empty, as every
	// suite leaves it).
	restoreGlobal(t, store, panel.KeyBeaconSites)
	if _, err := admin.Exec(ctx, `DELETE FROM panel_settings WHERE key = $1 AND site_id = ''`,
		string(panel.KeyBeaconSites)); err != nil {
		t.Fatal(err)
	}

	// Rows under instances of their roles, so nothing another suite
	// writes is overwritten; removed afterwards, as they were never there.
	collectorA := heartbeat.RowKey(testdb.Collector, "w4d-a")
	collectorB := heartbeat.RowKey(testdb.Collector, "w4d-b")
	beaconRow := heartbeat.RowKey(testdb.Beacon, "w4d")
	report := func(key string, r configreport.Report) {
		t.Helper()
		body, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, `
			INSERT INTO service_heartbeat (service, version, started_at, beat_at, config)
			VALUES ($1, 'w4d', now() - interval '1 minute', now(), $2::jsonb)
			ON CONFLICT (service) DO UPDATE SET beat_at = now(), config = EXCLUDED.config`,
			key, string(body)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DELETE FROM service_heartbeat WHERE service = ANY($1)`,
			[]string{collectorA, collectorB, beaconRow})
		_, _ = admin.Exec(context.Background(), `DELETE FROM panel_owner_claims WHERE email = $1`,
			"sahip@w4d.invalid")
	})

	report(collectorA, configreport.Report{Program: configreport.Collector, Values: map[string]string{
		"site_id": "w4d-a", "network.listen_addr": "10.9.8.7:443", "logging.dir": "",
		// What a careless or older writer could put in the row. The
		// writer never reads a secret key; the page must not draw one
		// either.
		"storage.timescale_dsn": "postgres://collector:hunter2@db/analytics",
	}})
	report(collectorB, configreport.Report{Program: configreport.Collector, Values: map[string]string{
		"site_id": "w4d-b", "network.listen_addr": "10.9.8.8:443",
	}})

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	dev := newClient(t, server.URL)
	noAccountYet(t, store)
	token, _, err := store.RequestDevAccess(ctx, testReasonPrefix+"w4d", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := dev.Get(server.URL + DevAccessPathPrefix + token)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	// ---- item 5: the config step draws what each process said ----
	_, body := get(t, dev, server.URL+SetupPathPrefix+"toplama")
	for _, want := range []string{collectorA, collectorB, "10.9.8.7:443", "10.9.8.8:443",
		lang.T("kurulum.adim.toplama.bos")} {
		if !strings.Contains(body, want) {
			t.Errorf("the config step does not show %q", want)
		}
	}
	if strings.Contains(body, "hunter2") {
		t.Error("the config step drew a secret a report carried")
	}

	// ---- item 6: no beacon has said anything ----
	silent := lang.T("kurulum.beacon_siteleri.sessiz")
	if _, body = get(t, dev, server.URL+SetupPathPrefix+"siteler"); !strings.Contains(body, silent) {
		t.Errorf("with no beacon report the sites step does not say so")
	}

	// ---- the beacon accepts its file's sites; the panel has none ----
	report(beaconRow, configreport.Report{Program: configreport.Beacon, Values: map[string]string{
		configreport.KeyAcceptedSites: "w4d-dosya-a, w4d-dosya-b",
		configreport.KeySitesSource:   configreport.SourceFile,
	}})
	fromFile := lang.Tf("kurulum.beacon_siteleri.dosyadan", beaconRow, "w4d-dosya-a, w4d-dosya-b")
	for _, step := range []string{"siteler", "gorunum"} {
		_, body = get(t, dev, server.URL+SetupPathPrefix+step)
		if !strings.Contains(body, htmlEscape(fromFile)) {
			t.Errorf("the %s step does not say which sites the beacon is accepting from its file", step)
		}
		if strings.Contains(body, silent) {
			t.Errorf("the %s step says no beacon reported, and one did", step)
		}
	}

	// ---- and the handover is closed, on the page and to a POST ----
	handover := server.URL + SetupPathPrefix + "devir"
	reason := lang.T("kurulum.devir.site_yok")
	_, body = get(t, dev, handover)
	if !strings.Contains(body, reason) {
		t.Errorf("the handover step does not name the missing site list")
	}
	if strings.Contains(body, `name="eposta"`) {
		t.Error("the handover form is drawn with no site list saved")
	}
	status, body := postWithToken(t, dev, handover, csrfFrom(t, body), url.Values{
		"eposta": {"sahip@w4d.invalid"}, "ad": {"W4d"},
	})
	if status == http.StatusOK && claimLinkPattern.MatchString(body) {
		t.Error("a POST minted an invitation with no site list saved; the account would own nothing")
	}
	if !strings.Contains(body, reason) {
		t.Errorf("the refused POST does not name the missing site list: %q", messageOf(body))
	}
	var claims int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM panel_owner_claims WHERE email = $1`,
		"sahip@w4d.invalid").Scan(&claims); err != nil || claims != 0 {
		t.Errorf("owner claims for the address = %d (%v), want 0", claims, err)
	}

	// ---- the same list is saved; the beacon has not read it yet ----
	//
	// The same two sites as its file, so the lists agree and only the
	// source tells this beacon apart from one that has read the panel's.
	// And the sentence is not "the panel has no list", which the first
	// version said here.
	if err := store.SetSetting(ctx, panel.KeyBeaconSites, "", []string{"w4d-dosya-a", "w4d-dosya-b"}, nil); err != nil {
		t.Fatal(err)
	}
	notYet := lang.Tf("kurulum.beacon_siteleri.henuz", beaconRow, "w4d-dosya-a, w4d-dosya-b")
	_, body = get(t, dev, server.URL+SetupPathPrefix+"siteler")
	if !strings.Contains(body, htmlEscape(notYet)) {
		t.Error("a beacon still on its file is not named once the panel has the same list")
	}
	if strings.Contains(body, htmlEscape(fromFile)) {
		t.Error("the sites step says the panel has no list, and it has one")
	}
	_, body = get(t, dev, handover)
	if strings.Contains(body, reason) {
		t.Error("the handover still names the site list after one was saved")
	}

	// ---- it reads it, and takes all of it: nothing to say ----
	report(beaconRow, configreport.Report{Program: configreport.Beacon, Values: map[string]string{
		configreport.KeyAcceptedSites: "w4d-dosya-a, w4d-dosya-b",
		configreport.KeySitesSource:   configreport.SourcePanel,
	}})
	_, body = get(t, dev, server.URL+SetupPathPrefix+"siteler")
	if strings.Contains(body, beaconRow) {
		t.Error("the sites step talks about a beacon that accepts exactly the saved list")
	}

	// ---- it reads it and cannot take all of it: said ----
	report(beaconRow, configreport.Report{Program: configreport.Beacon, Values: map[string]string{
		configreport.KeyAcceptedSites: "",
		configreport.KeySitesSource:   configreport.SourcePanel,
	}})
	differs := lang.Tf("kurulum.beacon_siteleri.farkli", beaconRow, lang.T("kurulum.beacon_siteleri.hicbiri"))
	if _, body = get(t, dev, server.URL+SetupPathPrefix+"siteler"); !strings.Contains(body, htmlEscape(differs)) {
		t.Error("a beacon accepting something other than the saved list is not named")
	}
}

// htmlEscape is what html/template makes of a message with an apostrophe
// in it - beacon.toml'daki.
func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "'", "&#39;", "<", "&lt;", ">", "&gt;", `"`, "&#34;").Replace(s)
}
