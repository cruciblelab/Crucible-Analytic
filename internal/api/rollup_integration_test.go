//go:build integration

// The rollup must never disagree with the table it summarizes.
//
// # Why this file is the phase rather than an addition to it
//
// A rollup is a second place that holds the same history. C9.3 was about
// exactly this shape in another part of the product: a page said a
// membership had ended while the gate still let the member in, and
// nobody looks twice at somebody they believe has left. Here the two
// places are the dashboard's headline numbers and the detail rows under
// them, and a customer who sees them disagree has no way to tell which
// one is the product.
//
// So the oracle in this file is not a second implementation of the
// rollup's arithmetic. It is the query this endpoint ran *before* the
// rollup existed - one pass over traffic_snapshots - and every assertion
// is the same question asked twice against the same rows: once through
// the path a deployment actually uses, once through the raw table. If
// they differ, the rollup is wrong, and it does not matter which of the
// two is prettier.
package api

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/storage"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// rateTolerance is how far two floating-point sums of the same numbers
// may sit apart.
//
// Not zero, and the reason is arithmetic rather than laziness: the rollup
// adds request_rate within a bucket and then adds the bucket totals,
// while the oracle adds every row in one go. Floating-point addition is
// not associative, so the two orders can differ in the last bits. What
// they cannot do is differ in a way a person would see.
//
// Relative rather than absolute, because the figures span a rate of 0,4
// and a window count in the thousands, and an absolute epsilon that
// suits one is meaningless for the other.
const rateTolerance = 1e-9

// oracleSummary is what this endpoint computed before the rollup existed.
//
// Kept verbatim rather than rewritten, deliberately. A tidier oracle is
// one that shares reasoning with the code under test, and shared
// reasoning is exactly what a divergence test must not have.
func oracleSummary(ctx context.Context, t *testing.T, pool *pgxpool.Pool,
	site string, from, to time.Time) aggregable {
	t.Helper()

	var out aggregable
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(max(request_rate), 0),
		       COALESCE(avg(request_rate), 0),
		       count(*)
		FROM traffic_snapshots
		WHERE site_id = $1 AND time >= $2 AND time < $3`,
		site, from, to,
	).Scan(&out.PeakRequestRate, &out.AvgRequestRate, &out.Snapshots)
	if err != nil {
		t.Fatalf("oracle rates: %v", err)
	}
	err = pool.QueryRow(ctx, `
		WITH per_flush AS (
		    SELECT time, sum(prev_window_count + curr_window_count) AS window_requests
		    FROM traffic_snapshots
		    WHERE site_id = $1 AND time >= $2 AND time < $3
		    GROUP BY time
		)
		SELECT COALESCE(max(window_requests), 0) FROM per_flush`,
		site, from, to,
	).Scan(&out.PeakWindow)
	if err != nil {
		t.Fatalf("oracle peak window: %v", err)
	}
	return out
}

// closeEnough compares the two answers and says which figure parted.
func closeEnough(t *testing.T, what string, got, want aggregable) {
	t.Helper()
	if got.Snapshots != want.Snapshots {
		t.Errorf("%s: the rollup path counted %d snapshots, the raw table %d.\n"+
			"A count is exact in both places or one of them is losing rows, and "+
			"a lost row is a quiet minute the customer never had",
			what, got.Snapshots, want.Snapshots)
	}
	if got.PeakWindow != want.PeakWindow {
		t.Errorf("%s: the rollup path reports a peak window of %d, the raw table %d.\n"+
			"This is the figure the rollup could hold only because a maximum of "+
			"per-flush totals is the maximum of the per-bucket maxima of those "+
			"totals. If they differ, that identity is not being preserved",
			what, got.PeakWindow, want.PeakWindow)
	}
	for _, f := range []struct {
		name       string
		got, want  float64
		whyItCould string
	}{
		{"peak request rate", got.PeakRequestRate, want.PeakRequestRate,
			"a maximum does not accumulate error, so any difference here is a " +
				"range boundary landing in a different place, not arithmetic"},
		{"average request rate", got.AvgRequestRate, want.AvgRequestRate,
			"stored as a sum over a count on purpose: an average of averages " +
				"would weight a quiet bucket the same as a busy one, and would " +
				"be wrong by far more than a tolerance"},
	} {
		if !within(f.got, f.want, rateTolerance) {
			t.Errorf("%s: the rollup path reports a %s of %v, the raw table %v.\n%s",
				what, f.name, f.got, f.want, f.whyItCould)
		}
	}
}

func within(a, b, tol float64) bool {
	if a == b {
		return true
	}
	scale := math.Max(math.Abs(a), math.Abs(b))
	return math.Abs(a-b) <= tol*scale
}

// seedRollupTraffic writes rows the way a collector does: several IPs per
// flush, all sharing that flush's timestamp, at an irregular cadence.
//
// Irregular on purpose. The peak-window figure is a maximum over flush
// events, and a fixture that flushed on an exact grid would let a rollup
// that grouped by something else - the bucket, say, rather than the flush
// - agree with the oracle by accident.
func seedRollupTraffic(t *testing.T, admin *pgxpool.Pool, site string,
	start time.Time, flushes int) {
	t.Helper()
	ctx := context.Background()

	// Offsets chosen so that flushes land at uneven spacings and cross
	// bucket boundaries, including two inside one minute.
	gaps := []time.Duration{
		0, 7 * time.Second, 13 * time.Second, 4 * time.Minute,
		11 * time.Minute, 3 * time.Second, 17 * time.Minute,
	}
	at := start
	for i := 0; i < flushes; i++ {
		at = at.Add(gaps[i%len(gaps)])
		// Three IPs at this flush, with rates and window counters that
		// differ per IP so that a sum and a maximum cannot be confused.
		for ip := 1; ip <= 3; ip++ {
			if _, err := admin.Exec(ctx, `
				INSERT INTO traffic_snapshots
				  (time, site_id, ip, ja4, prev_window_count, curr_window_count,
				   request_rate, bot_score, is_known_bot_ja4)
				VALUES ($1, $2, $3, 't13d1516h2_8daaf6152771_b186095e22b6',
				        $4, $5, $6, $7, false)`,
				at, site, fmt.Sprintf("198.51.100.%d", ip),
				i%5, ip*2, float64(i%9)+float64(ip)/4, (i*7+ip*11)%100,
			); err != nil {
				t.Fatalf("seeding flush %d ip %d: %v", i, ip, err)
			}
		}
	}
}

// TestTheRollupAndTheRawTableNeverDisagree is the phase's own finish
// condition, asked over ranges that stress every part of the split.
func TestTheRollupAndTheRawTableNeverDisagree(t *testing.T) {
	ctx := context.Background()
	admin := testdb.Admin(t)
	const site = "rollup-agree"
	testdb.CleanSite(t, admin, site)

	// Two days back, so most buckets are settled and the newest are not.
	start := time.Now().UTC().Add(-48 * time.Hour).Truncate(storage.RollupBucket)
	seedRollupTraffic(t, admin, site, start, 240)

	roll := storage.NewRollup(testdb.Pool(t, testdb.Collector))
	report, err := roll.Refresh(ctx, site)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if !report.Rolled() {
		t.Fatalf("refresh materialized nothing (%s), so nothing below tests the "+
			"rollup path - it tests the fallback", report.Skipped)
	}
	t.Logf("materialized %d buckets, watermark %s", report.Buckets,
		report.Through.Format(time.RFC3339))

	// The rollup has to have taken part, or every case below passes
	// against a path that never read it.
	var rows int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM traffic_rollup WHERE site_id = $1`, site).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows == 0 {
		t.Fatal("no rollup rows for this site, so the assertions below would hold " +
			"even if the rollup were never consulted")
	}
	t.Logf("%d rollup rows", rows)

	store := &Store{pool: testdb.Pool(t, testdb.Reader)}
	watermark, err := store.rollupWatermark(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	if watermark.IsZero() {
		t.Fatal("no watermark, so the read path will answer from the raw table alone")
	}

	splitFrom, splitTo := splittingInstants(ctx, t, admin, site)
	t.Logf("mid-bucket bounds derived from the rows: %s .. %s",
		splitFrom.Format(time.RFC3339), splitTo.Format(time.RFC3339))

	for _, c := range []struct {
		name     string
		from, to time.Time
		why      string
	}{
		{
			name: "the whole seeded range, ending in the unrolled tail",
			from: start.Add(-time.Hour), to: time.Now().UTC(),
			why: "the ordinary dashboard question: some of it materialized, the " +
				"newest of it not, and the two halves have to add up",
		},
		{
			name: "entirely inside the materialized part",
			from: start, to: watermark.Add(-2 * storage.RollupBucket),
			why: "the case the rollup exists for. If this one disagrees, the " +
				"aggregation itself is wrong rather than the seam",
		},
		{
			name: "entirely inside the unrolled tail",
			from: watermark, to: time.Now().UTC().Add(time.Hour),
			why: "no rollup row may contribute here, and the answer must be the " +
				"same one the endpoint gave before this phase",
		},
		{
			name: "straddling the watermark",
			from: watermark.Add(-3 * storage.RollupBucket), to: watermark.Add(3 * storage.RollupBucket),
			why: "the seam. A bucket counted on both sides doubles it and a " +
				"bucket counted on neither loses it, and both look plausible",
		},
		{
			name: "bounds mid-bucket, which the API permits",
			from: start.Add(7 * time.Minute), to: watermark.Add(-83 * time.Second),
			why: "the panel's own bounds are local midnights, but the API takes " +
				"any instant. A rollup bucket that is only partly inside the " +
				"range belongs to neither end, so the raw table must cover both " +
				"ragged edges",
		},
		{
			// This case exists because a mutation survived the one above,
			// twice.
			//
			// Moving the end of the rollup's span from alignDown to
			// alignUp makes it claim a bucket only partly inside the
			// range, and so count rows past the range's end. It changed
			// no answer - not because the rule holds but because the
			// fixture had nothing in the part of the bucket that would
			// have been wrongly included. The first attempt at a fix
			// picked a mid-bucket instant by arithmetic and landed in a
			// seventeen-minute gap between flushes, which is the same
			// failure with more confidence.
			//
			// So the boundary is *derived from the rows*: splitFrom and
			// splitTo are instants a second after a flush inside a
			// bucket that holds later flushes too. A bucket taken whole
			// instead of left to the raw table then counts those later
			// flushes, and the answer moves.
			name: "bounds mid-bucket, with rows on both sides of each edge",
			from: splitFrom, to: splitTo,
			why: "the same claim as above with data under it. An edge bucket " +
				"taken whole rather than left to the raw table counts rows from " +
				"outside the range, and nothing can see that unless there are " +
				"rows outside the range close enough to be pulled in",
		},
		{
			name: "a single bucket",
			from: start, to: start.Add(storage.RollupBucket),
			why: "the smallest range the rollup can answer at all, and the one " +
				"where an off-by-one in the alignment shows up as the whole answer",
		},
		{
			name: "a range with nothing in it",
			from: start.Add(-72 * time.Hour), to: start.Add(-71 * time.Hour),
			why: "zero has to come back as zero from both paths rather than as " +
				"a division by a count of nought",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := store.aggregableOver(ctx, site, c.from, c.to, watermark)
			if err != nil {
				t.Fatalf("rollup path: %v", err)
			}
			want := oracleSummary(ctx, t, admin, site, c.from, c.to)
			closeEnough(t, c.why, got, want)
		})
	}
}

// TestASecondRefreshChangesNothing.
//
// The refresh recomputes a window it has already written, so that a row
// arriving a little late still lands in the rollup. That makes it a
// statement run repeatedly over the same rows, and an upsert that
// accumulated instead of replacing would double every figure in that
// window - quietly, and only for the most recent hour, which is the part
// of the dashboard anybody actually looks at.
func TestASecondRefreshChangesNothing(t *testing.T) {
	ctx := context.Background()
	admin := testdb.Admin(t)
	const site = "rollup-idempotent"
	testdb.CleanSite(t, admin, site)

	start := time.Now().UTC().Add(-6 * time.Hour).Truncate(storage.RollupBucket)
	seedRollupTraffic(t, admin, site, start, 60)

	roll := storage.NewRollup(testdb.Pool(t, testdb.Collector))
	store := &Store{pool: testdb.Pool(t, testdb.Reader)}

	read := func() aggregable {
		t.Helper()
		wm, err := store.rollupWatermark(ctx, site)
		if err != nil {
			t.Fatal(err)
		}
		got, err := store.aggregableOver(ctx, site, start, time.Now().UTC(), wm)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	if _, err := roll.Refresh(ctx, site); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	first := read()
	for i := 0; i < 3; i++ {
		if _, err := roll.Refresh(ctx, site); err != nil {
			t.Fatalf("refresh %d: %v", i+2, err)
		}
	}
	again := read()

	if first != again {
		t.Errorf("four refreshes over the same rows gave %+v then %+v.\n"+
			"The refresh recomputes its most recent window every cycle, so it "+
			"runs over the same rows continuously in a deployment. An upsert "+
			"that added instead of replacing would inflate exactly the newest "+
			"hour, which is the part of the dashboard people watch", first, again)
	}
	// And against the raw table, so that "unchanged" cannot be satisfied
	// by being consistently wrong.
	closeEnough(t, "after four refreshes", again,
		oracleSummary(ctx, t, admin, site, start, time.Now().UTC()))
}

// TestTheWatermarkNeverClaimsBucketsItHasNotWritten.
//
// The one failure that makes the read path lie rather than be slow. If
// the watermark says a range is materialized and the rows are not there,
// the rollup answers for it and the missing rows read as quiet hours -
// no error, no gap, just smaller numbers.
func TestTheWatermarkNeverClaimsBucketsItHasNotWritten(t *testing.T) {
	ctx := context.Background()
	admin := testdb.Admin(t)
	const site = "rollup-watermark"
	testdb.CleanSite(t, admin, site)

	start := time.Now().UTC().Add(-30 * time.Hour).Truncate(storage.RollupBucket)
	seedRollupTraffic(t, admin, site, start, 120)

	// And a second batch running right up to the present, which is what
	// a live collector produces and what the first version of this test
	// did not have.
	//
	// Without it the newest seeded row was twenty hours old, so the
	// buckets between the settled horizon and now held nothing - and a
	// watermark set to now() instead of to the horizon claimed a stretch
	// of empty buckets, which is a claim about nothing. Measured: that
	// mutation survived. With rows in that stretch it is a claim about
	// rows, and a false one.
	seedRollupTraffic(t, admin, site, time.Now().UTC().Add(-40*time.Minute), 6)

	roll := storage.NewRollup(testdb.Pool(t, testdb.Collector))
	if _, err := roll.Refresh(ctx, site); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	var watermark time.Time
	if err := admin.QueryRow(ctx,
		`SELECT materialized_before FROM traffic_rollup_state WHERE site_id = $1`,
		site).Scan(&watermark); err != nil {
		t.Fatalf("reading the watermark: %v", err)
	}

	// Every bucket that has raw rows and starts before the watermark must
	// have a rollup row. Derived from the raw table rather than from a
	// count: a count would pass if the rollup held the right number of
	// the wrong buckets.
	rows, err := admin.Query(ctx, `
		SELECT b FROM (
		    SELECT DISTINCT time_bucket($3::interval, time) AS b
		      FROM traffic_snapshots
		     WHERE site_id = $1 AND time < $2
		) want
		WHERE NOT EXISTS (
		    SELECT 1 FROM traffic_rollup r
		     WHERE r.site_id = $1 AND r.bucket = want.b)
		ORDER BY b`,
		site, watermark, storage.RollupBucket)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var missing []time.Time
	for rows.Next() {
		var b time.Time
		if err := rows.Scan(&b); err != nil {
			t.Fatal(err)
		}
		missing = append(missing, b)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Errorf("the watermark claims everything before %s is materialized, and "+
			"%d bucket(s) with rows in them are not - the first is %s.\n"+
			"The read path believes the watermark, so those rows are simply "+
			"absent from every summary that covers them. No error is raised "+
			"anywhere: the numbers are just smaller",
			watermark.Format(time.RFC3339), len(missing),
			missing[0].Format(time.RFC3339))
	}

	// And the converse: no rollup row at or after the watermark, because
	// the read path answers that part from the raw table and would count
	// such a row twice if it ever consulted it.
	var ahead int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM traffic_rollup WHERE site_id = $1 AND bucket >= $2`,
		site, watermark).Scan(&ahead); err != nil {
		t.Fatal(err)
	}
	if ahead > 0 {
		t.Errorf("%d rollup row(s) sit at or after the watermark %s. The read "+
			"path takes everything from there on from the raw table, so such a "+
			"row is not read today - which makes it a trap for the next person "+
			"who widens the span rather than a fault now",
			ahead, watermark.Format(time.RFC3339))
	}
}

// TestGoAndPostgresPutTheBucketGridInTheSamePlace.
//
// The split is computed in Go with Truncate and the buckets are written
// in SQL with time_bucket. Two implementations of one grid, and if they
// disagree the read path asks the rollup for buckets whose boundaries
// are not where the rollup put them - so a summary would be short by
// whatever falls between the two grids, at every range, silently.
//
// Both origins are documented and both happen to be a multiple of a
// quarter-hour from the epoch. "Happen to be" is why this is a test.
func TestGoAndPostgresPutTheBucketGridInTheSamePlace(t *testing.T) {
	ctx := context.Background()
	admin := testdb.Admin(t)

	base := time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC)
	var checked int
	for _, off := range []time.Duration{
		0, time.Second, 59 * time.Second, 60 * time.Second,
		7*time.Minute + 30*time.Second, 14*time.Minute + 59*time.Second,
		15 * time.Minute, 15*time.Minute + 1*time.Second,
		23*time.Hour + 44*time.Minute, 25*time.Hour + 15*time.Minute,
		-1 * time.Second, -13 * time.Minute,
	} {
		at := base.Add(off)
		var pg time.Time
		if err := admin.QueryRow(ctx,
			`SELECT time_bucket($1::interval, $2::timestamptz)`,
			storage.RollupBucket, at).Scan(&pg); err != nil {
			t.Fatal(err)
		}
		if got := alignDown(at); !got.Equal(pg) {
			t.Errorf("at %s Go truncates to %s and PostgreSQL buckets to %s.\n"+
				"The read path uses the first to decide what to ask for and the "+
				"refresh uses the second to decide what to store, so a summary "+
				"would be short by everything between the two grids",
				at.Format(time.RFC3339), got.Format(time.RFC3339),
				pg.Format(time.RFC3339))
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no instants compared")
	}
}

// TestEveryTimezoneSitsOnTheBucketGrid.
//
// The bucket width is a quarter of an hour because a local day has to be
// a whole number of buckets, in every zone a customer might name. That
// is a claim about the tz database, not about arithmetic, so it is asked
// of the database rather than asserted here - and asked of every zone
// PostgreSQL knows rather than of a list somebody typed, because a list
// is exactly what would not contain the zone that broke it.
//
// If a future tz release brings back an offset off the grid - they
// existed until 1972 - this test is the only warning anybody gets, and
// what it would mean is that the rollup cannot serve that zone's days.
func TestEveryTimezoneSitsOnTheBucketGrid(t *testing.T) {
	ctx := context.Background()
	admin := testdb.Admin(t)

	// Sampled daily across the retention ceiling, so that both sides of
	// every daylight-saving transition in range are included. A single
	// instant would pass on a zone whose summer offset is off the grid.
	var zones, samples, offGrid int
	var example *string
	if err := admin.QueryRow(ctx, `
		WITH inst AS (
		    SELECT generate_series(now() - make_interval(days => $1::int),
		                           now() + interval '30 days',
		                           interval '1 day') AS t
		), off AS (
		    SELECT z.name,
		           extract(epoch FROM (i.t AT TIME ZONE z.name)
		                            - (i.t AT TIME ZONE 'UTC'))::bigint AS secs
		      FROM pg_timezone_names z CROSS JOIN inst i
		)
		SELECT count(DISTINCT name), count(*),
		       count(*) FILTER (WHERE secs % $2 <> 0),
		       min(name) FILTER (WHERE secs % $2 <> 0)
		  FROM off`,
		730, int64(storage.RollupBucket/time.Second),
	).Scan(&zones, &samples, &offGrid, &example); err != nil {
		t.Fatalf("asking the tz database: %v", err)
	}

	if zones < 100 || samples < 10000 {
		t.Fatalf("only %d zones over %d samples, which is too few to be the tz "+
			"database - this test would pass on an empty one", zones, samples)
	}
	t.Logf("%d zones, %d samples", zones, samples)

	if offGrid > 0 {
		name := "(unknown)"
		if example != nil {
			name = *example
		}
		t.Errorf("%d of %d zone/instant pairs have a UTC offset that is not a "+
			"whole number of %v buckets - %s among them.\n"+
			"A local midnight in such a zone falls inside a bucket rather than "+
			"on its edge, so that zone's day cannot be summed out of the rollup "+
			"and its dashboard would be wrong by part of a bucket at each end. "+
			"The bucket width is what would have to change",
			offGrid, samples, storage.RollupBucket, name)
	}
}

// TestOnlyTheCollectorMayWriteTheRollup.
//
// The rollup is a table one service maintains and another service reads,
// and both of them run the same retention code. So the question is not
// whether the grants were written but whether they were written to the
// right role: beacon_writer runs an identical cycle against its own
// table, and if it could write this one, one service's cycle could
// rewrite the other's numbers.
//
// Asked of the database rather than of grants.sql, because what a role
// may do is the state of the database and not the text of a file. Four
// roles, and the expectations are opposite in pairs so that a blanket
// GRANT and a blanket REVOKE both fail this.
func TestOnlyTheCollectorMayWriteTheRollup(t *testing.T) {
	ctx := context.Background()
	admin := testdb.Admin(t)
	const site = "rollup-grants"
	testdb.CleanSite(t, admin, site)

	bucket := time.Now().UTC().Add(-24 * time.Hour).Truncate(storage.RollupBucket)

	for _, c := range []struct {
		role              string
		mayRead, mayWrite bool
		whyRead, whyWrite string
	}{
		{
			role: testdb.Collector, mayRead: true, mayWrite: true,
			whyRead:  "the upsert reads back its own conflict target",
			whyWrite: "this is the role whose cycle maintains the table",
		},
		{
			role: testdb.Reader, mayRead: true, mayWrite: false,
			whyRead: "the read API answers summaries out of it",
			whyWrite: "the read API writes nothing anywhere, and a rollup it " +
				"could write is a rollup a request could rewrite",
		},
		{
			role: testdb.Beacon, mayRead: false, mayWrite: false,
			whyRead: "it never reads back what it wrote, in any table",
			whyWrite: "it runs the same retention cycle against its own table. " +
				"If it could write here, one service's housekeeping could " +
				"rewrite the other service's numbers - which is the separation " +
				"the whole deployment rests on",
		},
		{
			role: testdb.Panel, mayRead: false, mayWrite: false,
			whyRead:  "the panel never touches the analytics tables directly",
			whyWrite: "the same, and more so",
		},
	} {
		t.Run(c.role, func(t *testing.T) {
			pool := testdb.Pool(t, c.role)

			var n int
			readErr := pool.QueryRow(ctx,
				`SELECT count(*) FROM traffic_rollup WHERE site_id = $1`, site).Scan(&n)
			if c.mayRead && readErr != nil {
				t.Errorf("%s cannot read traffic_rollup: %v\nIt has to: %s",
					c.role, readErr, c.whyRead)
			}
			if !c.mayRead && readErr == nil {
				t.Errorf("%s can read traffic_rollup, and should not: %s",
					c.role, c.whyRead)
			}

			_, writeErr := pool.Exec(ctx, `
				INSERT INTO traffic_rollup (site_id, bucket, snapshots, sum_rate, max_rate, max_window)
				VALUES ($1, $2, 1, 1, 1, 1)
				ON CONFLICT (site_id, bucket) DO UPDATE SET snapshots = 99`,
				site, bucket)
			if c.mayWrite && writeErr != nil {
				t.Errorf("%s cannot write traffic_rollup: %v\nIt has to: %s",
					c.role, writeErr, c.whyWrite)
			}
			if !c.mayWrite && writeErr == nil {
				t.Errorf("%s can write traffic_rollup, and must not: %s",
					c.role, c.whyWrite)
			}
			if writeErr == nil {
				if _, err := admin.Exec(ctx,
					`DELETE FROM traffic_rollup WHERE site_id = $1`, site); err != nil {
					t.Fatalf("clearing the probe row: %v", err)
				}
			}
		})
	}
}

// TestPruningTheRollupFollowsTheRowsItSummarizes.
//
// Without this the rollup is a second, longer history. A deployment
// keeping thirty days of snapshots would keep months of rollup, and
// since the dashboard reads the rollup for long ranges, it would draw
// traffic for weeks whose detail pages are empty - two answers about the
// same week and no way to tell which one is the product's.
//
// Asserted through the read path as well as against the table, because a
// prune that removed the rows and left the watermark claiming them would
// pass a row count and fail a customer.
func TestPruningTheRollupFollowsTheRowsItSummarizes(t *testing.T) {
	ctx := context.Background()
	admin := testdb.Admin(t)
	const site = "rollup-prune"
	const keepDays = 3
	testdb.CleanSite(t, admin, site)

	// Rows on both sides of the retention age, because an age limit
	// cannot be tested with data that only exists on one side of it.
	old := time.Now().UTC().Add(-10 * 24 * time.Hour).Truncate(storage.RollupBucket)
	recent := time.Now().UTC().Add(-2 * time.Hour).Truncate(storage.RollupBucket)
	seedRollupTraffic(t, admin, site, old, 40)
	seedRollupTraffic(t, admin, site, recent, 8)

	roll := storage.NewRollup(testdb.Pool(t, testdb.Collector))
	if _, err := roll.Refresh(ctx, site); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	before := rollupRowsAt(t, admin, site)
	if before.older == 0 {
		t.Fatal("no rollup rows older than the retention age, so a prune that " +
			"deleted nothing would pass this test")
	}
	if before.newer == 0 {
		t.Fatal("no rollup rows inside the retention age, so a prune that " +
			"deleted everything would pass this test too")
	}

	n, err := roll.Prune(ctx, site, keepDays)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	t.Logf("pruned %d of %d rollup rows", n, before.older+before.newer)

	after := rollupRowsAt(t, admin, site)
	if after.older != 0 {
		t.Errorf("%d rollup row(s) older than %d days survived the prune.\n"+
			"Retention will drop the snapshots behind them, and then the "+
			"dashboard reads traffic out of the rollup for a range whose "+
			"detail rows are gone", after.older, keepDays)
	}
	if after.newer != before.newer {
		t.Errorf("the prune took %d row(s) from inside the retention window "+
			"(%d before, %d after). Those are the rows the rollup exists to "+
			"hold", before.newer-after.newer, before.newer, after.newer)
	}

	// And the read path still agrees with the raw table for the range
	// that is left, which is the only thing a customer can check.
	store := &Store{pool: testdb.Pool(t, testdb.Reader)}
	wm, err := store.rollupWatermark(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	from := time.Now().UTC().Add(-keepDays * 24 * time.Hour).Truncate(storage.RollupBucket)
	got, err := store.aggregableOver(ctx, site, from, time.Now().UTC(), wm)
	if err != nil {
		t.Fatal(err)
	}
	closeEnough(t, "after pruning", got,
		oracleSummary(ctx, t, admin, site, from, time.Now().UTC()))
}

type rollupSplit struct{ older, newer int }

// rollupRowsAt counts this site's rollup rows either side of a
// three-day age, derived from the table rather than from what the seed
// intended.
func rollupRowsAt(t *testing.T, admin *pgxpool.Pool, site string) rollupSplit {
	t.Helper()
	var out rollupSplit
	if err := admin.QueryRow(context.Background(), `
		SELECT count(*) FILTER (WHERE bucket <  now() - interval '3 days'),
		       count(*) FILTER (WHERE bucket >= now() - interval '3 days')
		  FROM traffic_rollup WHERE site_id = $1`, site).
		Scan(&out.older, &out.newer); err != nil {
		t.Fatalf("counting rollup rows: %v", err)
	}
	return out
}

// TestARowArrivingAfterTheRefreshIsStillCounted.
//
// The settle margin is a bet that a bucket stops receiving rows an hour
// after it ends, and a bet is not a guarantee: rows carry the
// collector's clock, and the collector may be a different machine. So
// the read path must not depend on the bet being right for the part of
// the range it has not claimed.
//
// A row landing inside the unmaterialized tail is the ordinary case -
// every row does, continuously - and it has to appear in the very next
// summary without a refresh having run at all. If it does not, the
// dashboard is stale rather than slow, and stale is the failure this
// design was chosen to avoid.
func TestARowArrivingAfterTheRefreshIsStillCounted(t *testing.T) {
	ctx := context.Background()
	admin := testdb.Admin(t)
	const site = "rollup-freshness"
	testdb.CleanSite(t, admin, site)

	start := time.Now().UTC().Add(-8 * time.Hour).Truncate(storage.RollupBucket)
	seedRollupTraffic(t, admin, site, start, 60)

	roll := storage.NewRollup(testdb.Pool(t, testdb.Collector))
	if _, err := roll.Refresh(ctx, site); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	store := &Store{pool: testdb.Pool(t, testdb.Reader)}
	wm, err := store.rollupWatermark(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	to := time.Now().UTC().Add(time.Hour)
	before, err := store.aggregableOver(ctx, site, start, to, wm)
	if err != nil {
		t.Fatal(err)
	}

	// One flush, now, with a rate above anything seeded so that the
	// maximum has to move as well as the count.
	const loudRate = 999.5
	if _, err := admin.Exec(ctx, `
		INSERT INTO traffic_snapshots
		  (time, site_id, ip, ja4, prev_window_count, curr_window_count,
		   request_rate, bot_score, is_known_bot_ja4)
		VALUES (now(), $1, '198.51.100.9', 't13d1516h2_8daaf6152771_b186095e22b6',
		        0, 1, $2, 10, false)`, site, loudRate); err != nil {
		t.Fatalf("the late flush: %v", err)
	}

	// No refresh. The same watermark, deliberately: this is what a
	// request arriving between two cycles sees.
	after, err := store.aggregableOver(ctx, site, start, to, wm)
	if err != nil {
		t.Fatal(err)
	}

	if after.Snapshots != before.Snapshots+1 {
		t.Errorf("a row written after the last refresh moved the snapshot count "+
			"from %d to %d, and one row was written.\nThe rollup does not claim "+
			"the newest buckets, so the raw table has to cover them - if it does "+
			"not, the dashboard is showing a number from the last cycle and "+
			"nothing says so", before.Snapshots, after.Snapshots)
	}
	if after.PeakRequestRate != loudRate {
		t.Errorf("the peak rate is %v after a flush at %v.\nA maximum that does "+
			"not move for a new maximum means the unrolled tail is not being "+
			"read at all", after.PeakRequestRate, loudRate)
	}
	closeEnough(t, "with a row newer than the watermark", after,
		oracleSummary(ctx, t, admin, site, start, to))
}

// splittingInstants finds two mid-bucket instants that a wrongly-aligned
// span would give away.
//
// The requirement is precise, and getting it wrong is what let a mutation
// through twice: the instant has to fall inside a bucket that holds at
// least one *later* flush. Then a span that takes the whole bucket
// instead of leaving the ragged part to the raw table counts those later
// flushes, and the answer moves. An instant in a bucket with nothing
// after it is a boundary no rule can be caught at.
//
// Derived from the rows rather than computed from the seed's arithmetic.
// The first attempt did the arithmetic and landed in a seventeen-minute
// gap between flushes, which looked exactly as convincing.
func splittingInstants(ctx context.Context, t *testing.T, admin *pgxpool.Pool,
	site string) (from, to time.Time) {
	t.Helper()

	rows, err := admin.Query(ctx, `
		WITH flushes AS (
		    SELECT DISTINCT time, time_bucket($2::interval, time) AS bucket
		      FROM traffic_snapshots WHERE site_id = $1
		), ranked AS (
		    SELECT time, bucket,
		           count(*) OVER (PARTITION BY bucket) AS in_bucket,
		           row_number() OVER (PARTITION BY bucket ORDER BY time) AS nth
		      FROM flushes
		)
		SELECT time FROM ranked
		 WHERE in_bucket >= 2 AND nth = 1
		 ORDER BY time`,
		site, storage.RollupBucket)
	if err != nil {
		t.Fatalf("looking for a bucket with more than one flush: %v", err)
	}
	defer rows.Close()
	var firsts []time.Time
	for rows.Next() {
		var at time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatal(err)
		}
		firsts = append(firsts, at)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(firsts) < 2 {
		t.Fatalf("only %d bucket(s) hold more than one flush, and two are needed "+
			"- one for each edge of the range. The fixture's flush cadence is "+
			"what decides this, so a change to the gaps in seedRollupTraffic "+
			"can quietly make this test unable to see anything", len(firsts))
	}

	// A second past the first flush in the bucket: inside the bucket,
	// after one flush, before the others.
	from = firsts[0].UTC().Add(time.Second)
	to = firsts[len(firsts)-1].UTC().Add(time.Second)
	for _, at := range []time.Time{from, to} {
		if at.Equal(at.Truncate(storage.RollupBucket)) {
			t.Fatalf("%s sits on a bucket boundary, so it is not a mid-bucket "+
				"bound and this case tests nothing it was written for",
				at.Format(time.RFC3339Nano))
		}
	}
	return from, to
}

// TestACatchUpMakesProgressInStepsThatCommit.
//
// A refresh is capped at rollupMaxSpan so that a deployment with months
// of existing rows does not depend on one long statement finishing. The
// cap is only worth having if each capped call *commits* what it did -
// otherwise it is the same unbounded catch-up in smaller words - so this
// asks for the two things that make it real: the watermark moves on
// every call, and the rows behind it are there.
//
// Seeded across more than one cap's worth of history, or the cap would
// never engage and this would be a test of the ordinary path with a
// longer fixture.
func TestACatchUpMakesProgressInStepsThatCommit(t *testing.T) {
	ctx := context.Background()
	admin := testdb.Admin(t)
	const site = "rollup-catchup"
	testdb.CleanSite(t, admin, site)

	// 100 days back, against a 30-day cap: four calls to reach the
	// present, and the first three must each stop short and commit.
	start := time.Now().UTC().Add(-100 * 24 * time.Hour).Truncate(storage.RollupBucket)
	for _, at := range []time.Time{
		start,
		start.Add(35 * 24 * time.Hour),
		start.Add(70 * 24 * time.Hour),
		time.Now().UTC().Add(-3 * time.Hour),
	} {
		seedRollupTraffic(t, admin, site, at, 12)
	}

	roll := storage.NewRollup(testdb.Pool(t, testdb.Collector))
	store := &Store{pool: testdb.Pool(t, testdb.Reader)}

	var last time.Time
	var calls int
	for {
		report, err := roll.Refresh(ctx, site)
		if err != nil {
			t.Fatalf("refresh %d: %v", calls+1, err)
		}
		calls++
		if report.Skipped != "" {
			t.Fatalf("refresh %d skipped (%s) before reaching the present, so the "+
				"catch-up stalled rather than finished", calls, report.Skipped)
		}

		// Committed: the watermark the read path will believe is on disk
		// now, not at the end of the loop.
		wm, err := store.rollupWatermark(ctx, site)
		if err != nil {
			t.Fatal(err)
		}
		if !wm.After(last) {
			t.Fatalf("call %d left the watermark at %s, where call %d already had "+
				"it. A capped refresh that does not advance is an unbounded one "+
				"with extra steps: the deployment would never catch up",
				calls, wm.Format(time.RFC3339), calls-1)
		}
		last = wm

		// And correct so far, over everything it now claims.
		got, err := store.aggregableOver(ctx, site, start, wm, wm)
		if err != nil {
			t.Fatal(err)
		}
		closeEnough(t, fmt.Sprintf("after catch-up call %d", calls), got,
			oracleSummary(ctx, t, admin, site, start, wm))

		if report.CaughtUpTo.IsZero() {
			break
		}
		if calls > 10 {
			t.Fatalf("still catching up after %d calls over 100 days of history, "+
				"which is more than the cap can account for", calls)
		}
	}

	if calls < 2 {
		t.Fatalf("the whole catch-up took %d call(s), so the cap never engaged and "+
			"nothing here tested it. The fixture has to span more than one cap's "+
			"worth of history", calls)
	}
	t.Logf("caught up over 100 days in %d calls", calls)
}
