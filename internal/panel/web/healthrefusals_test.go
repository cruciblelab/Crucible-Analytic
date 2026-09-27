package web

import (
	"reflect"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

func turkishCatalogue(t *testing.T) *ui.Language {
	t.Helper()
	catalogs, err := ui.LoadCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	lang := catalogs.ByCode("tr")
	if lang == nil {
		t.Fatal("no Turkish catalogue")
	}
	return lang
}

// TestRefusalsAreDrawnUnderTheirTotal: the reasons are parts of the
// refusals total and are drawn as such - under it, only the ones that
// happened, in the heartbeat's order - and nothing else is split.
//
// "Only the ones that happened" is what keeps five zeros off every
// service row, and "under it" is what keeps an operator from adding the
// parts to the total and reading twice the refusals there were.
func TestRefusalsAreDrawnUnderTheirTotal(t *testing.T) {
	lang := turkishCatalogue(t)
	label := func(key string) string { return lang.T("saglik.sayac." + key) }

	drawn := labelledCounters(lang, map[string]int64{
		heartbeat.CounterAccepted:             10,
		heartbeat.CounterRejected:             5,
		heartbeat.CounterRejectedUnknownSite:  3,
		heartbeat.CounterRejectedMalformed:    2,
		heartbeat.CounterRejectedInvalid:      0,
		heartbeat.CounterRejectedOverCapacity: 0,
		heartbeat.CounterRejectedOther:        0,
	})

	var total *healthCounter
	for i, c := range drawn {
		for _, key := range heartbeat.RejectionCounters {
			if c.Label == label(key) {
				t.Errorf("%q is drawn at the top level; a reason belongs under its total", c.Label)
			}
		}
		if c.Label == label(heartbeat.CounterRejected) {
			total = &drawn[i]
			continue
		}
		if len(c.Parts) != 0 {
			t.Errorf("%q has parts; only the refusals total is split", c.Label)
		}
	}
	if total == nil {
		t.Fatalf("the refusals total is not drawn: %+v", drawn)
	}
	if total.Value != 5 {
		t.Errorf("the refusals total reads %d, want 5", total.Value)
	}
	want := []healthCounter{
		{Label: label(heartbeat.CounterRejectedUnknownSite), Value: 3},
		{Label: label(heartbeat.CounterRejectedMalformed), Value: 2},
	}
	if !reflect.DeepEqual(total.Parts, want) {
		t.Errorf("the parts under the total are %+v, want %+v", total.Parts, want)
	}
}

// TestARowFromBeforeTheSplitStandsAlone: a beacon older than the reasons
// reports only the total, and the page draws only the total - no empty
// list, no invented zeros for reasons that build never counted.
func TestARowFromBeforeTheSplitStandsAlone(t *testing.T) {
	lang := turkishCatalogue(t)
	drawn := labelledCounters(lang, map[string]int64{
		heartbeat.CounterAccepted: 10,
		heartbeat.CounterRejected: 5,
	})
	for _, c := range drawn {
		if c.Parts != nil {
			t.Errorf("%q has parts %+v from a row that reported none", c.Label, c.Parts)
		}
	}
}
