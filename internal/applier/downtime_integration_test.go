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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
	"github.com/cruciblelab/crucible-analytic/internal/upgrade"
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
// happening.
//
// The numbers above were measured before the thresholds below were
// chosen, and are the reason they are what they are.
//
// # The explanation that went with them was wrong
//
// This comment used to continue: "every CREATE is IF NOT EXISTS, so a
// re-apply finds its work already done and takes no heavy lock". The
// numbers were right and the mechanism was not, which is the more
// dangerous half - a wrong mechanism makes a whole class of failure look
// impossible. Measured directly:
//
//	CREATE INDEX IF NOT EXISTS lockprobe_id_idx ON lockprobe (id);
//	NOTICE: relation "lockprobe_id_idx" already exists, skipping
//	SELECT mode FROM pg_locks WHERE relation = 'lockprobe'::regclass ...
//	  -> ShareLock, granted
//
// It skips the *work*, not the *lock*. The ShareLock is taken before the
// existence check and held to the end of the transaction - and a schema
// file is one implicit transaction - so a re-apply does block every
// writer of every indexed table for as long as its file runs. That it
// costs milliseconds is a fact about how fast the files are, not a fact
// about locking.
//
// What the wrong mechanism hid: two parties taking the same tables in
// opposite orders can deadlock, and PostgreSQL then kills one of them.
// applier.lockTimeout is the answer, and the "panel write" probe below
// is what found it.

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
//
// # The floor is a minimum, not the whole floor - measured
//
// 250ms was chosen against a development machine where the upgrade took
// 35-86ms. On a CI runner it takes 639ms, and every probe scales with
// it: worst-at-rest went from ~6ms to ~42ms and worst-during from ~13ms
// to 54-251ms. One run failed by 0.7ms - api read waited 250.73ms - while
// the same commit passed on the same workflow minutes earlier.
//
// Nothing had stopped. What the numbers say is a machine roughly
// thirteen times slower, and ordinary queueing behind a schema file that
// holds a ShareLock for its whole duration (see the header: IF NOT
// EXISTS skips the work, not the lock). The stall was 39% of the upgrade
// window; on the development machine the same figure is 28%.
//
// So the floor also scales with the window: a query cannot have been
// blocked *by the upgrade* for longer than the upgrade lasted, and a
// wait comfortably inside the window is the queueing this design already
// accepts. effectiveFloor is that rule.
//
// # What this costs, said plainly
//
// Sensitivity in one narrow band: a mild regression that makes the
// upgrade itself take, say, 400ms and blocks a query for about as long
// would now sit at the floor rather than over it. That band is covered
// from the other side by TestTheUpgradeYieldsToTrafficRatherThanTheOther
// WayRound, which constructs the contention deliberately and asserts the
// mechanism - the applier gives way inside deadlock_timeout and the
// traffic commits - rather than inferring it from timings on whatever
// machine happens to be running.
const (
	comparedToRest        = 4
	worthComplainingAbout = 250 * time.Millisecond
)

// effectiveFloor is how slow a query has to get before this complains.
//
// The larger of the absolute minimum and the upgrade window, for the
// reason above: below the window, a wait is bounded by the thing that
// caused it and is the accepted cost of applying a schema file; above
// it, the query was waiting for something else.
func effectiveFloor(upgradeTook time.Duration) time.Duration {
	if upgradeTook > worthComplainingAbout {
		return upgradeTook
	}
	return worthComplainingAbout
}

// stallVerdict is what one probe's numbers mean.
type stallVerdict int

const (
	stallFine stallVerdict = iota
	// stallOverCeiling is a wait a customer notices, whatever caused it.
	stallOverCeiling
	// stallDisproportionate is under the ceiling but longer than the
	// upgrade that supposedly caused it, and several times this machine's
	// own at-rest worst.
	stallDisproportionate
)

// judgeStall is the rule, extracted so it can be tested against numbers
// rather than only against whatever the machine did today.
//
// The switch used to live inline, which meant the only way to know what
// it would say about a given run was to produce that run - and the runs
// that matter are the ones a laptop does not reproduce. The measurements
// in TestTheStallRuleAgreesWithWhatWasMeasured are real observations,
// including the two that made this rule what it is.
func judgeStall(during, baseline, upgradeTook time.Duration) stallVerdict {
	switch {
	case during > worstAcceptableStall:
		return stallOverCeiling
	case during > effectiveFloor(upgradeTook) && during > comparedToRest*baseline:
		return stallDisproportionate
	default:
		return stallFine
	}
}

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

// baselinePeriod is how long the load runs before the upgrade starts.
//
// Named rather than written at the call site because the paced probe's
// floor is derived from it: a probe that pauses cannot reach
// minimumQueriesOverall and must not be asked to, but "it ran at all" is
// still worth asserting. baselinePeriod/pause is what it should manage,
// and a quarter of that is the floor - loose enough for a loaded runner,
// far above the zero that means the goroutine never started.
const baselinePeriod = 1500 * time.Millisecond

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
		// pause between this probe's queries. Zero for the four that
		// model a firehose, because that is what they are: a collector
		// and a beacon really do write continuously.
		pause time.Duration
		run   func(context.Context, *pgxpool.Pool) error
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
		{
			// The probe that found the defect, and the reason it is a
			// write with a foreign key rather than another read.
			//
			// Four reading-or-single-table probes can only ever queue
			// behind the applier: they take one lock, wait, and proceed.
			// They measure how long a wait is and cannot see the failure
			// that matters more, which is a wait that never ends because
			// the two parties hold what the other wants.
			//
			// panel_operations references panel_users, so this statement
			// locks the two tables in the opposite order to the panel
			// schema file - which is a real cycle, and PostgreSQL resolved
			// it by killing one of them:
			//
			//	Process A waits for RowShareLock on panel_users; blocked by B.
			//	Process B waits for ShareLock on panel_operations; blocked by A.
			//
			// Copied from that run's log. Without applier.lockTimeout the
			// victim can be this probe, which is a customer's operation
			// failing during an upgrade they were told was safe to start.
			name: "panel write",
			role: testdb.Panel,
			// Paced, and this one number is a finding rather than a knob.
			//
			// Without a pause this probe never lets go of
			// panel_operations, so the applier's 250ms lock_timeout
			// expires every time and the upgrade can never land at all -
			// measured: three runs out of three, "canceling statement due
			// to lock timeout" on internal/panel/schema.sql.
			//
			// That is the honest behaviour rather than a defect, and it is
			// what an operator seeing repeated lock timeouts should read
			// it as: something is writing that table continuously and the
			// schema change needs a quieter moment. But it is not traffic.
			// A panel writes an operation row per human action; 50/s is
			// already far above any real deployment, and it leaves the
			// 250ms windows the applier needs.
			pause: 20 * time.Millisecond,
			run: func(ctx context.Context, p *pgxpool.Pool) error {
				_, err := p.Exec(ctx, `
					INSERT INTO panel_operations (id, action, target, outcome, actor_kind)
					VALUES ($1, 'test', 'downtime-probe', 'succeeded', 'test')`,
					"op-downtime-"+strconv.FormatInt(time.Now().UnixNano(), 36))
				return err
			},
		},
	}
	t.Cleanup(func() {
		if _, err := testdb.Admin(t).Exec(context.Background(),
			`DELETE FROM panel_operations WHERE target = 'downtime-probe'`); err != nil {
			t.Logf("cleanup: clearing the probe's operations: %v", err)
		}
	})

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
				if l.pause > 0 {
					time.Sleep(l.pause)
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
	time.Sleep(baselinePeriod)

	// The real thing: a real request, applied by the real applier, over
	// every schema file this binary carries.
	//
	// Retried on ErrBusy exactly as the production timer does. The
	// applier gives way to traffic on purpose, and `go test ./...` runs
	// this suite beside two others that write these tables continuously -
	// more contention than any deployment sees. The window measured is
	// the attempt that actually applied, so a busy attempt does not
	// contribute a stall to a measurement of an upgrade that did not
	// happen.
	askFor(t, panelPool, schemaver.Fingerprint)
	var (
		req                        *upgrade.Request
		err                        error
		upgradeBegan, upgradeEnded time.Time
	)
	for deadline := time.Now().Add(30 * time.Second); ; {
		upgradeBegan = time.Now()
		req, err = a.RunOnce(ctx)
		upgradeEnded = time.Now()
		if !errors.Is(err, ErrBusy) {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
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

		floor := minimumQueriesOverall
		if l.pause > 0 {
			floor = int(baselinePeriod/l.pause) / 4
		}
		if len(r.samples) < floor {
			t.Errorf("%s ran %d queries in total, which is too few to have measured "+
				"anything - this test would report an undisturbed service without "+
				"ever having asked one (floor %d)", l.name, len(r.samples), floor)
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

		switch judgeStall(during, baseline, upgradeTook) {
		case stallOverCeiling:
			t.Errorf("%s waited %v for a single query while the schema was applied, "+
				"and the ceiling is %v. At rest the same query's worst was %v.\n"+
				"Something in the schema files now takes a heavy lock on a table with "+
				"rows in it - an ALTER that rewrites, or a CREATE INDEX without "+
				"IF NOT EXISTS. A customer watching their dashboard sees that as the "+
				"dashboard stopping", l.name, during, worstAcceptableStall, baseline)

		case stallDisproportionate:
			t.Errorf("%s waited %v during the upgrade against %v at rest on this same "+
				"machine - %dx worse, and longer than the upgrade itself took (%v).\n"+
				"Under the absolute ceiling, so this is the sensitive half: on a fast "+
				"machine a real lock regression can cost far less than %v and still be "+
				"an outage a customer sees. A wait longer than the whole upgrade is "+
				"not queueing behind it",
				l.name, during, baseline, comparedToRest, effectiveFloor(upgradeTook),
				worstAcceptableStall)
		}
	}
}

// TestTheUpgradeYieldsToTrafficRatherThanTheOtherWayRound.
//
// The measurement above is probabilistic: it runs real load against a
// real apply and reports what happened. This one is not. It constructs
// the contention deliberately and asserts the outcome, because "the
// traffic wins" is the promise the upgrade button makes and a promise
// kept by luck is not kept.
//
// A panel transaction holds panel_operations. The applier then tries to
// apply, which needs ShareLock on that table. Exactly one of the two can
// proceed, and which one is the whole question.
func TestTheUpgradeYieldsToTrafficRatherThanTheOtherWayRound(t *testing.T) {
	a, panelPool := applierAndPanel(t)
	ctx := context.Background()

	// The customer's write, in flight and uncommitted - a panel request
	// that has done its insert and is still assembling the audit row.
	tx, err := panelPool.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning the panel's transaction: %v", err)
	}
	defer tx.Rollback(context.Background())

	id := "op-yield-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	if _, err := tx.Exec(ctx, `
		INSERT INTO panel_operations (id, action, target, outcome, actor_kind)
		VALUES ($1, 'test', 'yield-probe', 'succeeded', 'test')`, id); err != nil {
		t.Fatalf("the panel's insert failed before the applier had even started: %v", err)
	}

	askFor(t, panelPool, schemaver.Fingerprint)

	began := time.Now()
	_, err = a.RunOnce(ctx)
	waited := time.Since(began)

	// The applier has to fail here, and that is the passing case.
	if err == nil {
		t.Fatal("the applier applied the schema while a panel transaction held " +
			"panel_operations, which means it either did not need the lock (has the " +
			"schema stopped indexing that table?) or waited for it. Waiting is the " +
			"failure this test exists for: a statement queued for a table lock puts " +
			"every later request behind it")
	}
	if !strings.Contains(err.Error(), "lock timeout") {
		t.Fatalf("the applier failed for the wrong reason: %v.\n"+
			"Expected a lock timeout, which is the applier giving way. Any other "+
			"error means this test measured something else", err)
	}

	// Under deadlock_timeout, which is the point rather than a detail: a
	// wait that reaches it lets the detector pick a victim, and the
	// victim can be the customer.
	const deadlockDetection = time.Second
	if waited >= deadlockDetection {
		t.Errorf("the applier waited %v before giving up, and PostgreSQL starts "+
			"looking for deadlocks at %v (deadlock_timeout).\n"+
			"Under that threshold the cycle is always broken by the upgrade "+
			"yielding; over it, the database chooses, and it can choose the "+
			"customer's write", waited, deadlockDetection)
	}

	// And the traffic is untouched: not rolled back, not cancelled,
	// still able to commit.
	if _, err := tx.Exec(ctx, `
		UPDATE panel_operations SET outcome = 'succeeded' WHERE id = $1`, id); err != nil {
		t.Fatalf("the panel's transaction could not continue after the applier gave "+
			"up: %v.\nThis is the outcome the whole arrangement exists to prevent - "+
			"a customer's operation killed by an upgrade they were told was safe "+
			"to start", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("the panel's transaction could not commit: %v", err)
	}

	t.Logf("the applier gave up after %v; the panel's transaction committed", waited)

	// And the request is waiting again rather than marked failed.
	//
	// The difference a customer sees: a queued request needs nothing from
	// them, a failed one needs the button pressed again. Reporting "not
	// now" as "it broke" is the kind of message that gets a working
	// system restarted at three in the morning.
	if !errors.Is(err, ErrBusy) {
		t.Errorf("the applier returned %v rather than ErrBusy, so a caller cannot "+
			"tell a busy table from a real failure", err)
	}
	latest, latestErr := upgrade.Latest(context.Background(), panelPool)
	if latestErr != nil {
		t.Fatalf("reading the request back: %v", latestErr)
	}
	if latest.State != upgrade.StatePending {
		t.Errorf("the request is %q after the applier gave way, want %q.\n"+
			"A busy table is not a failed upgrade: the next tick applies it, and "+
			"telling the customer it failed asks them to press a button for a "+
			"condition that clears by itself", latest.State, upgrade.StatePending)
	}
	if latest.ErrorChain == "" {
		t.Error("the requeued request carries no note, so the health page shows a " +
			"request that is queued and no reason it has not run")
	}

	if _, err := testdb.Admin(t).Exec(context.Background(),
		`DELETE FROM panel_operations WHERE target = 'yield-probe'`); err != nil {
		t.Logf("cleanup: %v", err)
	}
}
