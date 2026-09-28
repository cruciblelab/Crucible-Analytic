//go:build integration

package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/preflight"
	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// TestTheDiagnosticFileCarriesWhatThePageShows: a real heartbeat row,
// read back through the download a customer actually clicks.
//
// The service rows are compared with the table itself, read under the
// same lock, rather than with a list written here: a fixture that named
// the rows would agree with the file whatever the file did.
func TestTheDiagnosticFileCarriesWhatThePageShows(t *testing.T) {
	srv, store := setupTestServer(t)
	server, client, store := healthServerOn(t, srv, store)
	writeBeat(t, store, "saglik-tani", time.Now().Add(-time.Hour),
		map[string]int64{heartbeat.CounterWritten: 7, heartbeat.CounterDropped: 2},
		errors.New("tanı testinin hatası"))

	resp, err := client.Get(server.URL + DiagnosticPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the owner got %d from the diagnostic file, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="crucible-tani-`) {
		t.Errorf("Content-Disposition = %q; the browser should save the file, not show it", cd)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}

	var got diagnosticBundle
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("the file is not JSON: %v", err)
	}
	if got.Format != diagnosticFormat || got.PanelVersion == "" {
		t.Errorf("format %q, panel version %q", got.Format, got.PanelVersion)
	}
	if got.Schema.ExpectedVersion != schemaver.Version || got.Schema.Error != "" {
		t.Errorf("schema section = %+v", got.Schema)
	}
	if !got.Schema.Recorded || !got.Schema.Matches {
		t.Errorf("an installed test database reads as recorded=%v matches=%v", got.Schema.Recorded, got.Schema.Matches)
	}

	// The rows, against the table.
	beats, err := heartbeat.Read(context.Background(), store.Pool())
	if err != nil {
		t.Fatal(err)
	}
	var want, have []string
	for _, b := range beats {
		want = append(want, b.Service+"/"+b.Version)
	}
	var ours *diagnosticService
	for i, row := range got.Services.Rows {
		have = append(have, row.Service+"/"+row.Version)
		if row.Service == testdb.Collector && row.Version == "saglik-tani" {
			ours = &got.Services.Rows[i]
		}
	}
	sort.Strings(want)
	sort.Strings(have)
	if strings.Join(want, " ") != strings.Join(have, " ") {
		t.Errorf("the file's services are %v and the table holds %v", have, want)
	}
	if ours == nil {
		t.Fatalf("the row this test wrote is not in the file: %v", have)
	}
	if ours.Counters[heartbeat.CounterWritten] != 7 || ours.Counters[heartbeat.CounterDropped] != 2 {
		t.Errorf("counters = %v, want written 7 and dropped 2", ours.Counters)
	}
	if !strings.Contains(ours.LastError, "tanı testinin hatası") || ours.LastErrorAt.IsZero() {
		t.Errorf("last error = %q at %v; the page shows it, so the file must", ours.LastError, ours.LastErrorAt)
	}
	if ours.AgeSeconds < 0 || ours.AgeSeconds > 600 {
		t.Errorf("age = %d s for a row written a moment ago", ours.AgeSeconds)
	}

	// Every check, the passed ones included, against the runner the page
	// uses: a file that dropped the passes would read as a list of
	// problems with nothing to say what is fine.
	var ran, filed []string
	passed := 0
	for _, c := range srv.runHealthChecks(context.Background()) {
		ran = append(ran, c.ID)
	}
	for _, c := range got.Checks.Results {
		filed = append(filed, c.ID)
		if c.Status == preflight.CheckPass {
			passed++
		}
	}
	sort.Strings(ran)
	sort.Strings(filed)
	if len(ran) == 0 || strings.Join(ran, " ") != strings.Join(filed, " ") {
		t.Errorf("the file's checks are %v and the page's runner gives %v", filed, ran)
	}
	if passed == 0 {
		t.Error("no passed check in the file; on an installed database some always pass")
	}

	if got.Storage.Error != "" || len(got.Storage.Tables) == 0 {
		t.Errorf("storage section = %+v", got.Storage)
	}

	// Deployment-wide settings, with where each value came from, and the
	// three display-only site settings left out as the file says.
	keys := map[panel.Key]diagnosticSetting{}
	for _, v := range got.Settings.Values {
		keys[v.Key] = v
	}
	if got.Settings.Error != "" {
		t.Errorf("settings error: %s", got.Settings.Error)
	}
	if v, ok := keys[panel.KeyBeaconSites]; !ok || v.Source == "" {
		t.Errorf("the file has no %s with a source: %+v", panel.KeyBeaconSites, v)
	}
	if _, ok := keys[panel.KeySiteName]; ok {
		t.Errorf("%s is a per-site display setting and the file says it leaves those out", panel.KeySiteName)
	}
	if len(got.Omitted) != 3 {
		t.Errorf("omitted = %q; the file says what it does not carry", got.Omitted)
	}
}

// TestWhoMayDownloadTheDiagnosticFile: whoever may read the Health
// page, and nobody else - the download is not a way around the page.
func TestWhoMayDownloadTheDiagnosticFile(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()

	owner := makeUser(t, store, "tani-yetki-sahip", false)
	if err := store.AddMember(ctx, healthSite, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	admin := makeUser(t, store, "tani-yetki-yonetici", false)
	if err := store.AddMember(ctx, healthSite, admin.ID, panel.RoleAdmin, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	ownerClient := signedIn(t, server.URL, owner.Email)
	if status, _ := get(t, ownerClient, server.URL+DiagnosticPath); status != http.StatusOK {
		t.Errorf("the owner got %d, want 200", status)
	}
	// A read, and only a read: any other method is refused rather than
	// answered with the file.
	posted, err := ownerClient.Post(server.URL+DiagnosticPath, "application/x-www-form-urlencoded", nil)
	if err != nil {
		t.Fatal(err)
	}
	posted.Body.Close()
	if posted.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("a POST got %d, want 405", posted.StatusCode)
	}
	if status, _ := get(t, signedIn(t, server.URL, admin.Email), server.URL+DiagnosticPath); status != http.StatusForbidden {
		t.Errorf("an admin got %d, want 403 - the Health page refuses them too", status)
	}

	anonymous := newClient(t, server.URL)
	resp, err := anonymous.Get(server.URL + DiagnosticPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), LoginPath) {
		t.Errorf("no session got %d to %q, want a redirect to the sign-in form",
			resp.StatusCode, resp.Header.Get("Location"))
	}

	// A developer session, obtained the only way one can be.
	liveToken, liveReq := requestAccess(t, store, "tani-yetki")
	if err := store.ApproveDevAccess(ctx, liveReq.ID, owner); err != nil {
		t.Fatal(err)
	}
	dev := newClient(t, server.URL)
	if status, _ := get(t, dev, server.URL+DevAccessPathPrefix+liveToken); status != http.StatusSeeOther {
		t.Fatalf("redeeming the developer link answered %d", status)
	}
	if status, _ := get(t, dev, server.URL+DiagnosticPath); status != http.StatusOK {
		t.Errorf("a developer got %d, want 200 - they are who the file is for", status)
	}
}

// TestEachSectionOfTheDiagnosticFileFallsAlone: with the database gone,
// the four sections that read it say so, each in its own words, and the
// file still arrives - which is when it is wanted most.
func TestEachSectionOfTheDiagnosticFileFallsAlone(t *testing.T) {
	srv, _ := setupTestServer(t)
	closed, err := panel.NewStore(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	working := srv.Store
	srv.Store = closed
	defer func() { srv.Store = working }()

	lang := srv.language(httptest.NewRequest(http.MethodGet, DiagnosticPath, nil))
	got := srv.buildDiagnostic(context.Background(), lang, time.Now())

	for name, section := range map[string]string{
		"schema": got.Schema.Error, "services": got.Services.Error,
		"storage": got.Storage.Error, "settings": got.Settings.Error,
	} {
		if section == "" {
			t.Errorf("the %s section reports no error with its database closed", name)
		}
	}
	if got.Services.Rows == nil || got.Storage.Tables == nil || got.Settings.Values == nil {
		t.Error("a failed section encodes its list as null; an empty list and a missing one read differently")
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("a file with failed sections does not encode: %v", err)
	}
	if !strings.Contains(string(body), `"format":"`+diagnosticFormat+`"`) || len(got.Omitted) != 3 {
		t.Errorf("the sections that need no database did not arrive: %s", body)
	}
}
