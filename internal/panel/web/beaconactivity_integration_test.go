//go:build integration

// The Health page's per-site beacon section (B3g), read through the real
// read API from the real table, and through the diagnostic file that
// carries the same answer.

package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

const (
	activityOwn     = "bs-kendi"
	activityOther   = "bs-baska"
	activitySilent  = "bs-hic"
	activityOwnerA  = "bs-sahip-a"
	activityOwnerB  = "bs-sahip-b"
	activityOperate = "bs-isletmeci"
)

// activityRow is one row of the section, by its site.
func activityRow(t *testing.T, body, site string) (row string, ok bool) {
	t.Helper()
	m := regexp.MustCompile(`(?s)<tr data-site="` + regexp.QuoteMeta(site) + `" data-durum="([a-z]+)">.*?</tr>`).
		FindString(body)
	return m, m != ""
}

// activityFile downloads the diagnostic file, raw.
func activityFile(t *testing.T, c *http.Client, base string) map[string]json.RawMessage {
	t.Helper()
	resp, err := c.Get(base + DiagnosticPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the diagnostic file answered %d", resp.StatusCode)
	}
	var got map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got
}

// TestTheBeaconSectionShowsEachPersonOnlyTheirSites.
//
// Two owners, two sites, events on both. The page and the file each
// show an owner their own site's row and never the other's - the
// section asks the read API by site, and the read API's token sees every
// site, so the panel's list is the only thing between one customer and
// another's numbers. The operator (a superadmin) sees both.
func TestTheBeaconSectionShowsEachPersonOnlyTheirSites(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	ctx := context.Background()

	ownerA := makeUser(t, store, activityOwnerA, false)
	ownerB := makeUser(t, store, activityOwnerB, false)
	operator := makeUser(t, store, activityOperate, true)
	for _, m := range []struct {
		site string
		user panel.User
	}{{activityOwn, ownerA}, {activitySilent, ownerA}, {activityOther, ownerB}, {activityOwn, operator}} {
		if err := store.AddMember(ctx, m.site, m.user.ID, panel.RoleOwner, panel.Grant{}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	seedBeacon(t, activityOwn, now.Add(-20*time.Minute), []beaconRow{
		{visitor: "a", kind: "pageview", path: "/"}, {visitor: "b", kind: "pageview", path: "/"},
	})
	seedBeacon(t, activityOther, now.Add(-3*time.Minute), []beaconRow{{visitor: "c", kind: "pageview", path: "/"}})
	testdb.CleanSite(t, testdb.Admin(t), activitySilent)

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	lang := srv.Renderer.Catalogs().Base()

	a := signedIn(t, server.URL, ownerA.Email)
	_, body := get(t, a, server.URL+HealthPath)
	row, ok := activityRow(t, body, activityOwn)
	if !ok {
		t.Fatalf("the owner's own site has no row in the section")
	}
	if !strings.Contains(row, `data-durum="var"`) {
		t.Errorf("the owner's site row does not say it reported:\n%s", row)
	}
	// A time, never a count: this page carries no number about a visitor.
	if regexp.MustCompile(`<td>\d+</td>`).MatchString(row) {
		t.Errorf("the row carries a count:\n%s", row)
	}
	// Worded by the same formatter, about the same gap. seedBeacon spaces
	// its rows a minute apart, so the newest is 19 minutes old - or 20 if
	// the minute turned between the seed and the read.
	f := ui.NewFormatter(lang, time.UTC)
	if !strings.Contains(row, shown(f.SinceAt(now, now.Add(19*time.Minute)))) &&
		!strings.Contains(row, shown(f.SinceAt(now, now.Add(20*time.Minute)))) {
		t.Errorf("the row does not say the newest event came about 19 minutes ago:\n%s", row)
	}
	if row, ok := activityRow(t, body, activitySilent); !ok ||
		!strings.Contains(row, `data-durum="hic"`) || !strings.Contains(row, shown(lang.T("saglik.beacon_site.hic"))) {
		t.Errorf("a site that never sent an event is not drawn as such:\n%s", row)
	}
	if _, ok := activityRow(t, body, activityOther); ok {
		t.Fatal("an owner was shown another customer's site in the beacon section")
	}
	// The diagnostic file leaves the machine and carries none of it, and
	// says so in its omitted list rather than leaving an absence.
	file := activityFile(t, a, server.URL)
	if _, ok := file["beacon_sites"]; ok {
		t.Error("the diagnostic file carries the per-site beacon section")
	}
	if !strings.Contains(string(file["omitted"]), lang.T("saglik.tani.disarida.beacon_site")[:20]) {
		t.Errorf("the file's omitted list does not say the section was left out: %s", file["omitted"])
	}
	for _, site := range []string{activityOwn, activityOther} {
		if strings.Contains(string(file["services"])+string(file["omitted"]), site) {
			t.Errorf("the diagnostic file names %s", site)
		}
	}

	op := signedIn(t, server.URL, operator.Email)
	_, body = get(t, op, server.URL+HealthPath)
	for _, site := range []string{activityOwn, activityOther} {
		if _, ok := activityRow(t, body, site); !ok {
			t.Errorf("the operator's section has no row for %s", site)
		}
	}
}

// TestABeaconRowThatCouldNotBeReadSaysSo.
//
// The read API down: the panel still knows the owner's site, so the row
// is drawn, and it says the read failed - not "no event yet", which is
// what a snippet nobody embedded looks like, and an outage must not look
// like that.
func TestABeaconRowThatCouldNotBeReadSaysSo(t *testing.T) {
	srv, store := setupTestServer(t)
	dead, err := analytics.New("http://127.0.0.1:1", "jeton")
	if err != nil {
		t.Fatal(err)
	}
	srv.Analytics = dead
	owner := makeUser(t, store, "bs-kapali", false)
	if err := store.AddMember(context.Background(), activityOwn, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	c := signedIn(t, server.URL, owner.Email)
	lang := srv.Renderer.Catalogs().Base()

	status, body := get(t, c, server.URL+HealthPath)
	if status != http.StatusOK {
		t.Fatalf("the health page answered %d with its API down", status)
	}
	row, ok := activityRow(t, body, activityOwn)
	if !ok {
		t.Fatal("the owner's site has no row while the API is down; the list comes from the panel")
	}
	if !strings.Contains(row, `data-durum="okunamadi"`) || !strings.Contains(row, shown(lang.T("saglik.beacon_site.okunamadi"))) {
		t.Errorf("the row does not say the read failed:\n%s", row)
	}
	if strings.Contains(row, shown(lang.T("saglik.beacon_site.hic"))) {
		t.Errorf("a row whose read failed says no event ever came:\n%s", row)
	}
}

// TestTheBeaconSectionStopsAtFiftySitesAndSaysSo.
//
// Fifty-two sites for one owner, the API down so each read fails at
// once: fifty rows, and a sentence naming the two left out - a section
// that silently showed fifty would read as the whole list.
func TestTheBeaconSectionStopsAtFiftySitesAndSaysSo(t *testing.T) {
	srv, store := setupTestServer(t)
	dead, err := analytics.New("http://127.0.0.1:1", "jeton")
	if err != nil {
		t.Fatal(err)
	}
	srv.Analytics = dead
	owner := makeUser(t, store, "bs-elli", false)
	for i := range maxActivitySites + 2 {
		if err := store.AddMember(context.Background(), fmt.Sprintf("bs-elli-%02d", i), owner.ID,
			panel.RoleOwner, panel.Grant{}); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	_, body := get(t, signedIn(t, server.URL, owner.Email), server.URL+HealthPath)

	if n := strings.Count(body, `<tr data-site="bs-elli-`); n != maxActivitySites {
		t.Errorf("the section drew %d rows for %d sites, want %d", n, maxActivitySites+2, maxActivitySites)
	}
	lang := srv.Renderer.Catalogs().Base()
	if !strings.Contains(body, shown(lang.Tf("saglik.beacon_site.fazlasi", 2, maxActivitySites))) {
		t.Error("the section does not say two sites were left out")
	}
}
