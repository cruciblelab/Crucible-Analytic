package web

import (
	"slices"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
)

// noWarning fails the test if anything is reported unknown.
func noWarning(t *testing.T) func(string) {
	t.Helper()
	return func(name string) { t.Errorf("%q was reported unknown and should not be", name) }
}

// collect gathers the ids reported unknown.
func collect(into *[]string) func(string) {
	return func(name string) { *into = append(*into, name) }
}

// TestAnUnsetVisibleSetIsTheDefault, never a blank page.
//
// Every deployment that existed before this setting is unset. A page that
// emptied itself on upgrade would be the worst possible reading of "not
// configured" - and it would hide the product from the customer paying
// for it, which is the same rule D5 states for views.
func TestAnUnsetVisibleSetIsTheDefault(t *testing.T) {
	for _, raw := range [][]string{nil, {}} {
		if got := resolveCards(raw, noWarning(t)); !slices.Equal(got, defaultCards) {
			t.Errorf("cards from %v = %v, want the default %v", raw, got, defaultCards)
		}
		if got := resolveBreakdowns(raw, noWarning(t)); !slices.Equal(got, defaultBreakdowns) {
			t.Errorf("breakdowns from %v = %v, want the default %v", raw, got, defaultBreakdowns)
		}
	}
}

// TestAChosenSetIsHonouredInTheOrderItWasChosen.
//
// The order is the installer's, not the registry's: somebody who put the
// bot card first meant it to be first.
func TestAChosenSetIsHonouredInTheOrderItWasChosen(t *testing.T) {
	got := resolveCards([]string{string(cardBotIPs), string(cardVisitors)}, noWarning(t))
	want := []cardID{cardBotIPs, cardVisitors}
	if !slices.Equal(got, want) {
		t.Errorf("cards = %v, want %v", got, want)
	}

	kinds := resolveBreakdowns([]string{
		string(analytics.BreakdownEvents), string(analytics.BreakdownPages),
	}, noWarning(t))
	wantKinds := []analytics.BreakdownKind{analytics.BreakdownEvents, analytics.BreakdownPages}
	if !slices.Equal(kinds, wantKinds) {
		t.Errorf("breakdowns = %v, want %v", kinds, wantKinds)
	}
}

// TestAnUnknownIdIsDroppedAndReported.
//
// These values come out of a database. A page that trusted them would put
// a stored string into a catalog lookup and into an API path, and the
// value would have been written by a version of this binary that no
// longer exists.
func TestAnUnknownIdIsDroppedAndReported(t *testing.T) {
	var warned []string
	got := resolveCards([]string{string(cardVisitors), "ja4", "../../etc/passwd"}, collect(&warned))
	if want := []cardID{cardVisitors}; !slices.Equal(got, want) {
		t.Errorf("cards = %v, want %v", got, want)
	}
	if len(warned) != 2 {
		t.Errorf("reported %v unknown, want both of the two", warned)
	}

	warned = nil
	kinds := resolveBreakdowns([]string{"raw", string(analytics.BreakdownPages)}, collect(&warned))
	if want := []analytics.BreakdownKind{analytics.BreakdownPages}; !slices.Equal(kinds, want) {
		t.Errorf("breakdowns = %v, want %v", kinds, want)
	}
	if len(warned) != 1 {
		t.Errorf("reported %v unknown, want exactly one", warned)
	}
}

// TestASetOfNothingKnownStillDrawsAPage. An upgrade that renamed every id
// must not leave the customer looking at nothing.
func TestASetOfNothingKnownStillDrawsAPage(t *testing.T) {
	var warned []string
	if got := resolveCards([]string{"eski", "daha_eski"}, collect(&warned)); !slices.Equal(got, defaultCards) {
		t.Errorf("cards = %v, want the default", got)
	}
	if len(warned) != 2 {
		t.Errorf("reported %v unknown, want two", warned)
	}
	if got := resolveBreakdowns([]string{"eski"}, collect(&warned)); !slices.Equal(got, defaultBreakdowns) {
		t.Errorf("breakdowns = %v, want the default", got)
	}
}

// TestADuplicateIsDrawnOnce. A stored list is edited by people and by
// forms; the same card twice is a doubled block, not a preference.
func TestADuplicateIsDrawnOnce(t *testing.T) {
	got := resolveCards([]string{string(cardVisitors), string(cardVisitors)}, noWarning(t))
	if want := []cardID{cardVisitors}; !slices.Equal(got, want) {
		t.Errorf("cards = %v, want %v", got, want)
	}
}

// TestAClosedBlockIsNeverRequested is the phase.
//
// Hiding a block in the template would leave the query running: the
// database still does the work, the API still answers, and the only thing
// saved is some HTML. The setting exists so the saving reaches the
// database, so what is asserted here is the request, not the page.
func TestAClosedBlockIsNeverRequested(t *testing.T) {
	cases := []struct {
		name  string
		sets  visibleSets
		want  []analytics.BreakdownKind
		trafc bool
		bcn   bool
	}{
		{
			name: "the whole default page",
			sets: visibleSets{Cards: defaultCards, Breakdowns: defaultBreakdowns},
			want: defaultBreakdowns, trafc: true, bcn: true,
		},
		{
			// The customer who was told about bots and wanted only that.
			// No beacon card and no breakdown means the beacon summary is
			// never fetched - one fewer query per page view, for ever.
			name: "collector cards only",
			sets: visibleSets{Cards: []cardID{cardHumanIPs, cardBotIPs}},
			want: nil, trafc: true, bcn: false,
		},
		{
			// The ordinary shop: pageviews and where they came from.
			name: "one card and one breakdown",
			sets: visibleSets{
				Cards:      []cardID{cardPageviews},
				Breakdowns: []analytics.BreakdownKind{analytics.BreakdownPages},
			},
			want:  []analytics.BreakdownKind{analytics.BreakdownPages},
			trafc: false, bcn: true,
		},
		{
			// A breakdown with no beacon card still needs the beacon
			// summary, because every share divides by it. Skipping it
			// would empty the share column and say nothing about why.
			name: "a breakdown next to a collector card",
			sets: visibleSets{
				Cards:      []cardID{cardBotIPs},
				Breakdowns: []analytics.BreakdownKind{analytics.BreakdownCountries},
			},
			want:  []analytics.BreakdownKind{analytics.BreakdownCountries},
			trafc: true, bcn: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.sets.request(sectionRows)

			if req.Traffic != tc.trafc {
				t.Errorf("traffic summary requested = %v, want %v", req.Traffic, tc.trafc)
			}
			if req.Beacon != tc.bcn {
				t.Errorf("beacon summary requested = %v, want %v", req.Beacon, tc.bcn)
			}

			got := make([]analytics.BreakdownKind, 0, len(req.Breakdowns))
			for _, b := range req.Breakdowns {
				got = append(got, b.Kind)
				if b.Limit != sectionRows {
					t.Errorf("%s asked for %d rows, want %d", b.Kind, b.Limit, sectionRows)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("breakdowns requested = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestEveryBreakdownStillDividesByTheBeaconSummary.
//
// request() forces the beacon summary on whenever any breakdown is shown,
// and that shortcut is only correct while every breakdown is
// beacon-sourced and divides by it. D3 adds collector-side ones; this is
// the test that will say so rather than a comment nobody re-reads.
func TestEveryBreakdownStillDividesByTheBeaconSummary(t *testing.T) {
	for kind, def := range breakdownDefs {
		if def.Source != sourceBeacon {
			t.Errorf("%s reads %s, so request() forcing the beacon summary on is no longer "+
				"the right rule - it has to ask each breakdown for its own source", kind, def.Source)
		}
		if def.Metric != metricPageviews && def.Metric != metricEvents {
			t.Errorf("%s divides by %q, which is not one of the beacon summary's totals",
				kind, def.Metric)
		}
	}
}

// TestTheDefaultSetsAreDrawable. Both defaults are the fallback for every
// unset deployment, so a typo in either is a blank page nobody asked for.
func TestTheDefaultSetsAreDrawable(t *testing.T) {
	if len(defaultCards) == 0 || len(defaultBreakdowns) == 0 {
		t.Fatal("a default set is empty; every unset deployment would draw nothing")
	}
	for _, id := range defaultCards {
		if _, ok := cards[id]; !ok {
			t.Errorf("default card %q is not in the registry", id)
		}
	}
	for _, kind := range defaultBreakdowns {
		if _, ok := breakdownDefs[kind]; !ok {
			t.Errorf("default breakdown %q is not in the registry", kind)
		}
	}
}
