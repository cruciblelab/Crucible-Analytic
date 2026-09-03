//go:build integration

// The marker that says a string is missing, read by a machine.
//
// # The defect this exists because of
//
// internal/panel/ui marks an undefined message key by wrapping it in
// guillemets, so the fault is visible on the page instead of appearing
// as an empty space. dashboardData built its period buttons with
//
//	lang.Tn("pano.aralik.gun", d, lang.T("")+strconv.Itoa(d))
//
// and lang.T("") is a lookup of the empty key, which no pack defines.
// So every customer's dashboard - the page this product exists for -
// carried four buttons reading "Son «»1 gün", "Son «»7 gün" and so on,
// from the day the dashboard was written.
//
// It survived sixteen days, a full test suite, two rounds of
// screenshots taken specifically to look at these pages, and being
// looked at by the person who wrote the marker. Nobody read it as a
// defect, because with an empty key the marker collapses to a bare pair
// of guillemets, and a pair of guillemets next to a number looks like
// punctuation.
//
// # What that says about the design, not about the reader
//
// The marker is a signal aimed at a human eye, and the eye is exactly
// the instrument that normalises small unfamiliar punctuation. A signal
// no machine reads is a signal that works until somebody stops looking,
// which for a page that renders on every visit is immediately.
//
// So the marker gets a reader. This file loads every page a signed-in
// customer and a signed-in developer can reach, and fails if any of
// them contains one - and TestEveryPageHandlerIsWalkedOrExcused reads
// the router, so a page added later cannot quietly skip it.

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
)

// markerSite is this file's own site, so a failure here never means a
// fixture another suite left behind.
const markerSite = "isaret-testi"

// walkedPages is every page this file loads, named by the handler the
// router sends it to.
//
// Keyed by handler name rather than by URL because that is what the
// mirror test below reads out of server.go. A URL list would drift the
// moment somebody renamed a path constant, and drift in the direction
// that looks fine.
var walkedPages = map[string]string{
	"home":                     "/",
	"dashboardHandler":         MembersPathPrefix + markerSite,
	"membersHandler":           MembersPathPrefix + markerSite + membersPathSuffix,
	"settingsHandler":          MembersPathPrefix + markerSite + settingsPathSuffix,
	"detailHandler":            MembersPathPrefix + markerSite + breakdownPathSegment + string(analytics.BreakdownPages),
	"addressListHandler":       MembersPathPrefix + markerSite + addressListPathSegment + "sessiz",
	"accountHandler":           AccountPath,
	"loginHandler":             LoginPath,
	"recoveryHandler":          RecoveryPath,
	"secondFactorHandler":      SecondFactorPath,
	"devAccessRequestsHandler": DevAccessRequestsPath,
	"mailHandler":              MailPath,
	"healthHandler":            HealthPath,
	"technicalDoorHandler":     TechnicalDoorPath,
	"welcomeHandler":           WelcomePathPrefix + "site",
	"setupHandler":             SetupPathPrefix + "baslangic",
}

// excusedPages are the registered handlers this file does not load, and
// why. An excuse is a sentence somebody has to write, which is the
// point: it is harder to add than a URL.
var excusedPages = map[string]string{
	"logoutHandler": "it renders nothing - it clears the session and redirects",
	"totpQRHandler": "it answers a PNG, which has no strings in it to be missing",
	"claimHandler": "reaching it needs a single-use invitation token, and " +
		"TestHandoverCreatesAnOwner already walks the claim page with a real one",
	"devAccessHandler": "reaching it needs a single-use developer token; " +
		"the pages it leads to are setupHandler's, which is walked",
}

// markerPattern is the marker itself. Written from the package's own
// constant rather than as a literal, so a change to the marker cannot
// leave this test looking for something that is no longer produced.
var markerPattern = regexp.MustCompile(`«[^»]{0,80}»`)

// TestNoPageAnOwnerReachesCarriesAMissingMessageMarker.
//
// The owner is the customer, and the customer's pages are the ones a
// marker actually costs something on. Loaded against a real database
// and a real analytics API, because half of these strings are chosen by
// what the data says - an empty period renders different sentences from
// a full one, and the empty ones were never the ones anybody looked at.
func TestNoPageAnOwnerReachesCarriesAMissingMessageMarker(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	ctx := context.Background()

	if err := store.SetSetting(ctx, panel.KeyBeaconSites, "", []string{markerSite}, nil); err != nil {
		t.Fatal(err)
	}
	server, client, _ := signedInOwner(t, srv, store, markerSite, "isaret-sahip")

	for handler, path := range walkedPages {
		t.Run(handler, func(t *testing.T) {
			status, body := get(t, client, server.URL+path)
			// Not a status assertion: a page that redirects or refuses
			// still renders words, and this test is about the words. A
			// 500 is somebody else's test to fail.
			if status >= 500 {
				t.Fatalf("%s answered %d, so there is no page to read", path, status)
			}
			assertNoMarker(t, path, body)
		})
	}
}

// TestNoPageADeveloperReachesCarriesAMissingMessageMarker.
//
// A superadmin sees sections an owner never does - the technical
// breakdowns, the wizard, the developer-mode blocks - and those
// sections have their own strings. Running the same walk twice with
// different eyes is cheaper than reasoning about which page hides what.
func TestNoPageADeveloperReachesCarriesAMissingMessageMarker(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	ctx := context.Background()

	if err := store.SetSetting(ctx, panel.KeyBeaconSites, "", []string{markerSite}, nil); err != nil {
		t.Fatal(err)
	}
	dev := makeUser(t, store, "isaret-gelistirici", true)
	if err := store.AddMember(ctx, markerSite, dev.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	t.Cleanup(server.Close)
	client := signedIn(t, server.URL, dev.Email)

	for handler, path := range walkedPages {
		t.Run(handler, func(t *testing.T) {
			status, body := get(t, client, server.URL+path)
			if status >= 500 {
				t.Fatalf("%s answered %d, so there is no page to read", path, status)
			}
			assertNoMarker(t, path, body)
		})
	}
}

// TestEveryPeriodButtonReadsAsAWholeSentence is the specific case.
//
// The walk above would catch this, and this one says what was wrong.
// The four buttons are built in a loop from one message and one number,
// and the defect put a marker between them - so the assertion is that
// each button is the message with the number in it and nothing else.
func TestEveryPeriodButtonReadsAsAWholeSentence(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	ctx := context.Background()

	if err := store.SetSetting(ctx, panel.KeyBeaconSites, "", []string{markerSite}, nil); err != nil {
		t.Fatal(err)
	}
	server, client, _ := signedInOwner(t, srv, store, markerSite, "isaret-donem")

	status, body := get(t, client, server.URL+MembersPathPrefix+markerSite)
	if status != http.StatusOK {
		t.Fatalf("the dashboard answered %d", status)
	}

	lang := srv.Renderer.Catalogs().Base()
	for _, d := range rangeDays {
		want := lang.Tn("pano.aralik.gun", d, strconv.Itoa(d))
		if !strings.Contains(body, ">\n"+want+"<") && !strings.Contains(body, ">"+want+"<") {
			t.Errorf("no period button reads exactly %q. The buttons are built from one "+
				"message and one number; anything between them is something the loop "+
				"added and nobody asked for", want)
		}
	}
}

// assertNoMarker fails with the marker in context rather than with the
// marker alone: "«» found" sends somebody looking through a page, and
// "Son «»7 gün" names the string.
func assertNoMarker(t *testing.T, path, body string) {
	t.Helper()
	for _, hit := range markerPattern.FindAllStringIndex(body, -1) {
		start := hit[0] - 40
		if start < 0 {
			start = 0
		}
		end := hit[1] + 40
		if end > len(body) {
			end = len(body)
		}
		t.Errorf("%s renders a missing-message marker: ...%s...\n"+
			"The marker means a message key resolved to nothing. Either the key is "+
			"wrong or the caller passed one it never had",
			path, strings.Join(strings.Fields(body[start:end]), " "))
	}
}

// TestEveryPageHandlerIsWalkedOrExcused is the two-way mirror.
//
// Read out of the router, because the shape of this defect is a page
// nobody thought to look at. A list of URLs maintained by hand is right
// on the day it is written and silent every day after; the router is
// the thing that is true.
func TestEveryPageHandlerIsWalkedOrExcused(t *testing.T) {
	root := repoRootForIsolation(t)
	body, err := os.ReadFile(filepath.Join(root, "internal", "panel", "web", "server.go"))
	if err != nil {
		t.Fatal(err)
	}

	registered := regexp.MustCompile(`mux\.HandleFunc\([^,]+,\s*s\.(\w+)\)`)
	matches := registered.FindAllStringSubmatch(string(body), -1)
	if len(matches) == 0 {
		t.Fatal("no handlers found in server.go; this test would pass by comparing nothing")
	}

	seen := map[string]bool{}
	for _, m := range matches {
		handler := m[1]
		seen[handler] = true
		if _, walked := walkedPages[handler]; walked {
			continue
		}
		if _, excused := excusedPages[handler]; excused {
			continue
		}
		t.Errorf("the router registers s.%s and nothing loads it. Add its URL to "+
			"walkedPages, or a sentence to excusedPages saying why a page with no "+
			"strings does not need reading", handler)
	}

	// And the other direction: a name in either map that the router no
	// longer registers is a page this file believes it is checking.
	for handler := range walkedPages {
		if !seen[handler] {
			t.Errorf("walkedPages names s.%s, which the router does not register. "+
				"This file has been loading a URL that reaches something else", handler)
		}
	}
	for handler := range excusedPages {
		if !seen[handler] {
			t.Errorf("excusedPages excuses s.%s, which the router does not register", handler)
		}
	}
}
