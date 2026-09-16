package api

import (
	"math"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/storage"
)

// The margin the response carries is the margin the stored precision
// actually gives.
//
// # Why the relationship is the test and not either number
//
// Both constants are reasonable on their own, and that is exactly how
// this class of defect survives: R1 shipped a bot score whose strongest
// signal was worth 30 against a cutoff of 50, two sensible numbers whose
// relationship was wrong. Here the pair is a precision the collector
// stores at and a margin the panel prints. Halve the registers to save
// disk and the printed margin stays 0,41 % while the real one doubles -
// a page quoting a precision the data does not have.
//
// 1,04/sqrt(m) is HyperLogLog's documented standard error, and the
// measured figure stayed inside it (0,36 % against 0,41 % at this
// precision, twenty draws, dense regime). The constant is the theory
// because the theory is the bound; quoting the measurement would be
// quoting the luckier of the two.
func TestTheReportedMarginMatchesTheStoredPrecision(t *testing.T) {
	want := 1.04 / math.Sqrt(float64(storage.SketchPrecision))
	if diff := math.Abs(VisitorSketchRelativeError - want); diff > 0.00005 {
		t.Errorf("the response reports a standard error of %.5f, and %d registers "+
			"give %.5f (1,04/sqrt(m)).\n"+
			"Whichever of the two moved, the other has to move with it: the panel "+
			"prints this number beside an estimate the collector built at that "+
			"precision, and a margin that does not belong to the data is worse than "+
			"no margin at all.",
			VisitorSketchRelativeError, storage.SketchPrecision, want)
	}
}

// The three visitor figures are consistent even when the two estimates
// are not.
//
// # Why this is a unit test with a table
//
// Because the interesting row cannot be produced on demand. The bot
// estimate lands above the unique estimate only when almost every
// address in a range reached the cutoff *and* the two sketches happen to
// err in opposite directions - a coincidence no fixture can be relied on
// to arrange. Reached through a database, this branch would be a branch
// nobody has ever run.
//
// The failure it prevents is a negative human count on a customer's
// dashboard: a number no reader can interpret, produced from two numbers
// that were both inside their margin.
func TestTheVisitorSplitIsAlwaysConsistent(t *testing.T) {
	for _, tc := range []struct {
		name               string
		unique, bot        int
		wantBot, wantHuman int
	}{
		{"the ordinary case", 1000, 400, 400, 600},
		{"every address is a bot", 1000, 1000, 1000, 0},
		{"no bots at all", 1000, 0, 0, 1000},
		// The reason the clamp exists. Two estimates of the same set,
		// four apart.
		{"the bot estimate overshoots", 1000, 1004, 1000, 0},
		{"an empty range", 0, 0, 0, 0},
		// A bot count from an empty unique count cannot happen through
		// the query - rollup() of the same rows produces both - but the
		// arithmetic must not invent a negative out of it either.
		{"bots with no addresses", 0, 7, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bot, human := splitVisitors(tc.unique, tc.bot)
			if bot != tc.wantBot || human != tc.wantHuman {
				t.Errorf("splitVisitors(%d, %d) = %d, %d; want %d, %d",
					tc.unique, tc.bot, bot, human, tc.wantBot, tc.wantHuman)
			}
			if human < 0 || bot < 0 {
				t.Errorf("a negative figure: bot %d, human %d", bot, human)
			}
			if bot+human != tc.unique {
				t.Errorf("bot %d + human %d = %d, but unique is %d; the page shows "+
					"all three", bot, human, bot+human, tc.unique)
			}
		})
	}
}

// Which part of a range the sketches may answer for.
//
// The cases that matter are the ones a customer's own day produces: a
// range that starts and ends at a local midnight contains no whole UTC
// day at all in most zones, and the read path has to notice rather than
// claim the days on either side of it.
func TestTheSketchSpanIsTheWholeDaysBehindTheWatermark(t *testing.T) {
	day := func(s string) time.Time {
		t.Helper()
		out, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	far := day("2030-01-01T00:00:00Z")

	for _, tc := range []struct {
		name             string
		from, to         time.Time
		watermark        time.Time
		wantOK           bool
		wantFrom, wantTo time.Time
	}{
		{
			name: "three whole UTC days",
			from: day("2026-03-01T00:00:00Z"), to: day("2026-03-04T00:00:00Z"),
			watermark: far, wantOK: true,
			wantFrom: day("2026-03-01T00:00:00Z"), wantTo: day("2026-03-04T00:00:00Z"),
		},
		{
			// Istanbul, one local day: 21:00 to 21:00 in UTC. It holds
			// no whole UTC day, so the sketches answer nothing and the
			// count answers everything - which is the common case for
			// the panel's shortest button and the reason a 24-hour
			// figure is still exact.
			name: "one local day holds no whole UTC day",
			from: day("2026-03-01T00:00:00+03:00"), to: day("2026-03-02T00:00:00+03:00"),
			watermark: far, wantOK: false,
		},
		{
			// Istanbul, three local days: the two ragged ends belong to
			// the raw table and the two whole days in the middle to the
			// sketches.
			name: "three local days hold two whole UTC days",
			from: day("2026-03-01T00:00:00+03:00"), to: day("2026-03-04T00:00:00+03:00"),
			watermark: far, wantOK: true,
			wantFrom: day("2026-03-01T00:00:00Z"), wantTo: day("2026-03-03T00:00:00Z"),
		},
		{
			name: "the watermark cuts the span short",
			from: day("2026-03-01T00:00:00Z"), to: day("2026-03-10T00:00:00Z"),
			watermark: day("2026-03-05T12:00:00Z"), wantOK: true,
			wantFrom: day("2026-03-01T00:00:00Z"), wantTo: day("2026-03-05T00:00:00Z"),
		},
		{
			name: "no watermark at all",
			from: day("2026-03-01T00:00:00Z"), to: day("2026-03-10T00:00:00Z"),
			wantOK: false,
		},
		{
			name: "the watermark is behind the range",
			from: day("2026-03-01T00:00:00Z"), to: day("2026-03-10T00:00:00Z"),
			watermark: day("2026-02-01T00:00:00Z"), wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from, to, ok := sketchSpan(tc.from, tc.to, tc.watermark)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (span %s .. %s)", ok, tc.wantOK,
					from.Format(time.RFC3339), to.Format(time.RFC3339))
			}
			if !ok {
				return
			}
			if !from.Equal(tc.wantFrom) || !to.Equal(tc.wantTo) {
				t.Errorf("span = %s .. %s, want %s .. %s",
					from.Format(time.RFC3339), to.Format(time.RFC3339),
					tc.wantFrom.Format(time.RFC3339), tc.wantTo.Format(time.RFC3339))
			}
			// Whatever the span is, it must lie inside the request and
			// behind the watermark. Asserted separately from the exact
			// bounds above, because this is the property a customer's
			// numbers depend on: a span reaching past either edge would
			// count rows twice or claim days nobody has materialized.
			if from.Before(tc.from) || to.After(tc.to) {
				t.Errorf("the span %s .. %s reaches outside the request %s .. %s",
					from.Format(time.RFC3339), to.Format(time.RFC3339),
					tc.from.Format(time.RFC3339), tc.to.Format(time.RFC3339))
			}
			if to.After(tc.watermark) {
				t.Errorf("the span ends at %s, past the watermark %s",
					to.Format(time.RFC3339), tc.watermark.Format(time.RFC3339))
			}
		})
	}
}

// A state row answers for the cutoff it was built at and no other.
func TestOnlyASketchBuiltAtTheAskedCutoffIsUsable(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name  string
		state sketchState
		asked int
		want  bool
	}{
		{"the cutoff matches", sketchState{Watermark: now, Cutoff: 50}, 50, true},
		{"another cutoff asked", sketchState{Watermark: now, Cutoff: 50}, 60, false},
		{"the product's cutoff moved", sketchState{Watermark: now, Cutoff: 40}, 50, false},
		{"no watermark", sketchState{Cutoff: 50}, 50, false},
		// Both zero is what a deployment with no sketch tables produces,
		// and a request may legitimately ask for cutoff 0 - every
		// address is a bot at that threshold. The zero watermark is what
		// must refuse it, not the cutoff comparison.
		{"nothing stored, cutoff zero asked", sketchState{}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.Usable(tc.asked); got != tc.want {
				t.Errorf("Usable(%d) on %+v = %v, want %v", tc.asked, tc.state, got, tc.want)
			}
		})
	}
}
