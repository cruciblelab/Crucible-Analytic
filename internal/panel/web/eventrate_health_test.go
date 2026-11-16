package web

import (
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
)

// The event rate's two counters on the Health page (W2b). The full table
// is a limit of the beacon that a deployment may never reach: reported at
// zero by every beacon that has one, drawn only once it moved. The drops
// are a refusal like the others: under "Rejected", by their own name.
func TestTheEventRateCountersAreDrawnWhereTheyBelong(t *testing.T) {
	lang := turkishCatalogue(t)
	untracked := lang.T("saglik.sayac." + heartbeat.CounterRateUntracked)
	rate := lang.T("saglik.sayac." + heartbeat.CounterRejectedRate)

	find := func(counters map[string]int64) (top map[string]int64, parts map[string]int64) {
		top, parts = map[string]int64{}, map[string]int64{}
		for _, c := range labelledCounters(lang, counters) {
			top[c.Label] = c.Value
			for _, p := range c.Parts {
				parts[p.Label] = p.Value
			}
		}
		return top, parts
	}

	top, _ := find(map[string]int64{heartbeat.CounterWritten: 10, heartbeat.CounterRateUntracked: 0})
	if _, drawn := top[untracked]; drawn {
		t.Error("a full table that never filled is drawn")
	}
	top, _ = find(map[string]int64{heartbeat.CounterWritten: 10, heartbeat.CounterRateUntracked: 7})
	if top[untracked] != 7 {
		t.Errorf("seven untracked events read %d", top[untracked])
	}

	top, parts := find(map[string]int64{
		heartbeat.CounterRejected: 5, heartbeat.CounterRejectedRate: 5,
	})
	if parts[rate] != 5 {
		t.Errorf("five rate refusals under %q read %d (parts %v)", lang.T("saglik.sayac."+heartbeat.CounterRejected), parts[rate], parts)
	}
	if _, onTop := top[rate]; onTop {
		t.Error("the rate refusals are drawn as a counter of their own, outside the total they are part of")
	}
}
