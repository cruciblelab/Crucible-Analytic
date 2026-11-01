package analytics

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/api"
)

// TestTheActivitySurvivesTheApisOwnJson ties the decoder to the
// producer's type, the way the coverage bands are tied
// (TestTheCoverageBandsSurviveTheApisOwnJson): a name either side
// changes decodes as zero, and zero here reads as "nothing in the last
// hour" - a healthy-looking answer.
func TestTheActivitySurvivesTheApisOwnJson(t *testing.T) {
	last := time.Date(2026, 10, 30, 14, 2, 0, 0, time.UTC)
	asOf := last.Add(26 * time.Hour)
	body, err := json.Marshal(api.BeaconActivity{SiteID: "s", LastEvent: &last, LastHour: 7, AsOf: asOf})
	if err != nil {
		t.Fatal(err)
	}
	var gotPath string
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write(body)
	}))
	got, err := c.BeaconActivity(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/v1/sites/s/beacon/activity" {
		t.Errorf("asked %q", gotPath)
	}
	if got.LastEvent == nil || !got.LastEvent.Equal(last) || got.LastHour != 7 || !got.AsOf.Equal(asOf) {
		t.Errorf("decoded %+v from %s", got, body)
	}
	if d, ok := got.Ago(); !ok || d != 26*time.Hour {
		t.Errorf("Ago is %v, %v; want 26h against the database's clock", d, ok)
	}
}

// TestNeverIsNotZero: a site that never sent anything decodes as no
// last event, and Ago says so instead of answering with the age of the
// year one.
func TestNeverIsNotZero(t *testing.T) {
	body, _ := json.Marshal(api.BeaconActivity{SiteID: "s", AsOf: time.Now()})
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	got, err := c.BeaconActivity(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Ago(); ok || got.LastEvent != nil {
		t.Errorf("a site with no event decoded as %+v", got)
	}
}

// TestAnEventAheadOfTheClockIsJustNow: the beacon stamps before the row
// lands, so the newest event can be a moment past as_of.
func TestAnEventAheadOfTheClockIsJustNow(t *testing.T) {
	asOf := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	ahead := asOf.Add(300 * time.Millisecond)
	if d, ok := (BeaconActivity{LastEvent: &ahead, AsOf: asOf}).Ago(); !ok || d != 0 {
		t.Errorf("Ago is %v, %v; want 0, true", d, ok)
	}
}
