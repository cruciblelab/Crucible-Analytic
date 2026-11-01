package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/api"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// activityServer answers the activity route with what the test says.
func activityServer(t *testing.T, srv *Server, status int, body any) {
	t.Helper()
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/beacon/activity") {
			t.Errorf("asked %q", r.URL.Path)
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(h.Close)
	c, err := analytics.New(h.URL, "jeton")
	if err != nil {
		t.Fatal(err)
	}
	srv.Analytics = c
}

// TestTheLastEventIsMeasuredOnTheDatabasesClock.
//
// The event ten minutes before the database's now, and the database's
// now two years from the panel's: read against the panel's clock the
// event is in the future and the sentence would be an absolute date;
// read against as_of it is "10 dakika önce". A machine whose clock is
// not the database's is the case B3e measured (the uptime mixed them).
func TestTheLastEventIsMeasuredOnTheDatabasesClock(t *testing.T) {
	srv := newTestServer(t)
	lang := srv.Renderer.Catalogs().Base()
	f := ui.NewFormatter(lang, time.UTC)

	asOf := time.Now().Add(2 * 365 * 24 * time.Hour).UTC().Truncate(time.Second)
	last := asOf.Add(-10 * time.Minute)
	activityServer(t, srv, http.StatusOK, api.BeaconActivity{SiteID: "s", LastEvent: &last, AsOf: asOf})

	got := srv.lastBeaconEvent(t.Context(), lang, f, "s")
	want := lang.Tf("pano.son_olay", f.SinceAt(last, asOf))
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if want == lang.Tf("pano.son_olay", f.DateTime(last)) {
		t.Fatal("the two clocks give the same sentence here, so this test cannot tell them apart")
	}
}

// TestNoLastEventIsNoSentence: a failed read and a site that never sent
// anything both add nothing to a card that already says the truth.
func TestNoLastEventIsNoSentence(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		body   any
	}{
		"refused": {http.StatusForbidden, map[string]string{"error": "no"}},
		"never":   {http.StatusOK, api.BeaconActivity{SiteID: "s", AsOf: time.Now()}},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newTestServer(t)
			lang := srv.Renderer.Catalogs().Base()
			activityServer(t, srv, c.status, c.body)
			if got := srv.lastBeaconEvent(t.Context(), lang, ui.NewFormatter(lang, time.UTC), "s"); got != "" {
				t.Errorf("got %q, want no sentence", got)
			}
		})
	}
}

// TestAHealthRowIsWordedOnTheDatabasesClock: the Health page's row has
// the dashboard's rule, and its own code, so its own test - the same
// two-years-apart clocks. And a row whose read failed carries no time.
func TestAHealthRowIsWordedOnTheDatabasesClock(t *testing.T) {
	srv := newTestServer(t)
	lang := srv.Renderer.Catalogs().Base()
	f := ui.NewFormatter(lang, time.UTC)
	asOf := time.Now().Add(2 * 365 * 24 * time.Hour).UTC().Truncate(time.Second)
	last := asOf.Add(-10 * time.Minute)

	row := activityRowFrom(f, siteActivity{Site: "s", State: activityReported,
		Activity: analytics.BeaconActivity{LastEvent: &last, LastHour: 4, AsOf: asOf}})
	if row.LastEvent != f.SinceAt(last, asOf) {
		t.Errorf("the row is %+v, want %q", row, f.SinceAt(last, asOf))
	}
	if row.LastEvent == f.DateTime(last) {
		t.Fatal("the two clocks word this alike, so the test cannot tell them apart")
	}

	failed := activityRowFrom(f, siteActivity{Site: "s", State: activityUnreadable,
		Activity: analytics.BeaconActivity{LastEvent: &last, AsOf: asOf}})
	if failed.LastEvent != "" {
		t.Errorf("a row whose read failed carries %+v", failed)
	}
}
