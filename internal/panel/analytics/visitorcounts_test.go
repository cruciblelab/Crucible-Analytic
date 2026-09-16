package analytics

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/api"
)

// O3 put two new names on this wire, and a page's honesty rests on both.
//
// # Why this is a round trip and not two constants compared
//
// Because the failure is silent in the direction that matters. If
// visitor_counts decoded as "" - a rename, a typo, a field the API moved
// - the panel would read that as "not estimated" and print an estimate
// as a count, on exactly the long ranges where the estimate is used.
// Nothing would look broken.
//
// So this test names no JSON at all. It marshals the read API's own
// Summary and requires the panel's decoder to see the values, which is a
// claim neither side can satisfy alone. The same arrangement the
// coverage bands got, after a decoder and its fixture agreed with each
// other and with nothing else for months.
func TestTheVisitorCountMethodSurvivesTheApisOwnJson(t *testing.T) {
	produced := api.Summary{
		SiteID:            "s",
		UniqueIPs:         1000,
		BotIPs:            400,
		HumanIPs:          600,
		Snapshots:         5_000_000,
		VisitorCounts:     api.VisitorCountEstimated,
		VisitorCountError: api.VisitorSketchRelativeError,
	}
	body, err := json.Marshal(produced)
	if err != nil {
		t.Fatal(err)
	}

	from, to := window()
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	got := c.FetchSite(context.Background(), "s", from, to, SiteRequest{Traffic: true})
	if got.TrafficErr != nil {
		t.Fatalf("summary: %v", got.TrafficErr)
	}

	if got.Traffic.VisitorCounts != string(produced.VisitorCounts) {
		t.Errorf("visitor counts decoded as %q, want %q. An empty string here reads "+
			"as \"counted\" everywhere in the panel, so a rename on either side turns "+
			"every estimate into a figure presented as exact.",
			got.Traffic.VisitorCounts, produced.VisitorCounts)
	}
	if got.Traffic.VisitorCountError != produced.VisitorCountError {
		t.Errorf("visitor count error decoded as %v, want %v. Zero here is what an "+
			"exact answer carries, so a dropped field would print a margin of ±0%% "+
			"beside an estimate.", got.Traffic.VisitorCountError, produced.VisitorCountError)
	}

	// And the constant the panel's own code compares against is the one
	// the API emits. Two spellings of one wire value, held equal here
	// rather than by everybody remembering.
	if VisitorCountsEstimated != string(api.VisitorCountEstimated) {
		t.Errorf("this package says %q and the read API says %q",
			VisitorCountsEstimated, api.VisitorCountEstimated)
	}

	// The other branch, from the same bytes: an exact answer must not
	// arrive looking like an estimate.
	produced.VisitorCounts = api.VisitorCountExact
	produced.VisitorCountError = 0
	exactBody, err := json.Marshal(produced)
	if err != nil {
		t.Fatal(err)
	}
	c = clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(exactBody)
	}))
	got = c.FetchSite(context.Background(), "s", from, to, SiteRequest{Traffic: true})
	if got.TrafficErr != nil {
		t.Fatalf("summary: %v", got.TrafficErr)
	}
	if got.Traffic.VisitorCounts == VisitorCountsEstimated {
		t.Error("an exact answer decoded as estimated. Without this half, a decoder " +
			"that hard-coded \"estimated\" would pass every assertion above.")
	}
}
