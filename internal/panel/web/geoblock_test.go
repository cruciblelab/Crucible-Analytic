package web

import (
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
)

// TestTheGeoRefusalCounterIsDrawnOnlyWhenARuleTurnedSomethingAway: every
// collector that counts reports it, zero included, so the Health page
// draws it only once a rule has turned something away.
func TestTheGeoRefusalCounterIsDrawnOnlyWhenARuleTurnedSomethingAway(t *testing.T) {
	lang := turkishCatalogue(t)
	label := lang.T("saglik.sayac." + heartbeat.CounterGeoBlocked)
	for _, c := range labelledCounters(lang, map[string]int64{
		heartbeat.CounterWritten: 10, heartbeat.CounterGeoBlocked: 0,
	}) {
		if c.Label == label {
			t.Error("a count of zero refusals is drawn")
		}
	}
	var seen bool
	for _, c := range labelledCounters(lang, map[string]int64{
		heartbeat.CounterWritten: 10, heartbeat.CounterGeoBlocked: 4,
	}) {
		if c.Label == label {
			seen = true
			if c.Value != 4 {
				t.Errorf("the count reads %d, want 4", c.Value)
			}
		}
	}
	if !seen {
		t.Error("four refusals are not drawn")
	}
}
