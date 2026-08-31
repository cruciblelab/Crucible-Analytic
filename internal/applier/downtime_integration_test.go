//go:build integration

// The claim L3 is not allowed to make without measuring it: applying a
// schema upgrade does not stop the four services.
//
// It is the claim the button rests on. A customer presses "upgrade" in
// the panel while their site is serving traffic, and what they are being
// told, implicitly, is that this is safe to do now rather than at three
// in the morning. PLAN.md says that has to be measured rather than
// asserted, and it is right to: nothing about the arrangement makes it
// true by construction.
//
// # What could actually stop them
//
// Not a crash. The four services never restart, never reconnect and
// never notice this happening - the applier is a different process
// against the same database. What can stop them is the lock:
//
//   - ALTER TABLE takes ACCESS EXCLUSIVE, which blocks every reader and
//     every writer of that table for as long as it is held.
//   - CREATE INDEX (not CONCURRENTLY) takes SHARE: readers continue,
//     writers wait.
//   - And a lock request queues. A statement waiting for ACCESS
//     EXCLUSIVE blocks everything that arrives behind it, even readers
//     that would not have conflicted with the running query.
//
// So "no service stops" is not a question about processes. It is a
// question about how long a query can be made to wait, and the only way
// to answer it is to run the real DDL against a real database with real
// concurrent traffic on it and time every query.
package applier

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// What the measurement says, on the development database, over three
// runs with all four services' query patterns running concurrently:
//
//	the upgrade itself        35ms, 35ms, 86ms
//	worst query during it     2.3ms .. 9.9ms
//	worst query at rest       5.0ms .. 83.5ms
//
// The headline is the last two lines together: during the upgrade the
// worst query was *faster* than the worst query when nothing was
// happening. The DDL is invisible to the four services, and the reason is
// in the schema files rather than in the applier - every CREATE is
// IF NOT EXISTS, so a re-apply finds its work already done and takes no
// heavy lock. The property belongs to the SQL, which is why it needs a
// test: the next schema file is one ALTER away from ending it.
//
// The numbers above were measured before the thresholds below were
// chosen, and are the reason they are what they are.

// worstAcceptableStall is the absolute ceiling.
//
// 2 seconds, against a measurement of ~10ms, and the gap is not slack for
// its own sake: this has to pass on a CI runner with a cold page cache
// and a busy disk, where a single insert can take a quarter of a second
// for reasons that have nothing to do with an upgrade. A threshold tuned
// to a warm laptop is a threshold that goes red for the weather.
//
// What it still catches is what it is for. An ALTER TABLE that rewrites a
// hypertable, or a CREATE INDEX without IF NOT EXISTS on a table with
// rows in it, does not cost 10ms - it costs seconds to minutes, and the
// customer watching their dashboard sees the dashboard stop.
const worstAcceptableStall = 2 * time.Second

// The absolute ceiling above is deliberately loose, and on a fast machine
// something could get four times slower during an upgrade and still pass
// it. So the run is also compared against itself.
//
// Both conditions have to hold before this complains. The ratio alone
// would be unusable - a baseline of 2ms makes 9ms a 4.5x regression and
// nothing is wrong - and the floor alone is the ceiling above with a
// smaller number. Together they say: it got several times worse than this
// same machine manages at rest, *and* it got slow enough for a person to
// notice.
const (
	comparedToRest        = 4
	worthComplainingAbout = 250 * time.Millisecond
)

// minimumQueriesOverall is the floor that stops this test passing while
// measuring nothing.
//
// Without it, a load generator that failed to start - a bad DSN, a
// cancelled context, a typo in a query - would produce zero queries, zero
// errors and zero stalls, and this test would report an undisturbed
// service. True, and meaningless.
//
// Against the whole run rather than against the upgrade window, which was
// the first version and was wrong. The window is 35ms when the database
// is warm, and the slowest of the four probes fits about sixteen queries
// into it - so a per-window floor of twenty failed a run in which
// everything worked, because the upgrade had been fast. A floor that goes
// red when the thing under test performs *well* is not a floor.
//
// What replaces it for the window is the weaker claim that is actually
// true by construction: each probe loops without pause, so there is
// always one of its queries in flight, so any window overlaps at least
// one. Zero overlapping queries means that goroutine stopped running.
const minimumQueriesOverall = 100

// TestNoServiceStopsWhileTheSchemaIsApplied.
func TestNoServiceStopsWhileTheSchemaIsApplied(t *testing.T) {
	a, panelPool := applierAndPanel(t)
	ctx := context.Background()

	// The four services, as themselves. Each connects as the role its
	// production code uses and issues the statement that role actually
	// issues - the collector's insert, the API's read, the panel's own
	// table - because a test that ran everything as one privileged role
	// would be measuring a database nobody deploys.
	//
	// internal/testdb's package comment lists the three real bugs a more
	// privileged fixture hid here before.
	load := []struct {
		name string
		role string
		run  func(context.Context, *pgxpool.Pool) error
	}{
		{
			name: "collector insert",
			role: testdb.Collector,
			run: func(ctx context.Context, p *pgxpool.Pool) error {
				_, err := p.Exec(ctx, `
					INSERT INTO traffic_snapshots
					  (time, site_id, ip, ja4, prev_window_count, curr_window_count,
					   request_rate, bot_score, is_known_bot_ja4, country, asn,
					   asn_org, is_known_bot_asn)
					VALUES (now(), 'downtime-probe', $1, 't13d1516h2_8daaf6152771_b186095e22b6',
					        1, 1, 1.0, 10, false, 'TR', 9121, 'Turk Telekom', false)`,
					netip.MustParseAddr("203.0.113.7"))
				return err
			},
		},
		{
			name: "beacon insert",
			role: testdb.Beacon,
			run: func(ctx context.Context, p *pgxpool.Pool) error {
				_, err := p.Exec(ctx, `
					INSERT INTO beacon_events (time, site_id, visitor_id, event_type, path, ip)
					VALUES (now(), 'downtime-probe', 'probe-visitor', 'pageview', '/', $1)`,
					netip.MustParseAddr("203.0.113.7"))
				return err
			},
		},
		{
			name: "api read",
			role: testdb.Reader,
			run: func(ctx context.Context, p *pgxpool.Pool) error {
				var n int64
				return p.QueryRow(ctx, `
					SELECT count(*) FROM traffic_snapshots
					WHERE site_id = 'downtime-probe' AND time > now() - interval '1 hour'`).Scan(&n)
			},
		},
		{
			name: "panel read",
			role: testdb.Panel,
			run: func(ctx context.Context, p *pgxpool.Pool) error {
				var n int64
				return p.QueryRow(ctx, `SELECT count(*) FROM panel_users`).Scan(&n)
			},
		},
	}

	// Opened before the load starts, so a pool that cannot connect fails
	// this test as a setup problem rather than as an availability one.
	pools := make([]*pgxpool.Pool, len(load))
	for i, l := range load {
		pools[i] = testdb.Pool(t, l.role)
	}

	// Every query's start and end, so the worst case can be attributed to
	// the upgrade window afterwards rather than to the whole run.
	//
	// The first version of this kept a single running maximum and
	// reported it as the upgrade's cost. Its own first run said so: the
	// upgrade took 141ms and the "worst stall" was 346ms - a number
	// larger than the thing it was supposedly caused by, which is not a
	// measurement of anything. Most of it was pool warm-up, before the
	// DDL had started.
	//
	// A test whose reported quantity is not the quantity in its name is
	// worse than no number, because the number gets quoted.
	type sample struct{ start, end time.Time }
	type result struct {
		samples  []sample
		firstErr error
	}
	results := make([]result, len(load))

	// Stopped by the applier finishing, not by a timer: a fixed duration
	// would either end before the DDL did - measuring an upgrade that had
	// not happened yet - or run on long after it, diluting the worst case
	// with idle time.
	stop := make(chan struct{})
	var running sync.WaitGroup
	var started atomic.Int32

	for i := range load {
		running.Add(1)
		go func(i int) {
			defer running.Done()
			l := load[i]
			started.Add(1)
			for {
				select {
				case <-stop:
					return
				default:
				}

				begin := time.Now()
				err := l.run(ctx, pools[i])
				end := time.Now()

				results[i].samples = append(results[i].samples, sample{begin, end})
				if err != nil && results[i].firstErr == nil {
					results[i].firstErr = fmt.Errorf("after %d queries: %w",
						len(results[i].samples), err)
				}
			}
		}(i)
	}

	// Let the load actually get going before the DDL starts.
	//
	// Two reasons, and the second was found by measuring. The obvious one
	// is that an applier finishing against an idle database measures
	// nothing. The other is that this period is the baseline the upgrade
	// is compared against, and a baseline taken during pool warm-up says
	// the machine is slow when it is only cold - so it has to be long
	// enough for the connections to settle.
	deadline := time.Now().Add(5 * time.Second)
	for started.Load() < int32(len(load)) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(1500 * time.Millisecond)

	// The real thing: a real request, applied by the real applier, over
	// every schema file this binary carries.
	askFor(t, panelPool, schemaver.Fingerprint)
	upgradeBegan := time.Now()
	req, err := a.RunOnce(ctx)
	upgradeEnded := time.Now()
	upgradeTook := upgradeEnded.Sub(upgradeBegan)

	close(stop)
	running.Wait()

	if err != nil {
		t.Fatalf("the upgrade itself failed, so nothing here is a measurement "+
			"of an upgrade: %v", err)
	}
	if req == nil {
		t.Fatal("RunOnce applied nothing")
	}

	t.Logf("the upgrade took %v (%s .. %s)", upgradeTook,
		upgradeBegan.Format("15:04:05.000"), upgradeEnded.Format("15:04:05.000"))

	for i, l := range load {
		r := results[i]

		// A query counts as "during" if it overlapped the window at all.
		//
		// Overlap rather than containment, and this is the case that
		// matters most: a query that started before the DDL and was still
		// running when it began is exactly the one a lock blocks. A
		// containment test would drop it - discarding the worst sample as
		// out of scope.
		var during, baseline time.Duration
		var duringCount int
		for _, sm := range r.samples {
			took := sm.end.Sub(sm.start)
			if sm.end.After(upgradeBegan) && sm.start.Before(upgradeEnded) {
				duringCount++
				if took > during {
					during = took
				}
				continue
			}
			if took > baseline {
				baseline = took
			}
		}

		t.Logf("%-16s %5d queries (%d during) | worst during %v | worst at rest %v",
			l.name, len(r.samples), duringCount, during, baseline)

		if len(r.samples) < minimumQueriesOverall {
			t.Errorf("%s ran %d queries in total, which is too few to have measured "+
				"anything - this test would report an undisturbed service without "+
				"ever having asked one", l.name, len(r.samples))
			continue
		}
		if duringCount == 0 {
			t.Errorf("%s had no query in flight during the upgrade window. It loops "+
				"without pause, so that cannot happen while it is running - the probe "+
				"stopped, and the worst case it would have seen went unmeasured", l.name)
			continue
		}

		// A failed query is a stopped service, whatever the timing says.
		// The four never reconnect during an upgrade, so an error here is
		// a request a real visitor would have lost.
		if r.firstErr != nil && !errors.Is(r.firstErr, context.Canceled) {
			t.Errorf("%s failed during the upgrade: %v.\n"+
				"The upgrade button tells a customer this is safe to press while "+
				"their site is serving traffic", l.name, r.firstErr)
		}

		switch {
		case during > worstAcceptableStall:
			t.Errorf("%s waited %v for a single query while the schema was applied, "+
				"and the ceiling is %v. At rest the same query's worst was %v.\n"+
				"Something in the schema files now takes a heavy lock on a table with "+
				"rows in it - an ALTER that rewrites, or a CREATE INDEX without "+
				"IF NOT EXISTS. A customer watching their dashboard sees that as the "+
				"dashboard stopping", l.name, during, worstAcceptableStall, baseline)

		case during > worthComplainingAbout && during > comparedToRest*baseline:
			t.Errorf("%s waited %v during the upgrade against %v at rest on this same "+
				"machine - %dx worse, and over %v.\n"+
				"Under the absolute ceiling, so this is the sensitive half: on a fast "+
				"machine a real lock regression can cost far less than %v and still be "+
				"an outage a customer sees",
				l.name, during, baseline, comparedToRest, worthComplainingAbout,
				worstAcceptableStall)
		}
	}
}
