//go:build integration

// Two appliers at once.
//
// The upgrade queue is built so that one applier holds the claim, and in
// a healthy deployment that is what happens. It is not a guarantee: a
// claim goes stale after fifteen minutes and another applier takes it
// over, so an applier that is slow rather than dead can still be inside
// the schema when its replacement starts. This file is about what the
// database does when they are.
package applier

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/schemafiles"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// TestAppliersRunningAtOnceGiveWayInsteadOfColliding.
//
// # What this measures, and why a unit test could not
//
// Applying a schema file twice over is safe by design - every CREATE has
// an IF NOT EXISTS and every policy is dropped before it is created -
// but "safe twice in a row" and "safe twice at once" are different
// properties, and only the first one had ever been tested.
//
// Measured before the fix, three sessions applying every file twelve
// times over:
//
//	internal/retention/schema.sql     tuple concurrently updated (XX000)
//	internal/asnlookup/schema.sql     tuple concurrently updated (XX000)
//	internal/upgrade/schema.sql       deadlock detected          (40P01)
//	internal/rangerefresh/schema.sql  deadlock detected          (40P01)
//
// 17 of 360. Neither error is retried by anything and neither reads as
// "another upgrade was running" to whoever pressed the button.
//
// # Why it calls apply rather than RunOnce
//
// Two earlier versions of this test measured nothing, and both looked
// convincing.
//
// The first ran the SQL directly. That is the wrong contract: two bare
// applications are not meant to be safe and no reachable amount of
// per-statement guarding would make them so - CREATE OR REPLACE FUNCTION
// rewrites its row whether or not the body changed, and DROP POLICY /
// CREATE POLICY cannot become conditional without making every future
// policy edit silently do nothing.
//
// The second drove RunOnce, which is what a deployment calls. It passed
// with the fix and it also passed with the lock deliberately broken,
// because the queue gets there first: one in-flight request means one
// applier holds a claim and the other two are told there is nothing to
// do, so no two of them were ever inside the schema at all. It reported
// "8 applied, 0 gave way" both times - and the second number was the
// whole answer, sitting in plain sight.
//
// So this calls apply directly. That is precisely the overlap the queue
// cannot prevent: a claim goes stale after fifteen minutes and another
// applier takes it over, and an applier that is slow rather than dead is
// still inside apply when its replacement starts. Reproducing that
// through the queue would mean a fifteen-minute test.
//
// It needs no list of files: schemafiles.InOrder is the schema, so a
// file added next year is covered on the day it is added rather than on
// the day somebody remembers this test exists.
func TestAppliersRunningAtOnceGiveWayInsteadOfColliding(t *testing.T) {
	ctx := context.Background()
	applierAndPanel(t) // for UpgradeQueueLock, SchemaVersionLock and a clean queue
	// Deliberately not testdb.SchemaApplyLock, though every other suite
	// that touches the schema takes it. The appliers below take it
	// themselves - that is the mechanism under test - so holding it here
	// would make all three give way to this test rather than to each
	// other, and measure a tidy "0 applied" that proved only that the
	// test could block itself. That was version one of this line.
	//
	// testdb.SchemaRaceLock instead, which means the opposite: no other
	// suite may be applying a schema while this one races. Without it the
	// racers time out against internal/asnlookup rather than against each
	// other, and report "8 applied, 16 gave way" - the exact signature of
	// the lock not being taken, so the false red and the true red were the
	// same red. Measured, on the first of two consecutive runs.
	testdb.Lock(t, testdb.Pool(t, testdb.SchemaAdmin), testdb.SchemaRaceLock)

	// Three, one more than the two the stale-claim path can actually
	// produce: a fix that only worked for two would pass a two-way test.
	const appliers = 3
	const rounds = 8

	// Pools and appliers built here rather than inside the goroutines:
	// testdb.Pool reports failure through t, and t.Fatalf from a goroutine
	// that is not the test's own stops the wrong one.
	//
	// One pool each, because the applier pins a connection and sets
	// session state on it - three sharing a pool would be three appliers
	// only some of the time.
	racers := make([]*Applier, appliers)
	for i := range racers {
		racers[i] = &Applier{
			Pool: testdb.Pool(t, testdb.SchemaAdmin),
			Name: fmt.Sprintf("racer-%d", i),
		}
	}

	var wg sync.WaitGroup
	bad := make(chan string, appliers*rounds)
	var mu sync.Mutex
	var applied, waitedForSchemaLock, waitedForTable int
	// How many were inside apply at the same moment, at the most. A run
	// where this never reached two raced nobody, whatever else it found.
	var inFlight, mostAtOnce int

	// Started together, so the overlap is real rather than three appliers
	// taking turns because the first had finished before the second began.
	start := make(chan struct{})
	for _, a := range racers {
		wg.Add(1)
		go func(a *Applier) {
			defer wg.Done()
			<-start
			for r := 0; r < rounds; r++ {
				mu.Lock()
				inFlight++
				if inFlight > mostAtOnce {
					mostAtOnce = inFlight
				}
				mu.Unlock()
				_, err := a.apply(ctx)
				mu.Lock()
				inFlight--
				mu.Unlock()

				switch {
				case err == nil:
					mu.Lock()
					applied++
					mu.Unlock()
				case errors.Is(err, ErrSchemaLockBusy):
					// Waited for another applier and gave way. The design
					// working: RunOnce turns this into ErrBusy and puts the
					// request back for the next tick.
					mu.Lock()
					waitedForSchemaLock++
					mu.Unlock()
				case isLockTimeout(err):
					// Waited for a *table* instead - that is traffic, and an
					// applier only meets traffic inside the schema. Counted
					// separately because this is the whole assertion below.
					mu.Lock()
					waitedForTable++
					mu.Unlock()
				default:
					bad <- err.Error()
				}
			}
		}(a)
	}
	close(start)
	wg.Wait()
	close(bad)

	// Counted rather than reported one by one: a regression here produces
	// many identical lines and the count is the interesting part.
	seen := map[string]int{}
	total := 0
	for msg := range bad {
		total++
		seen[msg]++
	}
	if total > 0 {
		var b strings.Builder
		for msg, n := range seen {
			fmt.Fprintf(&b, "\n\t%s (%dx)", msg, n)
		}
		t.Errorf("%d of %d concurrent applications failed with something other than "+
			"a lock timeout, across %d files:%s\n\n"+
			"Two appliers overlap whenever a claim goes stale, and the answer to "+
			"that has to be one of them giving way. \"tuple concurrently updated\" "+
			"or \"deadlock detected\" here means two of them were inside the schema "+
			"together - see internal/dblock",
			total, appliers*rounds, len(schemafiles.InOrder), b.String())
	}

	// Both guards exist because this test produced a confident green twice
	// while measuring nothing at all.
	//
	// Nothing applied: whatever happened, it was not a schema being
	// applied, so the run says nothing about applying one.
	//
	// Nobody overlapped: three appliers that politely took turns would
	// pass with no lock at all, which is exactly how the RunOnce version
	// of this test passed with the lock deliberately defeated.
	if applied == 0 {
		t.Error("nothing applied at all, so this run says nothing about two at once")
	}
	if mostAtOnce < 2 {
		t.Errorf("never more than %d applier inside the schema at once, so nothing "+
			"raced.\nThis run would pass with no schema lock at all", mostAtOnce)
	}

	// What this test does *not* assert, after being wrong about it twice.
	//
	// The assertion is the one above: the errors in `bad`. XX000 and
	// 40P01 happen only when two appliers rewrite one catalogue row
	// together, so they are proof of the thing this exists to prevent,
	// and they are proof at any speed. Removing the schema lock produces
	// them - measured, "tuple concurrently updated" three times over.
	//
	// Two weaker things looked like assertions and were not:
	//
	//   - "no more than three may give way" - a ratio, so a threshold, so
	//     a number belonging to the machine that measured it. Passed here
	//     at 24/0 and failed on CI at 17/7 with nothing wrong.
	//   - "nobody may wait on a table" - defended as machine-independent
	//     on the reasoning that an applier meets traffic only inside the
	//     schema, so a table wait means two were in there together. The
	//     reasoning is wrong, and the run that showed it had the same
	//     commit passing on one branch and failing on the other: the
	//     traffic is not other appliers. `go test ./...` runs packages in
	//     parallel and internal/panel, internal/logsink and internal/
	//     storage all write to tables these schema files lock. Under CI
	//     load one such wait went past 250ms. One of twenty-four.
	//
	// So both counts are reported and neither is judged. A number in a
	// log line costs nothing and tells whoever reads a failure what the
	// run looked like; the same number in an `if` is a threshold nobody
	// can calibrate from one machine.

	t.Logf("%d applied, %d waited for the schema lock, %d waited for a table, "+
		"%d inside at once at the most",
		applied, waitedForSchemaLock, waitedForTable, mostAtOnce)
}
