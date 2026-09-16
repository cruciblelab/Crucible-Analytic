//go:build integration

// O3's claims, against a real TimescaleDB with timescaledb_toolkit, on
// sketches a real collector role wrote.
//
// The fixture and the reasoning for a database of its own are in
// sketchdb_integration_test.go.

package api

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/scoring"
	"github.com/cruciblelab/crucible-analytic/internal/storage"
)

// A fixed base so every test in this file works in whole UTC days that
// are safely in the past - the refresh only materializes days that have
// already settled, and a day chosen relative to "now" would be half
// inside the settle margin for an hour after midnight.
func sketchBase() time.Time {
	return time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -20)
}

// The estimate and the count agree, and the estimate is the one the
// response says it is.
//
// # What this measures that a unit test cannot
//
// Three separate things have to be true at once for a number on this
// page to be right: the collector's INSERT has to build a sketch of the
// right set, the reader's rollup() has to merge the days without
// double-counting an address that appears on several, and the two sides
// have to agree on the *element type* of the sketch. That last one is
// not hypothetical - the first version of this measurement built the
// sketches from ip::text and read them with ip, and PostgreSQL rejected
// the merge with "mismatched types". Nothing in Go would have caught it.
func TestStore_RealToolkit_TheEstimateAgreesWithTheCount(t *testing.T) {
	f := newSketchFixture(t, "api-sketch-agree")
	base := sketchBase()

	// Six whole days, 400 addresses each, and the same 100 addresses
	// present on every day - so the union is genuinely smaller than the
	// sum of the days, which is the property that makes this table
	// necessary and the property a per-day sum would get wrong.
	const returning, fresh, days = 100, 300, 6
	var rows []sketchRow
	for d := 0; d < days; d++ {
		at := base.AddDate(0, 0, d).Add(9 * time.Hour)
		for i := 0; i < returning; i++ {
			rows = append(rows, sketchRow{IP: sketchIP(i), At: at, Score: 10})
		}
		for i := 0; i < fresh; i++ {
			rows = append(rows, sketchRow{
				IP: sketchIP(1000 + d*fresh + i), At: at, Score: 80})
		}
	}
	f.Seed(t, rows)

	report := f.Refresh(t)
	if report.Days != days {
		t.Fatalf("the refresh wrote %d sketches for %d days of rows", report.Days, days)
	}

	from, to := base, base.AddDate(0, 0, days)
	exact := f.Counted(t, from, to, scoring.BotCutoff)
	est := f.Estimated(t, from, to, scoring.BotCutoff)

	if exact.Method != VisitorCountExact || est.Method != VisitorCountEstimated {
		t.Fatalf("the two calls reported %q and %q; the test measures nothing unless "+
			"they are the two different branches", exact.Method, est.Method)
	}

	wantUnique := returning + fresh*days
	if exact.Unique != wantUnique {
		t.Fatalf("the exact count says %d addresses, the fixture seeded %d",
			exact.Unique, wantUnique)
	}
	// A union, not a sum: the returning addresses appear on all six days
	// and must be counted once.
	if sum := (returning + fresh) * days; exact.Unique >= sum {
		t.Fatalf("the fixture does not exercise a union: %d unique against %d "+
			"day-address pairs", exact.Unique, sum)
	}

	// The tolerance is the product's own margin with room for the fact
	// that a standard error is not a bound - three sigma, which for
	// 1.900 addresses is a handful either way. Written as the constant
	// times a factor rather than as a number, so a change to the
	// precision moves the assertion with it.
	const sigmas = 3
	tolerance := sigmas * VisitorSketchRelativeError
	for _, tc := range []struct {
		name       string
		got, exact int
	}{
		{"unique", est.Unique, exact.Unique},
		{"bot", est.Bot, exact.Bot},
		{"human", est.Human, exact.Human},
	} {
		if off := relative(tc.got, tc.exact); off > tolerance {
			t.Errorf("%s: estimated %d against an exact %d, off by %.3f%%, "+
				"tolerance %.3f%%", tc.name, tc.got, tc.exact, off*100, tolerance*100)
		}
	}

	// And the three still add up, which two independent estimates do not
	// guarantee on their own.
	if est.Bot+est.Human != est.Unique {
		t.Errorf("estimated bot %d + human %d = %d, but unique is %d. A page showing "+
			"all three would be showing a sum that does not sum.",
			est.Bot, est.Human, est.Bot+est.Human, est.Unique)
	}
}

// An address that was quiet on one day and noisy on another is a bot for
// the whole range, in both paths.
//
// # Why this is the load-bearing test of the whole design
//
// Because it is where the obvious design is wrong. The natural thing to
// store per day is "the humans" and "the bots", and the union of daily
// human sets is *not* the range's human set: an address below the cutoff
// on Monday and above it on Tuesday would land in both, and the page's
// three numbers would stop adding up. The bot direction survives -
// a maximum over a range is the maximum of the per-day maxima - and the
// human figure is therefore a subtraction rather than a third sketch.
//
// This test is what would fail if somebody added that third sketch.
func TestStore_RealToolkit_ABotOnAnyDayIsABotForTheRange(t *testing.T) {
	f := newSketchFixture(t, "api-sketch-cutoff")
	base := sketchBase()

	// One address, two days: well under the cutoff, then well over.
	// Plus one that is never over, so "everything is a bot" cannot pass.
	const quiet, noisy = 10, 90
	f.Seed(t, []sketchRow{
		{IP: sketchIP(1), At: base.Add(2 * time.Hour), Score: quiet},
		{IP: sketchIP(1), At: base.AddDate(0, 0, 1).Add(2 * time.Hour), Score: noisy},
		{IP: sketchIP(2), At: base.Add(3 * time.Hour), Score: quiet},
		{IP: sketchIP(2), At: base.AddDate(0, 0, 1).Add(3 * time.Hour), Score: quiet},
	})
	f.Refresh(t)

	from, to := base, base.AddDate(0, 0, 2)
	for _, tc := range []struct {
		name string
		got  visitors
	}{
		{"counted", f.Counted(t, from, to, scoring.BotCutoff)},
		{"estimated", f.Estimated(t, from, to, scoring.BotCutoff)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got.Unique != 2 || tc.got.Bot != 1 || tc.got.Human != 1 {
				t.Errorf("unique %d, bot %d, human %d; want 2, 1, 1.\n"+
					"Address 1 crossed the cutoff on the second day only. Over the "+
					"range it is one bot - a maximum over the range is the maximum of "+
					"the per-day maxima - and address 2 never crossed it. A human "+
					"figure built from a union of daily human sets would report 2 "+
					"humans here and a total of 3 people out of 2 addresses.",
					tc.got.Unique, tc.got.Bot, tc.got.Human)
			}
		})
	}
}

// An address seen both in a ragged end and in a materialized day is one
// address.
//
// # Why the ragged ends exist at all
//
// The sketches cover UTC days and the panel asks in the customer's zone,
// so a range that begins at local midnight begins mid-day in UTC. Those
// two partial days are read from the raw table and turned into sketches
// of their own before the merge, which is the only reason a daily grid
// can answer a question asked on a local one.
//
// The risk that buys is double counting, and it is invisible in a
// fixture whose edges hold addresses the middle does not. So this one
// puts the same address in all three parts.
func TestStore_RealToolkit_AnAddressInAnEdgeAndADayIsCountedOnce(t *testing.T) {
	f := newSketchFixture(t, "api-sketch-edges")
	base := sketchBase()

	// Istanbul's midnight is 21:00 the previous day in UTC, which is
	// what makes the two edges.
	zone, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Fatalf("Europe/Istanbul: %v", err)
	}
	from := base.In(zone).Truncate(24 * time.Hour).Add(-3 * time.Hour) // local midnight
	to := from.AddDate(0, 0, 4)

	// Three addresses in all four parts - the head edge, two whole days,
	// the tail edge - and one more in each edge alone.
	//
	// Both halves are needed and they catch opposite mistakes. The
	// shared three catch an edge counted *alongside* the days rather
	// than merged into them; the two lonely ones catch an edge dropped
	// altogether, which the shared three cannot see, because their
	// addresses are in the days too. The first version of this test had
	// only the shared three and would have passed with the edges gone.
	var rows []sketchRow
	head := from.Add(30 * time.Minute)
	tail := to.Add(-30 * time.Minute)
	for _, at := range []time.Time{
		head,                     // head edge, before the first whole day
		from.Add(26 * time.Hour), // inside a whole UTC day
		from.Add(50 * time.Hour), // inside the next whole UTC day
		tail,                     // tail edge, after the last whole day
	} {
		for i := 1; i <= 3; i++ {
			rows = append(rows, sketchRow{IP: sketchIP(i), At: at.UTC(), Score: 10})
		}
	}
	rows = append(rows,
		sketchRow{IP: sketchIP(91), At: head.UTC(), Score: 10},
		sketchRow{IP: sketchIP(92), At: tail.UTC(), Score: 10})
	f.Seed(t, rows)
	f.Refresh(t)

	est := f.Estimated(t, from, to, scoring.BotCutoff)
	if est.Method != VisitorCountEstimated {
		t.Fatalf("this range was answered %q; with whole UTC days inside it and a "+
			"watermark past them, the sketches should have answered", est.Method)
	}
	if est.Unique != 5 {
		t.Errorf("unique = %d, want 5.\n"+
			"Three addresses appear in both ragged ends and in both materialized "+
			"days, and one more lives in each edge alone. Fourteen is what adding "+
			"the parts would give, so anything above five means the edges are "+
			"counted alongside the days rather than merged into them; three means "+
			"the edges were not read at all and the two visitors who only ever "+
			"appeared there are missing from the range.", est.Unique)
	}
	// And the same question answered the other way, on the same rows, so
	// the assertion above is anchored to something rather than to 3.
	if exact := f.Counted(t, from, to, scoring.BotCutoff); exact.Unique != est.Unique {
		t.Errorf("the count says %d and the estimate says %d over the same range",
			exact.Unique, est.Unique)
	}
}

// Nothing is read past the watermark.
//
// A day the refresh has not reached yet must be answered from the raw
// table, not skipped. The failure this guards is the silent one: a
// watermark ahead of the sketches would make the dashboard show a range
// short of its last days, and nothing anywhere would say so.
func TestStore_RealToolkit_TheUnmaterializedTailComesFromTheRawTable(t *testing.T) {
	f := newSketchFixture(t, "api-sketch-watermark")
	base := sketchBase()

	// Four days of rows, each day with its own addresses.
	const perDay, days = 5, 4
	var rows []sketchRow
	for d := 0; d < days; d++ {
		for i := 0; i < perDay; i++ {
			rows = append(rows, sketchRow{
				IP: sketchIP(d*100 + i), At: base.AddDate(0, 0, d).Add(time.Hour), Score: 10})
		}
	}
	f.Seed(t, rows)
	f.Refresh(t)

	// Pull the watermark back by two days, as if the refresh had only got
	// that far - and make the sketches for those two days *disagree*
	// with the rows behind them.
	//
	// # Why the sketches have to be made wrong
	//
	// Because a correct sketch and the raw rows give the same answer, so
	// a read path that ignored the watermark entirely would pass. The
	// first version of this test left the rows correct and asserted the
	// total: the mutation "ignore the watermark" survived it, which is
	// the shape of a test that cannot see the thing it names.
	//
	// So the two days past the watermark are replaced with a sketch of a
	// single address nobody seeded. Now the two sources answer
	// differently and the assertion below can only be satisfied by
	// reading the raw table.
	cut := base.AddDate(0, 0, days-2)
	if _, err := f.Admin.Exec(context.Background(), `
		UPDATE visitor_sketch_state SET materialized_before = $2 WHERE site_id = $1`,
		f.Site, cut); err != nil {
		t.Fatalf("moving the watermark: %v", err)
	}
	if _, err := f.Admin.Exec(context.Background(), `
		UPDATE visitor_sketch
		   SET ips = (SELECT hyperloglog($3::int, v.a)
		                FROM (VALUES ('203.0.113.250'::inet)) AS v(a)),
		       bot_ips = NULL
		 WHERE site_id = $1 AND day >= $2`,
		f.Site, cut, storage.SketchPrecision); err != nil {
		t.Fatalf("rewriting the sketches past the watermark: %v", err)
	}
	var left int
	if err := f.Admin.QueryRow(context.Background(),
		`SELECT count(*) FROM visitor_sketch WHERE site_id = $1 AND day >= $2`,
		f.Site, cut).Scan(&left); err != nil {
		t.Fatalf("counting sketches past the watermark: %v", err)
	}
	if left != 2 {
		t.Fatalf("%d sketch rows sit past the moved watermark, want 2. Without rows "+
			"that disagree with the raw table, this test cannot tell a read path "+
			"that respects the watermark from one that ignores it.", left)
	}

	est := f.Estimated(t, base, base.AddDate(0, 0, days), scoring.BotCutoff)
	if est.Unique != perDay*days {
		t.Errorf("unique = %d, want %d.\n"+
			"The last two days are behind the watermark, so they have to come from "+
			"the raw table. A read path that merged the sketches anyway would find "+
			"one invented address there instead of %d real ones; a read path that "+
			"stopped at the watermark and read nothing further would report %d, and "+
			"a dashboard would show a quiet fortnight that never happened.",
			est.Unique, perDay*days, perDay*2, perDay*(days-2))
	}
}

// A cutoff the sketches were not built at falls back to the count.
//
// Two situations, one condition: a caller asking for its own threshold,
// and a product whose default has moved since the rows were written.
// Both mean the stored bot set is not the one the answer would be
// labelled with.
func TestStore_RealToolkit_AnotherCutoffIsCountedNotEstimated(t *testing.T) {
	f := newSketchFixture(t, "api-sketch-othercutoff")
	base := sketchBase()

	f.Seed(t, []sketchRow{
		{IP: sketchIP(1), At: base.Add(time.Hour), Score: 30},
		{IP: sketchIP(2), At: base.Add(time.Hour), Score: 60},
		{IP: sketchIP(3), At: base.AddDate(0, 0, 1).Add(time.Hour), Score: 60},
	})
	f.Refresh(t)

	from, to := base, base.AddDate(0, 0, 2)

	// The product's own cutoff: estimated, and the split is the stored
	// one.
	if got := f.Estimated(t, from, to, scoring.BotCutoff); got.Method != VisitorCountEstimated {
		t.Fatalf("at the default cutoff the answer came back %q", got.Method)
	}

	// Another cutoff: counted, and the split is the one that was asked
	// for rather than the one that was stored.
	other := f.Estimated(t, from, to, 20)
	if other.Method != VisitorCountExact {
		t.Errorf("a request for cutoff 20 was answered %q. The stored bot sketch "+
			"means \"reached %d\"; serving it under another label would report one "+
			"threshold's bots as another's.", other.Method, scoring.BotCutoff)
	}
	if other.Bot != 3 {
		t.Errorf("at cutoff 20 the bot count is %d, want 3 - every seeded address "+
			"scored at least 30, so all of them are bots at that threshold. Getting "+
			"the stored split back would give 2.", other.Bot)
	}
}

// A changed cutoff rebuilds from the start of the site's history rather
// than leaving the old rows in place.
//
// # Why this needs a test and not a comment
//
// Because the harmless-looking alternative is to leave the watermark
// where it is and only rewrite the days the recheck window reaches. The
// sketches before that point would then keep meaning the old cutoff
// forever, the read path would refuse them forever, and the fast path
// would be permanently gone with nothing in any log saying why.
func TestStore_RealToolkit_AChangedCutoffRebuildsTheWholeHistory(t *testing.T) {
	f := newSketchFixture(t, "api-sketch-rebuild")
	base := sketchBase()

	const days = 5
	var rows []sketchRow
	for d := 0; d < days; d++ {
		rows = append(rows, sketchRow{
			IP: sketchIP(d), At: base.AddDate(0, 0, d).Add(time.Hour), Score: 10})
	}
	f.Seed(t, rows)
	f.Refresh(t)

	// Stand in for a product whose cutoff moved: rewrite the state row
	// with another one. The sketch rows are left alone, which is exactly
	// the state a real cutoff change leaves behind.
	if _, err := f.Admin.Exec(context.Background(), `
		UPDATE visitor_sketch_state SET bot_score_min = $2 WHERE site_id = $1`,
		f.Site, scoring.BotCutoff+7); err != nil {
		t.Fatalf("changing the stored cutoff: %v", err)
	}

	// Before the next refresh the read path must refuse the sketches.
	if got := f.Estimated(t, base, base.AddDate(0, 0, days), scoring.BotCutoff); got.Method != VisitorCountExact {
		t.Errorf("with a stored cutoff of %d the answer came back %q; sketches built "+
			"at another threshold must not be merged", scoring.BotCutoff+7, got.Method)
	}

	report := f.Refresh(t)
	if !report.Rebuilt {
		t.Error("the refresh did not report a rebuild. A cutoff change is not an " +
			"ordinary catch-up and the log line for it is the only way an operator " +
			"learns why a day's sketches were rewritten.")
	}
	if !report.From.Equal(base) {
		t.Errorf("the rebuild started at %s; the site's oldest row is at %s, and a "+
			"rebuild that starts anywhere later leaves sketches that mean the old "+
			"cutoff", report.From.Format(time.RFC3339), base.Format(time.RFC3339))
	}
	if got := f.Estimated(t, base, base.AddDate(0, 0, days), scoring.BotCutoff); got.Method != VisitorCountEstimated {
		t.Errorf("after the rebuild the answer is still %q, so the fast path did not "+
			"come back", got.Method)
	}
}

// The watermark never moves ahead of the sketches, and a catch-up that
// stops short says so.
func TestStore_RealToolkit_TheWatermarkFollowsTheSketches(t *testing.T) {
	f := newSketchFixture(t, "api-sketch-catchup")
	base := sketchBase()

	f.Seed(t, []sketchRow{
		{IP: sketchIP(1), At: base.Add(time.Hour), Score: 10},
		{IP: sketchIP(2), At: base.AddDate(0, 0, 1).Add(time.Hour), Score: 10},
	})
	report := f.Refresh(t)

	var watermark time.Time
	var cutoff int
	if err := f.Admin.QueryRow(context.Background(), `
		SELECT materialized_before, bot_score_min FROM visitor_sketch_state
		 WHERE site_id = $1`, f.Site).Scan(&watermark, &cutoff); err != nil {
		t.Fatalf("reading the watermark: %v", err)
	}
	if !watermark.Equal(report.Through) {
		t.Errorf("the stored watermark is %s and the refresh reported %s",
			watermark.Format(time.RFC3339), report.Through.Format(time.RFC3339))
	}
	if cutoff != scoring.BotCutoff {
		t.Errorf("the stored cutoff is %d, want %d", cutoff, scoring.BotCutoff)
	}

	// Every sketch row is behind the watermark. The opposite - a row at
	// or after it - would not be wrong; it would be unread. A row
	// *missing* from behind it is the failure that makes the read path
	// lie, and this is the direction that catches it.
	var beyond int
	if err := f.Admin.QueryRow(context.Background(), `
		SELECT count(*) FROM traffic_snapshots
		 WHERE site_id = $1 AND time < $2
		   AND time_bucket('1 day', time) NOT IN
		       (SELECT day FROM visitor_sketch WHERE site_id = $1)`,
		f.Site, watermark).Scan(&beyond); err != nil {
		t.Fatalf("looking for unmaterialized rows behind the watermark: %v", err)
	}
	if beyond != 0 {
		t.Errorf("%d rows lie behind the watermark with no sketch covering their day. "+
			"The read path trusts everything before that instant, so those visitors "+
			"are simply gone from a long range.", beyond)
	}

	// A second refresh with no new settled day must leave the watermark
	// where it is - and it does recompute, deliberately: the recheck
	// window pulls the start back by one day every cycle so a row that
	// arrives late still lands somewhere. What must not happen is the
	// watermark moving backwards with it, because the read path trusts
	// everything before it and a watermark that walked back would hand a
	// day to both halves of the answer.
	again, err := f.Sketch.Refresh(context.Background(), f.Site)
	if err != nil {
		t.Fatalf("second Refresh: %v", err)
	}
	if !again.Through.Equal(report.Through) {
		t.Errorf("a second refresh moved the watermark from %s to %s with no new "+
			"settled day in between",
			report.Through.Format(time.RFC3339), again.Through.Format(time.RFC3339))
	}
	if !again.From.Before(again.Through) {
		t.Errorf("the second refresh recomputed nothing (%s to %s). The recheck "+
			"window is the only thing that gives a late row a place to land, and a "+
			"refresh that stops at its own watermark has removed it.",
			again.From.Format(time.RFC3339), again.Through.Format(time.RFC3339))
	}
	var afterwards time.Time
	if err := f.Admin.QueryRow(context.Background(), `
		SELECT materialized_before FROM visitor_sketch_state WHERE site_id = $1`,
		f.Site).Scan(&afterwards); err != nil {
		t.Fatalf("re-reading the watermark: %v", err)
	}
	if !afterwards.Equal(watermark) {
		t.Errorf("the stored watermark moved from %s to %s across an idle refresh",
			watermark.Format(time.RFC3339), afterwards.Format(time.RFC3339))
	}
}

// Pruning drops the sketches whose rows retention has taken.
//
// A stored aggregate that outlives the rows behind it is a second,
// longer history: the dashboard would draw visitors for days whose
// detail pages are empty, and a customer would have no way to tell which
// of the two the product meant.
func TestStore_RealToolkit_PruningFollowsRetention(t *testing.T) {
	f := newSketchFixture(t, "api-sketch-prune")
	now := time.Now().UTC()

	// One day inside a 10-day retention and one outside it - but both
	// within sketchMaxSpan of each other, because one call materializes
	// at most thirty days from the oldest row. The first attempt here
	// seeded 40 days apart and the newer day was never materialized at
	// all: the cap, working, and a fixture measuring it by accident.
	f.Seed(t, []sketchRow{
		{IP: sketchIP(1), At: now.AddDate(0, 0, -3), Score: 10},
		{IP: sketchIP(2), At: now.AddDate(0, 0, -20), Score: 10},
	})
	f.Refresh(t)

	var before int
	if err := f.Admin.QueryRow(context.Background(),
		`SELECT count(*) FROM visitor_sketch WHERE site_id = $1`, f.Site).Scan(&before); err != nil {
		t.Fatalf("counting sketches: %v", err)
	}
	if before != 2 {
		t.Fatalf("%d sketches before the prune, want 2 - one either side of the "+
			"retention boundary, or the prune has nothing to be wrong about", before)
	}

	n, err := f.Sketch.Prune(context.Background(), f.Site, 10)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Errorf("Prune removed %d sketches, want 1", n)
	}

	var day time.Time
	if err := f.Admin.QueryRow(context.Background(),
		`SELECT day FROM visitor_sketch WHERE site_id = $1`, f.Site).Scan(&day); err != nil {
		t.Fatalf("reading the surviving sketch: %v", err)
	}
	if want := now.AddDate(0, 0, -3).Truncate(24 * time.Hour); !day.Equal(want) {
		t.Errorf("the surviving sketch covers %s, want %s - the prune kept the wrong side",
			day.Format("2006-01-02"), want.Format("2006-01-02"))
	}

	// And a retention of zero is a mistake rather than "delete
	// everything": the caller passes the same number it gave retention,
	// and retention has no such policy.
	if _, err := f.Sketch.Prune(context.Background(), f.Site, 0); err == nil {
		t.Error("Prune accepted a retention of 0 days")
	}
}

// The hash behind the sketch handles every shape of address this product
// stores.
//
// # Why this is measured rather than assumed
//
// Because the accuracy of the whole feature rests on one hash being
// well-behaved on the values this product happens to feed it, and those
// values are not arbitrary strings: they are full IPv4 addresses, /24
// networks (privacy.IPMasked), and /64 IPv6 networks. A hash that
// ignored the prefix length, or the low octets, would collide addresses
// that are genuinely different and undercount silently.
//
// It was also nearly mismeasured. A first pass generated sequential
// addresses and the estimate came back exact to within 0,003 % - forty
// times better than theory, because a sequential input spread perfectly
// across the registers. Scattered addresses put it back where theory
// says it should be. The generator below hashes the index for exactly
// that reason: *a flaw in generated data looks like a result.*
func TestStore_RealToolkit_TheSketchHandlesEveryAddressShape(t *testing.T) {
	f := newSketchFixture(t, "api-sketch-shapes")
	base := sketchBase()

	const n = 2000
	for _, shape := range []struct {
		name string
		addr func(int) string
	}{
		{"ipv4", func(i int) string { return scatteredIP(i) }},
		{"masked-v4", func(i int) string { return scatteredIP(i) + "/24" }},
		{"masked-v6", func(i int) string {
			return fmt.Sprintf("2001:db8:%x:%x::/64", (i*2654435761)%65536, (i*40503)%65536)
		}},
	} {
		t.Run(shape.name, func(t *testing.T) {
			site := "api-sketch-shape-" + shape.name
			g := newSketchFixture(t, site)

			at := base.Add(4 * time.Hour)
			rows := make([]sketchRow, 0, n)
			for i := 0; i < n; i++ {
				rows = append(rows, sketchRow{IP: shape.addr(i), At: at, Score: 10})
			}
			g.Seed(t, rows)
			g.Refresh(t)

			from, to := base, base.AddDate(0, 0, 1)
			exact := g.Counted(t, from, to, scoring.BotCutoff)
			est := g.Estimated(t, from, to, scoring.BotCutoff)
			if exact.Unique == 0 {
				t.Fatalf("no addresses of shape %s were stored", shape.name)
			}
			// Generous, because 2.000 addresses is a small sketch and
			// the point here is that the hash separates these values at
			// all - a hash that dropped the prefix length would collapse
			// thousands of them into one.
			if off := relative(est.Unique, exact.Unique); off > 0.05 {
				t.Errorf("%s: estimated %d against %d, off by %.2f%%",
					shape.name, est.Unique, exact.Unique, off*100)
			}
		})
	}
	_ = f
}

// The whole endpoint says which answer it gave, and carries the margin.
//
// Through Summary rather than through visitorsOver, because the two
// fields a page reads are set there - and because Summary is where the
// row count that decides the branch comes from. A test that only drove
// the inner function would leave "does anything actually pass the row
// count in" unmeasured, which is the shape of defect P5a was.
func TestStore_RealToolkit_TheSummarySaysWhichAnswerItGave(t *testing.T) {
	f := newSketchFixture(t, "api-sketch-summary")
	base := sketchBase()

	f.Seed(t, []sketchRow{
		{IP: sketchIP(1), At: base.Add(time.Hour), Score: 10},
		{IP: sketchIP(2), At: base.Add(time.Hour), Score: 80},
		{IP: sketchIP(3), At: base.AddDate(0, 0, 1).Add(time.Hour), Score: 80},
	})
	f.Refresh(t)

	from, to := base, base.AddDate(0, 0, 2)
	for _, tc := range []struct {
		name      string
		budget    int
		wantHow   VisitorCountMethod
		wantError float64
	}{
		{"within the budget", 1 << 40, VisitorCountExact, 0},
		{"over the budget", 1, VisitorCountEstimated, VisitorSketchRelativeError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := f.Store.exactRows
			f.Store.exactRows = tc.budget
			defer func() { f.Store.exactRows = previous }()

			got, err := f.Store.Summary(context.Background(), f.Site, from, to, scoring.BotCutoff)
			if err != nil {
				t.Fatalf("Summary: %v", err)
			}
			if got.VisitorCounts != tc.wantHow {
				t.Errorf("visitor_counts = %q, want %q", got.VisitorCounts, tc.wantHow)
			}
			if got.VisitorCountError != tc.wantError {
				t.Errorf("visitor_count_error = %v, want %v", got.VisitorCountError, tc.wantError)
			}
			// The numbers themselves, in both branches: three addresses,
			// two of them bots. Small enough that the estimate is exact
			// here, which is the point - the label must not be the only
			// thing that changes.
			if got.UniqueIPs != 3 || got.BotIPs != 2 || got.HumanIPs != 1 {
				t.Errorf("unique %d, bot %d, human %d; want 3, 2, 1",
					got.UniqueIPs, got.BotIPs, got.HumanIPs)
			}
			// And the row count that decided the branch is the rollup's,
			// reported on the same response. A summary that estimated
			// while reporting three snapshots would be a summary whose
			// own numbers disagree about why it estimated.
			if got.Snapshots != 3 {
				t.Errorf("snapshots = %d, want 3", got.Snapshots)
			}
		})
	}
}

// A Store nobody configured counts exactly, both ways round.
//
// Two claims, because the seam the tests above use has two ends. NewStore
// sets the budget to the measured constant - so the binary's Store is the
// one that was measured - and a Store assembled by hand, which several
// tests in this package do, behaves the same rather than estimating
// everything. A seam whose unset value changes behaviour silently
// rewrites every test that did not know about it.
func TestAStoreNobodyConfiguredUsesTheMeasuredRowBudget(t *testing.T) {
	f := newSketchFixture(t, "api-sketch-budget")
	base := sketchBase()
	f.Seed(t, []sketchRow{
		{IP: sketchIP(1), At: base.Add(time.Hour), Score: 10},
		{IP: sketchIP(2), At: base.AddDate(0, 0, 1).Add(time.Hour), Score: 10},
	})
	f.Refresh(t)

	if f.Store.exactRows != exactVisitorRowBudget {
		t.Errorf("a Store built the way the binary builds one has a row budget of %d, "+
			"want %d. Every sketch test in this file moves that field, so a zero "+
			"here would mean the product estimates every range and the tests still pass.",
			f.Store.exactRows, exactVisitorRowBudget)
	}

	// The zero value, on a database that *does* have sketches to merge,
	// so "exact" here is a decision rather than the only option.
	bare := &Store{pool: f.Store.pool}
	got, err := bare.Summary(context.Background(), f.Site,
		base, base.AddDate(0, 0, 2), scoring.BotCutoff)
	if err != nil {
		t.Fatalf("Summary on an unconfigured Store: %v", err)
	}
	if got.VisitorCounts != VisitorCountExact {
		t.Errorf("an unconfigured Store answered %q for a two-row range. Zero has to "+
			"read as the measured budget; read as a budget of zero it means every "+
			"range on every hand-built Store is estimated.", got.VisitorCounts)
	}
	if got.UniqueIPs != 2 {
		t.Errorf("unique = %d, want 2", got.UniqueIPs)
	}
}

// relative is |got - want| / want, and 0 when want is 0.
func relative(got, want int) float64 {
	if want == 0 {
		if got == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return math.Abs(float64(got-want)) / float64(want)
}

// sketchIP is a distinct address per index, in a documentation range.
func sketchIP(i int) string {
	return fmt.Sprintf("198.51.%d.%d", (i/256)%256, i%256)
}

// scatteredIP is a distinct address per index whose octets do not follow
// the index, so a sketch over a run of them behaves the way it behaves
// on real traffic rather than on a counter. See the shapes test for what
// that cost the first attempt.
func scatteredIP(i int) string {
	h := i * 2654435761
	return fmt.Sprintf("%d.%d.%d.%d",
		11+(h>>24)%200, (h>>16)%256, (h>>8)%256, h%256)
}

// Compile-time proof that the refresher this suite drives is the one the
// collector drives. A test that built its own SQL would measure the
// test.
var _ = storage.NewSketch
