//go:build integration

// Pausing a site's recording (PLAN §4, #3) through the pages a person
// uses: the settings page's own control, the dashboard's banner, and the
// one sentence that names a writer which would not honour the pause.

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

const pauseKey = string(panel.KeyCollectionPausedUntil)

// settingSectionOf is the settings page's section for one key.
func settingSectionOf(t *testing.T, body, key string) string {
	t.Helper()
	for _, part := range strings.Split(body, `<section class="ayar">`)[1:] {
		if end := strings.Index(part, "</section>"); end >= 0 {
			part = part[:end]
		}
		if strings.Contains(part, `name="anahtar" value="`+key+`"`) {
			return part
		}
	}
	t.Fatalf("the settings page has no section for %s", key)
	return ""
}

// clearPauseHistory removes a test site's audit rows, at both ends.
//
// The dashboard reads past pauses back from the audit log, so a run that
// left its entries behind would hand the next run a history it never
// wrote - and panel_user cannot delete audit rows, correctly, so this goes
// through the schema's owner.
func clearPauseHistory(t *testing.T, site string) {
	t.Helper()
	admin := testdb.Admin(t)
	clear := func() {
		if _, err := admin.Exec(context.Background(),
			`DELETE FROM panel_audit_log WHERE site_id = $1`, site); err != nil {
			t.Logf("clearing the audit rows of %s: %v", site, err)
		}
	}
	clear()
	t.Cleanup(clear)
}

func pausedUntil(t *testing.T, store *panel.Store, site string) string {
	t.Helper()
	value, err := store.GetSetting(context.Background(), panel.KeyCollectionPausedUntil, site)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := value.(string)
	return text
}

// TestTheOwnerPausesAndResumesRecording is the whole control: the owner
// picks six hours on the settings page, the value stored is six hours
// from now, the dashboard says so, a duration the page did not offer is
// refused, and "off" resumes.
func TestTheOwnerPausesAndResumesRecording(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "duraklat-testi"
	clearSiteSettings(t, store, site)
	clearPauseHistory(t, site)
	owner := makeUser(t, store, "duraklat-sahip", false)
	if err := store.AddMember(ctx, site, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := signedIn(t, server.URL, owner.Email)
	page := server.URL + settingsPath(site)
	base := srv.Renderer.Catalogs().Base()

	_, body := get(t, client, page)
	section := settingSectionOf(t, body, pauseKey)
	if !strings.Contains(section, shown(base.T("ayarlar.sure.kapali_durum"))) {
		t.Errorf("before any pause the row does not say it is off:\n%s", section)
	}
	for _, minutes := range []string{"0", "60", "360", "1440", "10080"} {
		if !strings.Contains(section, `<option value="`+minutes+`">`) {
			t.Errorf("the control does not offer %s minutes:\n%s", minutes, section)
		}
	}
	if !strings.Contains(section, `<option value="0">`+shown(base.T("ayarlar.sure.kapat"))+`</option>`) {
		t.Errorf("while off the zero choice does not read as off:\n%s", section)
	}

	before := time.Now()
	status, body := post(t, client, page, url.Values{
		"anahtar": {pauseKey}, "islem": {"kaydet"}, "deger": {"360"},
	})
	if status != http.StatusOK {
		t.Fatalf("pausing answered %d: %q", status, noticeOf(body))
	}
	stored := pausedUntil(t, store, site)
	end, err := time.Parse(time.RFC3339, stored)
	if err != nil {
		t.Fatalf("stored %q, not a moment", stored)
	}
	if lo, hi := before.Add(6*time.Hour-time.Second), time.Now().Add(6*time.Hour+time.Second); end.Before(lo) || end.After(hi) {
		t.Errorf("stored %s; six hours from the press is between %s and %s", end, lo, hi)
	}
	section = settingSectionOf(t, body, pauseKey)
	if !strings.Contains(section, "saatine kadar açık") {
		t.Errorf("after pausing the row does not say until when")
	}
	// While on, the zero choice is the action, not a state beside "on".
	if !strings.Contains(section, `<option value="0">`+shown(base.T("ayarlar.sure.simdi_kapat"))+`</option>`) {
		t.Errorf("while paused the zero choice does not read as switching off now:\n%s", section)
	}

	_, dash := get(t, client, server.URL+sitePath(site))
	if !strings.Contains(dash, "Bu sitenin kaydı") || !strings.Contains(dash, "duraklatıldı") {
		t.Error("the dashboard does not say recording is paused")
	}

	// A number the page did not draw is refused, and changes nothing.
	status, body = post(t, client, page, url.Values{
		"anahtar": {pauseKey}, "islem": {"kaydet"}, "deger": {"7"},
	})
	if status != http.StatusBadRequest || !strings.Contains(body, shown(base.T("ayarlar.hata.sure"))) {
		t.Errorf("an unoffered duration answered %d: %q", status, noticeOf(body))
	}
	if pausedUntil(t, store, site) != stored {
		t.Error("a refused duration changed the stored end")
	}

	status, _ = post(t, client, page, url.Values{
		"anahtar": {pauseKey}, "islem": {"kaydet"}, "deger": {"0"},
	})
	if status != http.StatusOK || pausedUntil(t, store, site) != "" {
		t.Errorf("resuming answered %d and left %q stored", status, pausedUntil(t, store, site))
	}
	if _, dash = get(t, client, server.URL+sitePath(site)); strings.Contains(dash, "duraklatıldı") {
		t.Error("the dashboard still says paused after resuming")
	}
}

// TestTheDashboardNamesAWriterThatWouldGoOnRecording: a fresh heartbeat
// that reports rows written and no pause counter is a build from before
// the pause, and the banner says it goes on recording; once the writer
// reports the counter, the sentence goes.
//
// Beside it, the two rows the rule must pass over, each the only thing
// that shows its half of the rule: a writer from before the pause that
// has stopped (its row stays, and nothing it would do is in force), and
// a running service that writes no rows at all.
func TestTheDashboardNamesAWriterThatWouldGoOnRecording(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "duraklat-eski"
	clearSiteSettings(t, store, site)
	clearPauseHistory(t, site)
	owner := makeUser(t, store, "duraklat-eski-sahip", false)
	if err := store.AddMember(ctx, site, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSetting(ctx, panel.KeyCollectionPausedUntil, site,
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339), nil); err != nil {
		t.Fatal(err)
	}
	admin := testdb.Admin(t)
	// After setupTestServer's AccountsLock, the order every suite holding
	// both takes them in; before the cleanup, so the rows go while held.
	testdb.Lock(t, admin, testdb.HeartbeatLock)
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(),
			`DELETE FROM service_heartbeat WHERE version LIKE 'duraklat-%'`); err != nil {
			t.Logf("clearing the test heartbeat rows: %v", err)
		}
	})
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := signedIn(t, server.URL, owner.Email)
	base := srv.Renderer.Catalogs().Base()
	collector := base.T("saglik.servis.collector")
	beacon := base.T("saglik.servis.beacon_writer")

	api := base.T("saglik.servis.analytics_reader")
	// The sentence the banner adds, and whether it names a service.
	names := func(dash, service string) bool {
		at := strings.Index(dash, "Dikkat: ")
		if at < 0 {
			return false
		}
		end := strings.Index(dash[at:], "</")
		return end > 0 && strings.Contains(dash[at:at+end], shown(service))
	}

	started := time.Now().Add(-time.Minute)
	writeBeatDetail(t, store, testdb.Collector, "duraklat-eski-1", started,
		map[string]int64{heartbeat.CounterWritten: 4}, nil, "", heartbeat.TokenKeyUnknown)
	writeBeatDetail(t, store, testdb.Beacon, "duraklat-eski-2", started.Add(-time.Hour),
		map[string]int64{heartbeat.CounterWritten: 4}, nil, "", heartbeat.TokenKeyUnknown)
	if _, err := admin.Exec(ctx, `UPDATE service_heartbeat SET beat_at = now() - interval '10 minutes'
		WHERE version = 'duraklat-eski-2'`); err != nil {
		t.Fatal(err)
	}
	writeBeatDetail(t, store, testdb.Reader, "duraklat-api-1", started,
		map[string]int64{"istek": 4}, nil, "", heartbeat.TokenKeyUnknown)
	_, dash := get(t, client, server.URL+sitePath(site))
	if !names(dash, collector) {
		t.Errorf("the banner does not name the collector, which reports no pause counter")
	}
	if names(dash, beacon) {
		t.Error("the banner names the beacon, whose last beat is ten minutes old: it is not running")
	}
	if names(dash, api) {
		t.Error("the banner names the API, which writes no rows")
	}

	writeBeatDetail(t, store, testdb.Collector, "duraklat-yeni-1", started,
		map[string]int64{heartbeat.CounterWritten: 4, heartbeat.CounterPaused: 0}, nil, "", heartbeat.TokenKeyUnknown)
	writeBeatDetail(t, store, testdb.Beacon, "duraklat-yeni-2", started,
		map[string]int64{heartbeat.CounterWritten: 4, heartbeat.CounterPaused: 0}, nil, "", heartbeat.TokenKeyUnknown)
	_, dash = get(t, client, server.URL+sitePath(site))
	if !strings.Contains(dash, "duraklatıldı") || strings.Contains(dash, "Dikkat:") {
		t.Error("with both writers honouring the pause the banner should say paused and name nobody")
	}
}

// TestTheDashboardSaysWhichStretchesWentUnrecorded: once a pause has
// ended the banner goes, and the chosen range says which of its hours
// were not recorded - read back from the entries the settings page itself
// wrote, moved only in time. A pause still running is the banner's, not
// this notice's, and a range that does not reach the stretch says nothing.
func TestTheDashboardSaysWhichStretchesWentUnrecorded(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "duraklat-gecmis"
	clearSiteSettings(t, store, site)
	clearPauseHistory(t, site)
	admin := testdb.Admin(t)
	owner := makeUser(t, store, "duraklat-gecmis-sahip", false)
	if err := store.AddMember(ctx, site, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := signedIn(t, server.URL, owner.Email)
	page, dash := server.URL+settingsPath(site), server.URL+sitePath(site)
	base := srv.Renderer.Catalogs().Base()
	pastLead := shown(strings.SplitN(base.T("pano.duraklatilmisti"), "%s", 2)[0])
	banner := "saatine kadar duraklatıldı"

	pause := func(minutes string) {
		t.Helper()
		if status, body := post(t, client, page, url.Values{
			"anahtar": {pauseKey}, "islem": {"kaydet"}, "deger": {minutes},
		}); status != http.StatusOK {
			t.Fatalf("posting %s minutes answered %d: %q", minutes, status, noticeOf(body))
		}
	}

	pause("360")
	if _, body := get(t, client, dash); !strings.Contains(body, banner) || strings.Contains(body, pastLead) {
		t.Errorf("a running pause: banner %v, past notice %v; want the banner alone",
			strings.Contains(body, banner), strings.Contains(body, pastLead))
	}

	// Lifted, and both entries moved to yesterday, ten to noon, in the
	// zone the page draws in.
	pause("0")
	zone := srv.zone(ctx)
	y := time.Now().In(zone).AddDate(0, 0, -1)
	from := time.Date(y.Year(), y.Month(), y.Day(), 10, 0, 0, 0, zone)
	until := from.Add(2 * time.Hour)
	for nth, when := range []time.Time{from, until} {
		tag, err := admin.Exec(ctx, `UPDATE panel_audit_log SET time = $1 WHERE id =
			(SELECT id FROM panel_audit_log WHERE site_id = $2 AND target = $3 ORDER BY id OFFSET $4 LIMIT 1)`,
			when, site, pauseKey, nth)
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("moving entry %d: %v (%d rows)", nth, err, tag.RowsAffected())
		}
	}
	f := ui.NewFormatter(base, zone)
	stretch := shown(base.Tf("pano.duraklatma_araligi", f.DateTime(from), f.DateTime(until)))
	_, body := get(t, client, dash)
	if !strings.Contains(body, pastLead) || !strings.Contains(body, stretch) {
		t.Errorf("the week's page does not name the stretch %q", stretch)
	}
	if strings.Contains(body, banner) {
		t.Error("the banner is still there after the pause was lifted")
	}

	if _, body := get(t, client, dash+"?gun=1"); strings.Contains(body, pastLead) {
		t.Error("today's page names a stretch from yesterday")
	}
}
