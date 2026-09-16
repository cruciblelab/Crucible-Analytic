//go:build integration

// The dashboard draws the estimate marker, with a row on the page.
//
// # Why this is drawn and not asserted on a view struct
//
// Because a page nobody renders can ship with a template reading a field
// that does not exist. That happened in this project once already - a
// count added to a view type and not to the template's, unit tests
// green, and the page answering 500 the first time anybody looked at it.
// So the assertion is made on the HTML.
//
// The read API here is a stub rather than the real service, and
// deliberately: the development database has no timescaledb_toolkit, so
// a real API against it can only ever answer "exact" and the marked
// branch would be unreachable. What ties the stub to the real service is
// a separate test in internal/panel/analytics, which marshals the API's
// own Summary type and decodes it with the panel's - so a rename cannot
// leave this fixture alone.

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/api"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

func TestTheDashboardSaysWhenAVisitorCountIsAnEstimate(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()

	// The stub's answer, flipped between the two requests below. One
	// server and one signed-in session, because setupTestServer holds a
	// lock for the length of the test and a second call would hang.
	estimated := true
	readAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/summary") || strings.Contains(r.URL.Path, "/beacon/") {
			http.NotFound(w, r)
			return
		}
		body := map[string]any{
			"site_id":    dashboardSite,
			"unique_ips": 1200,
			"bot_ips":    500,
			"human_ips":  700,
			"snapshots":  9_000_000,
		}
		if estimated {
			body["visitor_counts"] = analytics.VisitorCountsEstimated
			body["visitor_count_error"] = api.VisitorSketchRelativeError
		} else {
			body["visitor_counts"] = string(api.VisitorCountExact)
			body["visitor_count_error"] = 0.0
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer readAPI.Close()

	client, err := analytics.New(readAPI.URL, "jeton")
	if err != nil {
		t.Fatal(err)
	}
	srv.Analytics = client

	owner := makeUser(t, store, "pano-yaklasik", false)
	if err := store.AddMember(ctx, dashboardSite, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	session := signedIn(t, server.URL, owner.Email)

	// The sentence as the page will render it: the margin comes from the
	// API's own constant, formatted by the panel's own formatter. Typing
	// "0,41%" here would have been a claim about Turkish punctuation -
	// which is how the first version of this test failed, since tr
	// writes the sign first.
	base := srv.Renderer.Catalogs().Base()
	sentence := base.Tf("pano.kart.yaklasik",
		ui.NewFormatter(base, nil).Percent(api.VisitorSketchRelativeError, 2))

	status, body := get(t, session, server.URL+sitePath(dashboardSite))
	if status != http.StatusOK {
		t.Fatalf("the dashboard answered %d", status)
	}
	if !strings.Contains(body, "kart-yaklasik") {
		t.Error("the page draws no estimate marker beside an estimated figure. A " +
			"reader taking that number into a report has no way to know it is not a " +
			"count.")
	}
	if !strings.Contains(body, shown(sentence)) {
		t.Errorf("the page does not carry the margin sentence %q", sentence)
	}
	// The number itself still has to be there. A marker on a blank card
	// would be a page that says "approximately nothing".
	if !strings.Contains(body, "700") {
		t.Error("the human figure is missing from a page that marked it as an estimate")
	}

	// And the other way, from the same page and the same session: an
	// exact answer must not be marked. Without this half, a template
	// that printed the marker unconditionally would pass everything
	// above.
	estimated = false
	status, body = get(t, session, server.URL+sitePath(dashboardSite))
	if status != http.StatusOK {
		t.Fatalf("the dashboard answered %d on the exact pass", status)
	}
	if strings.Contains(body, "kart-yaklasik") {
		t.Error("the page marks an exactly counted figure as an estimate")
	}
	if strings.Contains(body, shown(sentence)) {
		t.Error("the page prints a margin beside a counted figure")
	}
	if !strings.Contains(body, "700") {
		t.Error("the human figure is missing from the exact pass")
	}
}
