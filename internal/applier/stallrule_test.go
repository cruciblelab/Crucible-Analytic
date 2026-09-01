//go:build integration

package applier

import (
	"testing"
	"time"
)

// The stall rule, against numbers that were actually observed.
//
// # Why this exists
//
// The rule used to be a switch inline in the measurement, which meant
// the only way to learn what it would say about a given run was to
// produce that run - and the runs that matter are the ones a development
// machine does not reproduce. Two of them are below, and both were
// failures of the *rule* rather than of the code it watches.
//
// A threshold nobody can exercise except by getting lucky is a threshold
// that gets tuned by whoever is looking at a red build.

const ms = time.Millisecond

func TestTheStallRuleAgreesWithWhatWasMeasured(t *testing.T) {
	for _, c := range []struct {
		name                      string
		during, baseline, upgrade time.Duration
		want                      stallVerdict
		why                       string
	}{
		{
			name:   "the development machine, warm",
			during: 5*ms + 324*time.Microsecond, baseline: 13*ms + 947*time.Microsecond,
			upgrade: 44*ms + 767*time.Microsecond,
			want:    stallFine,
			why: "the ordinary case: during is faster than at rest, which is what " +
				"the header's headline says and what most runs look like",
		},
		{
			// Measured 2026-09-01 on the development machine, and the run
			// that showed the old rule was wrong on more than CI.
			name:   "writers queueing behind one schema file",
			during: 393 * ms, baseline: 8*ms + 473*time.Microsecond, upgrade: 461 * ms,
			want: stallFine,
			why: "46x the at-rest worst and well over the 250ms minimum - the old " +
				"rule failed this - but inside the 461ms the upgrade itself took. " +
				"A writer that arrives while a schema file holds its ShareLock " +
				"waits for the rest of that file, which is the accepted cost of " +
				"applying one",
		},
		{
			// Measured on a CI runner, 2026-09-01, run 33503582577. The
			// same commit passed on main minutes earlier.
			name:   "a slow CI runner",
			during: 250*ms + 732*time.Microsecond, baseline: 41*ms + 663*time.Microsecond,
			upgrade: 639*ms + 484*time.Microsecond,
			want:    stallFine,
			why: "failed the old rule by 0.7ms. Every probe on that run was 5-8x " +
				"its at-rest worst and the upgrade took 639ms against 47ms here: " +
				"a machine thirteen times slower, not a lock regression",
		},
		{
			name:   "a wait longer than the upgrade that supposedly caused it",
			during: 900 * ms, baseline: 10 * ms, upgrade: 400 * ms,
			want: stallDisproportionate,
			why: "the sensitive half's whole job. Under the 2s ceiling, so nothing " +
				"else would report it, and a query cannot have been queueing " +
				"behind an upgrade for longer than the upgrade lasted",
		},
		{
			name:   "a rewriting ALTER",
			during: 8 * time.Second, baseline: 10 * ms, upgrade: 9 * time.Second,
			want: stallOverCeiling,
			why: "the ceiling comes first even though the wait is inside the " +
				"window: at eight seconds the customer's dashboard has stopped, " +
				"and why it stopped is a second question",
		},
		{
			name:   "slow but proportionate on a fast upgrade",
			during: 300 * ms, baseline: 200 * ms, upgrade: 40 * ms,
			want: stallFine,
			why: "over the 250ms minimum and over the window, but only 1.5x the " +
				"at-rest worst - the machine was slow, and the ratio is what says so",
		},
		{
			name:   "just under the minimum on a trivially fast upgrade",
			during: 240 * ms, baseline: 5 * ms, upgrade: 10 * ms,
			want: stallFine,
			why: "48x at rest and well over the window, and still not worth a word: " +
				"the absolute minimum is what stops this test reporting a quarter " +
				"of a second as an outage",
		},
		{
			name:   "just over the minimum on a trivially fast upgrade",
			during: 260 * ms, baseline: 5 * ms, upgrade: 10 * ms,
			want: stallDisproportionate,
			why: "the other side of the same line, so the minimum is a threshold " +
				"rather than a number nothing ever crosses",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := judgeStall(c.during, c.baseline, c.upgrade)
			if got != c.want {
				t.Errorf("judgeStall(during=%v, at rest=%v, upgrade=%v) = %v, want %v.\n%s",
					c.during, c.baseline, c.upgrade, got, c.want, c.why)
			}
		})
	}
}

func (v stallVerdict) String() string {
	switch v {
	case stallOverCeiling:
		return "over the ceiling"
	case stallDisproportionate:
		return "disproportionate"
	default:
		return "fine"
	}
}
