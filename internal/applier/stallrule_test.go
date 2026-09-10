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
// machine does not reproduce. Six of them are below, and four were
// failures of the *rule* rather than of the code it watches.
//
// A threshold nobody can exercise except by getting lucky is a threshold
// that gets tuned by whoever is looking at a red build.
//
// # What "tail" is here, and where each case's tail comes from
//
// tail is how long the worst query went on running after the applier
// returned - see the header of downtime_integration_test.go for why that
// is the only part of a query the upgrade cannot be blamed for.
//
// Four of the six real runs below left no tail in their logs, because
// nothing was measuring one at the time; the log line prints it now, and
// the last two are the first runs taken afterwards. So for those four the
// during/at-rest/window figures are quoted and the tail is *derived*,
// with the derivation written into that case's reason. That is the honest
// half of this table, and it is why two cases share one run's numbers and
// differ only in the tail: one asks what the rule says about the shape
// that run must have had, the other what it would say if the tail had
// been real.

const ms = time.Millisecond

func TestTheStallRuleAgreesWithWhatWasMeasured(t *testing.T) {
	for _, c := range []struct {
		name                            string
		during, tail, baseline, upgrade time.Duration
		want                            stallVerdict
		why                             string
	}{
		{
			name:   "the development machine, warm",
			during: 5*ms + 324*time.Microsecond, tail: 0,
			baseline: 13*ms + 947*time.Microsecond,
			upgrade:  44*ms + 767*time.Microsecond,
			want:     stallFine,
			why: "the ordinary case: during is faster than at rest, which is what " +
				"the header's headline says and what most runs look like. Nothing " +
				"outlived a 44ms upgrade, so there is no tail to have",
		},
		{
			// Measured 2026-09-01 on the development machine, and the run
			// that showed the old rule was wrong on more than CI.
			name:   "writers queueing behind one schema file",
			during: 393 * ms, tail: 0,
			baseline: 8*ms + 473*time.Microsecond, upgrade: 461 * ms,
			want: stallFine,
			why: "46x the at-rest worst and well over the 250ms minimum - the very " +
				"first rule failed this - but the writer was let go inside the 461ms " +
				"the upgrade took, so it left no tail. A writer that arrives while a " +
				"schema file holds its ShareLock waits for the rest of that file, " +
				"which is the accepted cost of applying one. The window floor also " +
				"passed this run; the tail passes it for the reason rather than the " +
				"coincidence",
		},
		{
			// Measured 2026-09-10 on the development machine, under
			// -race, and one of only two rows here whose tail is an
			// observation rather than a derivation: it is the first run
			// taken after the log line started printing one.
			name:   "a writer released by the applier's own commit",
			during: 413*ms + 820*time.Microsecond, tail: 304*time.Microsecond + 911*time.Nanosecond,
			baseline: 298*ms + 743*time.Microsecond, upgrade: 458*ms + 307*time.Microsecond,
			want: stallFine,
			why: "the sentence the whole rule rests on, measured instead of argued. " +
				"The insert waited 413.82ms inside a 458.31ms window and then ran " +
				"for 305 microseconds - it was still in flight when the applier let " +
				"go, and finished a third of a millisecond later. A blocked query's " +
				"tail is its own work, and its own work here is three ten-thousandths " +
				"of the wait",
		},
		{
			// Measured 2026-09-10, the same probe, a run where the
			// machine was not busy - which is what makes this the one
			// that shows the tail is not merely small when everything
			// is small.
			name:   "unmistakably blocked, and the tail still says nothing happened",
			during: 223*ms + 236*time.Microsecond, tail: 201*time.Microsecond + 318*time.Nanosecond,
			baseline: 5*ms + 747*time.Microsecond, upgrade: 268*ms + 634*time.Microsecond,
			want: stallFine,
			why: "39x its own at-rest worst, which is the kind of number the ratio " +
				"was written for, and it is not a regression: the query was blocked " +
				"and released inside the window, and its tail is 201 microseconds. " +
				"On the same run the collector managed 132.32ms against 4.71ms at " +
				"rest with a tail of 370 microseconds. The ratio on the duration " +
				"would report both; on the tail it reports neither",
		},
		{
			// Measured on a CI runner, 2026-09-01, run 33503582577. The
			// same commit passed on main minutes earlier.
			name:   "a slow CI runner",
			during: 250*ms + 732*time.Microsecond, tail: 0,
			baseline: 41*ms + 663*time.Microsecond,
			upgrade:  639*ms + 484*time.Microsecond,
			want:     stallFine,
			why: "failed the 250ms floor by 0.7ms. Every probe on that run was 5-8x " +
				"its at-rest worst and the upgrade took 639ms against 47ms here: " +
				"a machine thirteen times slower, not a lock regression. A 250ms " +
				"query inside a 639ms window ends before the applier does",
		},
		{
			// Measured on a CI runner, 2026-09-10, run 34426623555, on the
			// branch. The same tree passed on the other ref four minutes
			// earlier, and this is the run that took the window out of the
			// floor.
			name:   "the CI run the window floor could not read",
			during: 359*ms + 921*time.Microsecond, tail: 48*ms + 469*time.Microsecond,
			baseline: 48*ms + 469*time.Microsecond, upgrade: 282*ms + 806*time.Microsecond,
			want: stallFine,
			why: "the window floor failed this by arithmetic and not by evidence: " +
				"359.92ms is longer than the 282.81ms window, and it has to be. " +
				"A query the upgrade holds is let go when the upgrade lets go, so " +
				"from inside the window it can only reach 282.81 + its own work. " +
				"The tail here is that whole own-work bound - 48.47ms, this " +
				"machine's at-rest worst, the largest a held query could carry - " +
				"and it is still nowhere near the 250ms minimum. If the sample " +
				"began before the window instead, no upgrade was holding it and " +
				"it has no tail at all: both readings pass, which is the point",
		},
		{
			// The same run's during and at-rest, with a tail that was not
			// there, because a rule that passes a shape has to be shown
			// still failing it for the right reason.
			name:   "that run's numbers, if the query really had outlived the upgrade",
			during: 359*ms + 921*time.Microsecond, tail: 900 * ms,
			baseline: 48*ms + 469*time.Microsecond, upgrade: 282*ms + 806*time.Microsecond,
			want: stallDisproportionate,
			why: "constructed, not observed: the same probe, still running 900ms " +
				"after the applier returned. The upgrade had let go of everything " +
				"it held, so those 900ms are the machine's or a lock nobody " +
				"declared - 18x what this machine manages at rest either way. " +
				"Without this case the line above would only prove the rule can " +
				"be quiet",
		},
		{
			name:   "a rewriting ALTER",
			during: 8 * time.Second, tail: 0, baseline: 10 * ms, upgrade: 9 * time.Second,
			want: stallOverCeiling,
			why: "the ceiling comes first even though the query was released before " +
				"the applier returned: at eight seconds the customer's dashboard " +
				"has stopped, and why it stopped is a second question",
		},
		{
			// The pair below straddles worstAcceptableStall, and it is
			// here because a mutation asked for it too: the ceiling was
			// 2s and moving it to 3s changed nothing this table said.
			// Eight seconds is over any ceiling anyone would pick, so
			// the only case exercising it did not pin it.
			//
			// Written as literal milliseconds rather than as
			// worstAcceptableStall +/- 1ms on purpose. A case that moves
			// with the constant cannot hold it still.
			name:   "a whisker over two seconds",
			during: 2001 * ms, tail: 0, baseline: 10 * ms, upgrade: 2500 * ms,
			want: stallOverCeiling,
			why: "released inside a 2.5s upgrade window, so no tail and nothing the " +
				"sensitive half would say - and still a failure, because two seconds " +
				"is where a person watching a dashboard stops believing it is " +
				"loading. The number is a decision about people, not about locks",
		},
		{
			name:   "a whisker under two seconds",
			during: 1999 * ms, tail: 0, baseline: 10 * ms, upgrade: 2500 * ms,
			want: stallFine,
			why: "the other side of the same line, two milliseconds away. Under the " +
				"ceiling with no tail there is nothing to report, and that is the " +
				"cost of the ceiling being loose - stated in the header, and the " +
				"reason the sensitive half exists at all",
		},
		{
			name:   "slow but proportionate on a fast upgrade",
			during: 300 * ms, tail: 260 * ms, baseline: 200 * ms, upgrade: 40 * ms,
			want: stallFine,
			why: "the tail is over the 250ms minimum - a 300ms query on a 40ms " +
				"upgrade is nearly all tail - but only 1.3x the at-rest worst, and " +
				"the floor is 4x. The machine was slow, and the ratio is what says so",
		},
		{
			name:   "just under the minimum on a trivially fast upgrade",
			during: 250 * ms, tail: 240 * ms, baseline: 5 * ms, upgrade: 10 * ms,
			want: stallFine,
			why: "48x at rest and almost entirely outside a 10ms window, and still " +
				"not worth a word: the absolute minimum is what stops this test " +
				"reporting a quarter of a second as an outage",
		},
		{
			name:   "just over the minimum on a trivially fast upgrade",
			during: 270 * ms, tail: 260 * ms, baseline: 5 * ms, upgrade: 10 * ms,
			want: stallDisproportionate,
			why: "the other side of the same line, so the minimum is a threshold " +
				"rather than a number nothing ever crosses",
		},
		{
			// The two rows below straddle the ratio rather than the
			// minimum, and they are here because a mutation asked for
			// them: comparedToRest 4 -> 5 survived this table. Nothing
			// in it had a tail between four and five times its own
			// at-rest worst, so the constant could have been any number
			// above four and every case would still have passed.
			//
			// 100ms at rest is a CI runner under load, and the point at
			// which the ratio takes over from the 250ms minimum: 4x100ms
			// is 400ms, above it.
			name:   "four and a half times what the machine manages at rest",
			during: 500 * ms, tail: 450 * ms, baseline: 100 * ms, upgrade: 20 * ms,
			want: stallDisproportionate,
			why: "the ratio is what decides this one - 450ms is over the 250ms " +
				"minimum either way - and at five times it would pass. That is " +
				"what makes comparedToRest a threshold rather than a number " +
				"nothing is ever weighed against",
		},
		{
			name:   "three and a half times, on the same machine",
			during: 400 * ms, tail: 350 * ms, baseline: 100 * ms, upgrade: 20 * ms,
			want: stallFine,
			why: "the other side of the same line. 350ms is well over the minimum " +
				"and still only 3.5x, which the header calls a slow machine rather " +
				"than a lock - and a ratio of three would report it",
		},
		{
			// Measured 2026-09-08, in a container running the whole
			// integration suite in parallel. Three probes reported this
			// same shape, and the third observation that made the rule
			// what it is.
			name:   "a machine already over the ceiling at rest",
			during: 4*time.Second + 370*ms, tail: 0,
			baseline: 3*time.Second + 957*ms,
			upgrade:  4*time.Second + 383*ms,
			want:     stallCeilingUnmeasurable,
			why: "1.1x the at-rest worst and gone before the applier returned: " +
				"nothing was blocked. The old rule fired the absolute ceiling and " +
				"printed a diagnosis about a heavy lock in the schema files, which " +
				"was not there - the machine was",
		},
		{
			name:   "something still holding on that same starved machine",
			during: 30 * time.Second, tail: 25*time.Second + 617*ms,
			baseline: 3*time.Second + 957*ms,
			upgrade:  4*time.Second + 383*ms,
			want:     stallDisproportionate,
			why: "the case that stops the line above from being a way to pass. " +
				"Where the ceiling cannot speak the tail still can: a query running " +
				"25.6s after the applier returned is holding on something the " +
				"upgrade no longer has, and 6.5x an at-rest worst of 3.96s is not " +
				"a slow container either",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := judgeStall(c.during, c.tail, c.baseline)
			if got != c.want {
				t.Errorf("judgeStall(during=%v, tail=%v, at rest=%v) = %v, want %v "+
					"(upgrade window that run: %v).\n%s",
					c.during, c.tail, c.baseline, got, c.want, c.upgrade, c.why)
			}
		})
	}
}

// Below the ceiling, how long the query took is not the rule's business.
//
// This is the mistake the run above was made of, and it is the one
// anybody re-deriving the sensitive half would make again: a query took
// 359.92ms, that is a long time, therefore say something. The whole
// finding is that the duration cannot be attributed - only the tail can -
// so the sensitive half must not read the duration at all.
//
// A signature says so for today. This says so under a sweep: hold the
// machine and the tail still, move the query's duration across the entire
// range the ceiling leaves to the sensitive half, and the verdict must
// not move. Any floor put back on the duration - the window, a constant,
// a ratio - flips one of these two rows somewhere in the sweep.
func TestBelowTheCeilingTheRuleReadsTheTailAndNotTheDuration(t *testing.T) {
	// The machine from CI run 34426623555, so the floor is the 250ms
	// minimum: 4x48.47ms is 193.88ms, under it.
	const baseline = 48*ms + 469*time.Microsecond

	for _, row := range []struct {
		name string
		tail time.Duration
		want stallVerdict
	}{
		{
			name: "a tail under the floor stays quiet however long the query ran",
			tail: 48*ms + 469*time.Microsecond,
			want: stallFine,
		},
		{
			name: "a tail over the floor speaks however short the query ran",
			tail: 300 * ms,
			want: stallDisproportionate,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			// From a query faster than anything measured to one a
			// millisecond under the ceiling. The steps are uneven on
			// purpose: 250ms and the CI run's own 359.92ms are the two
			// numbers a reintroduced floor would most likely sit at.
			for _, during := range []time.Duration{
				1 * ms, 100 * ms, 249 * ms, 250 * ms, 251 * ms,
				359*ms + 921*time.Microsecond, 500 * ms, 1 * time.Second,
				worstAcceptableStall - 1*ms,
			} {
				if during < row.tail {
					continue // a tail cannot outlast the query that carries it
				}
				if got := judgeStall(during, row.tail, baseline); got != row.want {
					t.Errorf("a query of %v with a tail of %v on a machine whose "+
						"at-rest worst is %v was judged %v, and every other duration "+
						"in this sweep gave %v.\nBelow the %v ceiling the duration is "+
						"not evidence: it holds time the upgrade was entitled to. "+
						"Only the tail is time the upgrade had already let go of",
						during, row.tail, baseline, got, row.want, worstAcceptableStall)
				}
			}
		})
	}
}

// Which part of a sample the upgrade can be asked about.
//
// tailAfter is where the attribution argument lives, and it has two ways
// of saying "nothing here is the upgrade's": the query finished while the
// upgrade still held things, or the query was already running before the
// upgrade started at all. They are different sentences and a machine
// produces them at different times, so they are asked here rather than
// waited for.
//
// The times are relative to the window, written out so a case can be read
// without arithmetic: the upgrade runs from 0 to 500ms.
func TestOnlyTheTimeAfterTheUpgradeReturnedIsTheUpgradesToAnswerFor(t *testing.T) {
	var (
		began = time.Date(2026, 9, 10, 7, 18, 29, 0, time.UTC)
		ended = began.Add(500 * ms)
	)
	at := func(d time.Duration) time.Time { return began.Add(d) }

	for _, c := range []struct {
		name       string
		start, end time.Duration
		want       time.Duration
		why        string
	}{
		{
			name:  "started and ended inside the window",
			start: 100 * ms, end: 400 * ms, want: 0,
			why: "the upgrade was still holding things when this finished, so all " +
				"300ms of it is the queueing this design accepts",
		},
		{
			name:  "started inside, still running when the applier returned",
			start: 100 * ms, end: 800 * ms, want: 300 * ms,
			why: "released at 500ms at the earliest and running until 800ms: the " +
				"300ms after the upgrade let go is the part it cannot be blamed for",
		},
		{
			name:  "ended exactly as the applier returned",
			start: 100 * ms, end: 500 * ms, want: 0,
			why: "the boundary on the near side. Zero tail rather than a zero-length " +
				"one, and nothing to report either way",
		},
		{
			name:  "started exactly as the applier was called",
			start: 0, end: 700 * ms, want: 200 * ms,
			why: "the boundary on the other side. RunOnce had been called, so the " +
				"DDL is a candidate for what this query waited on, and it kept " +
				"running 200ms past the point where there was nothing left to wait for",
		},
		{
			name:  "started before the applier was called",
			start: -50 * ms, end: 700 * ms, want: 0,
			why: "the claim this function exists for. This query took its locks at " +
				"-50ms, before RunOnce was called, so the DDL queued behind IT - and " +
				"its 750ms is the machine's or the query's, not the upgrade's. " +
				"Without this, the longest sample on a loaded machine is charged to " +
				"an upgrade that was itself waiting for it",
		},
		{
			name:  "started long before and ended long after",
			start: -3 * time.Second, end: 4 * time.Second, want: 0,
			why: "the same claim at a scale where getting it wrong reports a " +
				"three-and-a-half second tail and fails the run. The size of the " +
				"number is not what decides whether the upgrade can be asked about it",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := tailAfter(at(c.start), at(c.end), began, ended)
			if got != c.want {
				t.Errorf("a sample running %v..%v against a window of 0..%v has a "+
					"tail of %v, want %v.\n%s",
					c.start, c.end, ended.Sub(began), got, c.want, c.why)
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
	case stallCeilingUnmeasurable:
		return "the ceiling could not apply"
	default:
		return "fine"
	}
}

// The paced probes' query floor, against the run that made it what it is.
//
// # Why this is a table and not left to the machine
//
// The floor's job is to separate "the probe ran" from "the probe never
// started". It used to compute what a probe should manage from the pause
// alone - baselinePeriod/pause/4 - which is what a probe would manage if
// the query itself were free.
//
// It is not free, and on a starved machine it is not even close. So the
// floor went red on a run where nothing was wrong with the product,
// which is the same mistake the stall rule's own floor made and in the
// same shape: a threshold divided by a number that does not bound the
// thing it divides.
//
// A machine cannot be asked to be starved on demand, so the arithmetic
// is asked here instead.
func TestThePacedFloorAsksWhatTheMachineCanActuallyManage(t *testing.T) {
	// The pause the two paced probes use. Named so a case cannot drift
	// from the fixture it is about.
	const paced = 20 * time.Millisecond

	for _, c := range []struct {
		name            string
		pause, baseline time.Duration
		wantAtMost      int
		wantAtLeast     int
		why             string
	}{
		{
			// Measured 2026-09-10, the gate running every suite in
			// parallel in a container. The probe managed 17 queries and
			// the old floor asked for 18.
			name:  "the starved container that made this a function",
			pause: paced, baseline: 554*ms + 796*time.Microsecond,
			wantAtMost:  17,
			wantAtLeast: 2,
			why: "17 queries was the whole run on that machine, so any floor above " +
				"it reports a stopped probe that was running as fast as the " +
				"database would answer. And it must not fall to zero: a floor of " +
				"nought is the check not existing",
		},
		{
			// The same probe on a warm machine, from the runs taken
			// straight after the fix.
			name:  "a warm machine, where the floor has to keep its teeth",
			pause: paced, baseline: 5 * ms,
			wantAtLeast: 10,
			wantAtMost:  20,
			why: "the probe managed 75-80 queries here. A floor near fifteen still " +
				"reports a probe that stopped after three, which is what this " +
				"check is for - a floor of two would pass that silently",
		},
		{
			name:  "an unmeasurably fast machine",
			pause: paced, baseline: 0,
			wantAtLeast: 15, wantAtMost: 20,
			why: "with a free query the floor is the old arithmetic, which was " +
				"right for the machines it was written on. The change is not a " +
				"loosening; it is the same number where the assumption holds",
		},
		{
			name:  "a machine slower than the whole baseline period",
			pause: paced, baseline: 30 * time.Second,
			wantAtLeast: 2, wantAtMost: 2,
			why: "one cycle does not fit in the baseline at all, so the only " +
				"honest floor is the smallest one that still means something. Two " +
				"rather than one, because one sample cannot show a probe that " +
				"stalled after its first query",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := pacedFloor(c.pause, c.baseline)
			if got < c.wantAtLeast || got > c.wantAtMost {
				t.Errorf("pacedFloor(pause=%v, at rest=%v) = %d, want %d..%d.\n%s",
					c.pause, c.baseline, got, c.wantAtLeast, c.wantAtMost, c.why)
			}
		})
	}
}

// TestTheOldPacedFloorAndTheNewOneDisagreeOnTheRunThatFailed.
//
// Without this the table above proves only that some function returns
// some numbers. The fix is a *change*, and a change has to be shown
// changing something: the old arithmetic asked the starved container for
// 18 queries and it had managed 17.
//
// Written as the old expression rather than as the number 18, so that a
// reader can see the two side by side and so that the claim is about the
// arithmetic instead of about a constant somebody could edit.
func TestTheOldPacedFloorAndTheNewOneDisagreeOnTheRunThatFailed(t *testing.T) {
	const (
		pause    = 20 * time.Millisecond
		atRest   = 554*ms + 796*time.Microsecond
		measured = 17 // queries the probe actually managed
	)
	old := int(baselinePeriod/pause) / 4
	now := pacedFloor(pause, atRest)

	if old <= measured {
		t.Fatalf("the old floor was %d against %d queries, so it would not have "+
			"failed that run and this whole change is about something else",
			old, measured)
	}
	if now > measured {
		t.Errorf("the new floor is %d against the %d queries that machine managed, "+
			"so the run that prompted this would still be red", now, measured)
	}
	t.Logf("the run that failed: %d queries, old floor %d, new floor %d",
		measured, old, now)
}
