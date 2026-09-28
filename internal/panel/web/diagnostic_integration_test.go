//go:build integration

package web

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/logsink"
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
// the five sections that read it say so, each in its own words, and the
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
	operator := panel.Principal{Kind: panel.PrincipalDeveloper, Label: panel.DeveloperLabel, Superadmin: true}
	got := srv.buildDiagnostic(context.Background(), lang, operator, time.Now())

	for name, section := range map[string]string{
		"schema": got.Schema.Error, "services": got.Services.Error,
		"storage": got.Storage.Error, "settings": got.Settings.Error,
		"logs": got.Logs.Error,
	} {
		if section == "" {
			t.Errorf("the %s section reports no error with its database closed", name)
		}
	}
	if got.Services.Rows == nil || got.Storage.Tables == nil || got.Settings.Values == nil ||
		got.Logs.Lines == nil {
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

// TestTheDiagnosticFileCarriesTheLogLinesAsDecided: lines written the way
// the services write them - through the real sink, one of them a real
// authentication attempt's attributes - downloaded by an owner and by a
// developer.
//
// The raw file is searched as well as the parsed one: an address or a
// claim that reached the bytes by any route, in any field, is the
// failure, whatever the struct says.
func TestTheDiagnosticFileCarriesTheLogLinesAsDecided(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const mine, theirs = "tani-gunluk-benim", "tani-gunluk-onlarin"
	const prefix = "tanı-günlük-testi:"
	owner := makeUser(t, store, "tani-gunluk-sahip", false)
	other := makeUser(t, store, "tani-gunluk-oteki", false)
	if err := store.AddMember(ctx, mine, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, theirs, other.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	// An administrator there, which is not an owner: the file is for
	// owners, and it carries the sites its reader owns.
	if err := store.AddMember(ctx, theirs, owner.ID, panel.RoleAdmin, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	admin := testdb.Admin(t)
	clear := func() {
		if _, err := admin.Exec(ctx, `DELETE FROM panel_logs WHERE message LIKE $1`, prefix+"%"); err != nil {
			t.Errorf("clearing log lines: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)

	sink := logsink.New(store.Pool(), logsink.Config{Level: slog.LevelDebug})
	logger := slog.New(sink.Handler())
	logger.Warn(prefix+" giriş reddedildi", logging.Attempt(
		"saldirgan@example.com", logging.VerdictRejected, "wrong password from 203.0.113.9", "203.0.113.9")...)
	logger.Error(prefix+" benim sitemin satırı", logsink.SiteKey, mine, "err", "timeout talking to 198.51.100.23")
	logger.Error(prefix+" onların satırı", logsink.SiteKey, theirs, "err", "x")
	// The shape of the panel's own mail warning, which names the
	// recipient: the provider may go, the person may not.
	logger.Warn(prefix+" posta gönderilemedi", "to", "ali.veli@example.com", "stage", "rcpt")
	logger.Info(prefix + " bilgi satırı")
	sink.Close() // drains the buffer
	if written, _, failed := sink.Counters(); written != 5 || failed != 0 {
		t.Fatalf("the sink wrote %d lines and failed %d; the test's rows are not all there", written, failed)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	download := func(c *http.Client) (diagnosticBundle, map[string]diagnosticLogLine) {
		t.Helper()
		status, body := get(t, c, server.URL+DiagnosticPath)
		if status != http.StatusOK {
			t.Fatalf("download answered %d", status)
		}
		for _, leaked := range []string{"203.0.113.9", "198.51.100.23", "saldirgan", "ali.veli"} {
			if strings.Contains(body, leaked) {
				t.Errorf("the file carries %q", leaked)
			}
		}
		var b diagnosticBundle
		if err := json.Unmarshal([]byte(body), &b); err != nil {
			t.Fatal(err)
		}
		ours := map[string]diagnosticLogLine{}
		for _, l := range b.Logs.Lines {
			if strings.HasPrefix(l.Message, prefix) {
				ours[strings.TrimPrefix(l.Message, prefix+" ")] = l
			}
		}
		return b, ours
	}

	b, ours := download(signedIn(t, server.URL, owner.Email))
	if b.Logs.Error != "" || b.Logs.Scope != "owned" || b.Logs.Limit != diagnosticLogLimit {
		t.Errorf("the owner's log section: scope %q, limit %d, error %q", b.Logs.Scope, b.Logs.Limit, b.Logs.Error)
	}
	attempt, ok := ours["giriş reddedildi"]
	if !ok {
		t.Fatalf("the authentication line is missing from the owner's file: %v", ours)
	}
	if attempt.Attrs[logging.KeyPeer] != "203.0.113.0/24" || attempt.Category != string(logging.CategoryAuth) ||
		attempt.Attrs[logging.KeyVerdict] != logging.VerdictRejected {
		t.Errorf("attempt line = %+v", attempt)
	}
	if _, sent := attempt.Attrs[logging.KeyClaim]; sent || !slices.Contains(attempt.Withheld, logging.KeyClaim) {
		t.Errorf("the claim should be withheld and named as withheld: %+v", attempt)
	}
	if line, ok := ours["benim sitemin satırı"]; !ok || line.Site != mine ||
		line.Attrs["err"] != "timeout talking to 198.51.100.0/24" {
		t.Errorf("the owner's own site line = %+v (present %v)", line, ok)
	}
	if _, ok := ours["onların satırı"]; ok {
		t.Error("an owner's file carries a line about a site they administer and do not own")
	}
	if mail := ours["posta gönderilemedi"]; mail.Attrs["to"] != "…@example.com" {
		t.Errorf("the mail warning's recipient = %q, want the domain alone", mail.Attrs["to"])
	}
	if _, ok := ours["bilgi satırı"]; ok {
		t.Error("an INFO line is in the file; it carries warnings and errors")
	}

	// A developer session, obtained the only way one can be: every site.
	liveToken, liveReq := requestAccess(t, store, "tani-gunluk")
	if err := store.ApproveDevAccess(ctx, liveReq.ID, owner); err != nil {
		t.Fatal(err)
	}
	dev := newClient(t, server.URL)
	if status, _ := get(t, dev, server.URL+DevAccessPathPrefix+liveToken); status != http.StatusSeeOther {
		t.Fatalf("redeeming the developer link answered %d", status)
	}
	b, ours = download(dev)
	if b.Logs.Scope != "all" {
		t.Errorf("the developer's scope is %q", b.Logs.Scope)
	}
	if line, ok := ours["onların satırı"]; !ok || line.Site != theirs {
		t.Errorf("the developer's file is missing another site's line: %+v", ours)
	}
}
