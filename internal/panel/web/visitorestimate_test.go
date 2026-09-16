package web

import (
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/api"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// Every card that reads a visitor figure says when that figure is an
// estimate.
//
// # Why the list is derived and not written down
//
// Two of the six cards read the collector's visitor counts, and since O3
// those counts can come back estimated. A card that prints one without
// the marker presents an approximation as a count - and the rule "mark
// the human and bot cards" is exactly the kind of list that goes stale
// the day somebody adds a third.
//
// So the list is measured instead: perturb the three visitor figures,
// render every card twice, and see whose value moved. Those are the
// cards that depend on the figures, whatever they are called, and each
// of them has to carry Estimated. A new card reading UniqueIPs fails
// this test without anybody remembering to add it here.
func TestEveryCardThatReadsAVisitorCountSaysWhenItIsAnEstimate(t *testing.T) {
	srv := newTestServer(t)
	lang := srv.Renderer.Catalogs().Languages()[0]
	f := ui.NewFormatter(lang, nil)

	quiet := analytics.Dashboard{
		Traffic: analytics.Summary{
			UniqueIPs: 100, BotIPs: 40, HumanIPs: 60, Snapshots: 1000,
		},
		Beacon: analytics.BeaconSummary{
			Pageviews: 10, Visitors: 5, Sessions: 7, BounceRate: 0.5,
		},
	}
	busy := quiet
	busy.Traffic.UniqueIPs, busy.Traffic.BotIPs, busy.Traffic.HumanIPs = 900, 400, 500

	var depends []cardID
	for id, def := range cards {
		if def.Value(quiet, f) != def.Value(busy, f) {
			depends = append(depends, id)
		}
	}
	if len(depends) == 0 {
		t.Fatal("no card's value moved when the visitor figures changed, so this test " +
			"is measuring nothing. Either the cards stopped reading those figures or " +
			"the perturbation above no longer perturbs anything.")
	}

	for _, id := range depends {
		def := cards[id]
		if def.Estimated == nil {
			t.Errorf("card %q reads a visitor figure and has no Estimated. Since O3 "+
				"the read API answers those figures either exactly or from merged "+
				"daily sketches, and a card that does not ask which prints an "+
				"estimate as a count - on long ranges, which is where the difference "+
				"is largest.", id)
			continue
		}
		// And it has to ask the answer rather than assume one. A card
		// whose Estimated always said true would mark an exact count as
		// approximate, which is the same failure pointing the other way.
		estimated := quiet
		estimated.Traffic.VisitorCounts = analytics.VisitorCountsEstimated
		if def.Estimated(quiet) {
			t.Errorf("card %q calls an exactly counted figure an estimate", id)
		}
		if !def.Estimated(estimated) {
			t.Errorf("card %q does not notice an estimated figure", id)
		}
	}

	// A card that reads none of them must not carry the marker either:
	// the beacon's numbers are never estimated, and saying they might be
	// would be false in the one direction a reader cannot check.
	for id, def := range cards {
		if def.Estimated == nil {
			continue
		}
		var reads bool
		for _, d := range depends {
			reads = reads || d == id
		}
		if !reads {
			t.Errorf("card %q carries Estimated but its value does not move with the "+
				"visitor figures, so it would mark a number the estimate does not "+
				"reach", id)
		}
	}
}

// And the sentence exists in every language, with the margin in it.
//
// Assembled at runtime from a key the template never spells, so neither
// the template walk nor the ui package's dead-key scan can see it - the
// same blind spot the card labels have, and the reason they have a test
// of their own.
func TestTheEstimateSentenceHasWordsInEveryLanguage(t *testing.T) {
	srv := newTestServer(t)
	for _, lang := range srv.Renderer.Catalogs().Languages() {
		if !lang.Has("pano.kart.yaklasik") {
			t.Errorf("%s has no pano.kart.yaklasik", lang.Code)
			continue
		}
		// The margin as that language formats it, rather than as a
		// number typed here: Turkish writes %0,41 and English 0.41%, and
		// a hard-coded spelling would make this test a claim about one
		// locale's punctuation.
		margin := ui.NewFormatter(lang, nil).Percent(
			api.VisitorSketchRelativeError, 2)
		got := lang.Tf("pano.kart.yaklasik", margin)
		if got == "" {
			t.Errorf("%s renders pano.kart.yaklasik as empty", lang.Code)
		}
		// The margin has to reach the sentence. A message that dropped
		// its placeholder would read as a confident "this is an
		// estimate" with no idea how big an estimate.
		if !strings.Contains(got, margin) {
			t.Errorf("%s renders the estimate sentence without the margin (%s) in it: %q",
				lang.Code, margin, got)
		}
	}
}
