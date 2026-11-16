package analytics

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/api"
)

// W2 put one name on this wire: how many events the counts left out as
// automated. Decoded through the read API's own type, for the reason
// visitorcounts_test.go gives - a misspelt name decodes as zero, and zero
// is the one value under which the dashboard says nothing.
func TestTheAutomatedCountSurvivesTheApisOwnJson(t *testing.T) {
	body, err := json.Marshal(api.BeaconSummary{SiteID: "s", Pageviews: 189, AutomatedEvents: 303})
	if err != nil {
		t.Fatal(err)
	}
	from, to := window()
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	got := c.FetchSite(context.Background(), "s", from, to, SiteRequest{Beacon: true})
	if got.BeaconErr != nil {
		t.Fatalf("beacon summary: %v", got.BeaconErr)
	}
	if got.Beacon.AutomatedEvents != 303 {
		t.Errorf("automated events decoded as %d, want 303", got.Beacon.AutomatedEvents)
	}
}
