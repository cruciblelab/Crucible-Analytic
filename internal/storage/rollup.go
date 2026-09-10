package storage

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RollupBucket is the rollup's bucket width.
//
// Quarter-hour, and the reason is measured rather than chosen: since O2a
// a query buckets in the customer's own zone, so a local day boundary
// lands wherever their UTC offset puts it. A rollup bucket can only be
// summed into a local day if every offset is a whole number of buckets,
// and every zone PostgreSQL knows is a whole number of quarter-hours
// away from UTC - 499 zones, sampled daily across the retention window,
// zero exceptions. internal/api holds that as a test against the real
// database, with the list read from pg_timezone_names.
//
// Named here rather than written into the SQL twice, because the refresh
// and the read path have to agree on it exactly and a mismatch would not
// fail: it would answer with buckets that do not line up.
const RollupBucket = 15 * time.Minute

// rollupSettle is how far behind the present the rollup stops.
//
// A materialized bucket is treated as final, so it must not still be
// receiving rows. A traffic_snapshots row carries the *collector's*
// clock - internal/storage.BuildRows stamps every row in a flush with
// that flush's time - and the collector may be a different machine from
// the database. So "this bucket is over" is not a question the database
// clock alone can answer.
//
// An hour, against a flush interval measured in seconds. It is not a
// bound on clock skew, because there is no such bound; it is a margin
// wide enough that reaching it means something is wrong that a rollup
// would not be the right place to notice.
//
// What it costs when it is not enough is bounded and stated: a row that
// arrives for a bucket already behind the watermark is not counted in
// the rollup, and no later refresh looks at that bucket again. It is not
// a correctness hole in the *read* path - the split is stored, so the
// raw table always covers everything the rollup does not claim - but it
// is a way for a rolled-up bucket to be short. The divergence test in
// internal/api is what would catch it having happened.
const rollupSettle = time.Hour

// rollupRecheck is how far back a refresh recomputes buckets it has
// already written.
//
// The watermark is pulled back by this much on every cycle, so a row
// arriving within an hour of the boundary still lands in the rollup.
// Bounded work: four buckets an hour, whatever the size of the table.
const rollupRecheck = time.Hour

// rollupMaxSpan is the most history one refresh will materialize.
//
// # Why there is a cap at all - measured
//
// The first refresh against an existing table has to roll up everything
// in it. Measured on 11 million rows over 90 days: 31,7 s in one
// statement. That is fine. The retention ceiling is 730 days, and a
// deployment at that ceiling with a table of that density is an
// uncapped statement running for minutes.
//
// Slow is not the problem - this runs in a background goroutine and
// takes only ACCESS SHARE, so the collector keeps writing throughout.
// The problem is that an uncapped refresh commits nothing until it
// finishes, so one that is interrupted - a restart, a statement
// timeout, an operator - has made no progress at all, and the next
// attempt starts from the same place. A catch-up that can be
// interrupted is a catch-up that may never complete.
//
// Thirty days per call, so each call commits. At the hourly default a
// deployment at the retention ceiling is caught up inside a day, and
// every cycle in between leaves the rollup further along than it found
// it. What the customer sees meanwhile is the behaviour they already
// had: the read path answers the uncovered part from the raw table.
const rollupMaxSpan = 30 * 24 * time.Hour

// RollupReport is what one site's refresh did.
type RollupReport struct {
	SiteID string
	// From and Through are the bucket range recomputed, and Through is
	// the new watermark: every bucket before it is materialized.
	From, Through time.Time
	// Buckets counts the rows written or rewritten.
	Buckets int64
	// CaughtUpTo is set, and equal to Through, when this call hit
	// rollupMaxSpan and so stopped short of the settled horizon. Zero
	// means it reached it.
	//
	// Reported rather than inferred from Through, because "as far as it
	// could go" and "as far as there was to go" are the same instant on
	// every ordinary cycle and different ones only while a deployment is
	// catching up - which is exactly when somebody is reading the log.
	CaughtUpTo time.Time
	// Skipped explains why nothing was done, when nothing was.
	Skipped string
}

// Rolled reports whether this refresh materialized anything.
func (r RollupReport) Rolled() bool { return r.Skipped == "" }

// Rollup keeps traffic_rollup in step with traffic_snapshots.
//
// It runs as the collector's own role and needs no elevated wrapper: the
// two statements read a table the collector already reads and write a
// table derived from it. That is deliberately different from retention,
// whose wrappers exist because they call TimescaleDB's policy functions
// and delete another site's rows - operations the collector must not
// hold outright. Nothing here is one, so nothing here is SECURITY
// DEFINER.
type Rollup struct {
	pool *pgxpool.Pool
	// now is the clock, injectable so a test can place the settle margin
	// somewhere it can see.
	now func() time.Time
}

// NewRollup builds a refresher over an existing pool.
func NewRollup(pool *pgxpool.Pool) *Rollup {
	return &Rollup{pool: pool, now: time.Now}
}

// Refresh brings one site's rollup up to the settled horizon.
func (r *Rollup) Refresh(ctx context.Context, siteID string) (RollupReport, error) {
	out := RollupReport{SiteID: siteID}
	if siteID == "" {
		return out, fmt.Errorf("storage: rollup: empty site id")
	}

	// The horizon: the start of the newest bucket that is already
	// settled. Computed in SQL rather than in Go so that the truncation
	// uses the same time_bucket the refresh and the read path use - three
	// callers, one definition.
	var horizon time.Time
	if err := r.pool.QueryRow(ctx,
		`SELECT time_bucket($1::interval, $2::timestamptz)`,
		RollupBucket, r.now().Add(-rollupSettle),
	).Scan(&horizon); err != nil {
		return out, fmt.Errorf("storage: rollup horizon: %w", err)
	}

	// Where to start. The stored watermark pulled back by the recheck
	// window, or - the first time this site is seen - the site's oldest
	// row, so an existing table is rolled up from its beginning rather
	// than from today.
	var from *time.Time
	if err := r.pool.QueryRow(ctx, `
		SELECT COALESCE(
		    (SELECT materialized_before - $2::interval FROM traffic_rollup_state WHERE site_id = $1),
		    (SELECT time_bucket($3::interval, min(time)) FROM traffic_snapshots WHERE site_id = $1))`,
		siteID, rollupRecheck, RollupBucket,
	).Scan(&from); err != nil {
		return out, fmt.Errorf("storage: rollup start: %w", err)
	}
	if from == nil {
		// No rows for this site and no watermark: nothing to roll up,
		// and no watermark written either. Writing one would claim that
		// buckets before the horizon are materialized, which for a site
		// whose rows arrive tomorrow would be a claim that the read path
		// believes.
		out.Skipped = "no rows for this site yet"
		return out, nil
	}
	if !from.Before(horizon) {
		out.From, out.Through = *from, *from
		out.Skipped = "already materialized up to the settled horizon"
		return out, nil
	}
	// Capped, so this call commits and the next one carries on. Reported
	// on the report rather than hidden, because a deployment catching up
	// is a state an operator reading the log should be able to see
	// ending.
	if capped := from.Add(rollupMaxSpan); capped.Before(horizon) {
		horizon = capped
		out.CaughtUpTo = horizon
	}
	out.From, out.Through = *from, horizon

	// One statement, and the aggregate is the same one the read path
	// computes from the raw table for its own unrolled tail. They are
	// written out twice - here and in internal/api - and the divergence
	// test is what holds them together; see the comment there for why a
	// shared string was not the answer.
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO traffic_rollup (site_id, bucket, snapshots, sum_rate, max_rate, max_window)
		WITH per_flush AS (
		    SELECT time_bucket($4::interval, time) AS bucket,
		           time,
		           count(*)                                        AS rows_at_flush,
		           sum(request_rate)                               AS sum_rate,
		           max(request_rate)                               AS max_rate,
		           sum(prev_window_count + curr_window_count)      AS window_requests
		      FROM traffic_snapshots
		     WHERE site_id = $1 AND time >= $2 AND time < $3
		     GROUP BY 1, 2
		)
		SELECT $1, bucket,
		       sum(rows_at_flush),
		       sum(sum_rate),
		       max(max_rate),
		       max(window_requests)
		  FROM per_flush
		 GROUP BY bucket
		ON CONFLICT (site_id, bucket) DO UPDATE SET
		    snapshots  = excluded.snapshots,
		    sum_rate   = excluded.sum_rate,
		    max_rate   = excluded.max_rate,
		    max_window = excluded.max_window`,
		siteID, *from, horizon, RollupBucket)
	if err != nil {
		return out, fmt.Errorf("storage: rollup upsert: %w", err)
	}
	out.Buckets = tag.RowsAffected()

	// The watermark moves only after the buckets are written, and in the
	// same statement order every time. A watermark ahead of the rows it
	// claims is the one failure that makes the read path lie rather than
	// merely be slow: the rollup would answer for a range it has not
	// materialized and the missing rows would look like quiet hours.
	if _, err := r.pool.Exec(ctx, `
		INSERT INTO traffic_rollup_state (site_id, materialized_before, refreshed_at)
		VALUES ($1, $2, now())
		ON CONFLICT (site_id) DO UPDATE SET
		    materialized_before = excluded.materialized_before,
		    refreshed_at        = excluded.refreshed_at
		 WHERE traffic_rollup_state.materialized_before < excluded.materialized_before`,
		siteID, horizon); err != nil {
		return out, fmt.Errorf("storage: rollup watermark: %w", err)
	}
	return out, nil
}

// Prune drops rollup buckets older than the raw rows they summarize.
//
// # Why this exists, and what it stops
//
// The rollup is a second place that holds the same history, and the
// retention sweep deletes the first. Without this, a deployment keeping
// 30 days of snapshots would keep months of rollup - and the dashboard,
// which reads the rollup for long ranges, would show traffic for days
// whose detail pages are empty. Two answers about the same week, and no
// way for a customer to tell which one is the product's.
//
// So the rollup is pruned by the same age the raw table is, and the
// caller passes the same number it passed to retention. It is a separate
// call rather than part of the retention wrapper because the wrapper
// runs as schema_admin over both services' tables, and this table
// belongs to one of them.
func (r *Rollup) Prune(ctx context.Context, siteID string, keepDays int) (int64, error) {
	if keepDays <= 0 {
		return 0, fmt.Errorf("storage: rollup prune: %d days is not a retention", keepDays)
	}
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM traffic_rollup
		 WHERE site_id = $1
		   AND bucket < time_bucket($3::interval, now() - make_interval(days => $2::int))`,
		siteID, keepDays, RollupBucket)
	if err != nil {
		return 0, fmt.Errorf("storage: rollup prune: %w", err)
	}
	return tag.RowsAffected(), nil
}

// LogRefresh brings one site's rollup up to date, prunes what retention
// has dropped, and says what happened - once, at the level the outcome
// deserves.
//
// Here rather than in the collector's main, for the reason
// retention.LogApply gives about itself: which outcome is worth which
// level is a judgement, and a second copy of it is a second chance for
// one of them to start reporting an ordinary state as a fault.
//
// # What a failure here means, and why it is a warning
//
// Nothing. The read path answers from the raw table for everything the
// rollup does not claim, so a refresh that never runs leaves a
// deployment exactly as fast as it was before O2 - slow on long ranges,
// correct on all of them. That is why this is logged and the process
// carries on: it runs inside the collector, which is on the traffic
// path, and stopping a collector to fix a dashboard's speed would be the
// wrong trade in every direction.
func (r *Rollup) LogRefresh(ctx context.Context, logger *slog.Logger, siteID string, keepDays int) {
	report, err := r.Refresh(ctx, siteID)
	switch {
	case err != nil:
		logger.Warn("rollup: could not refresh, so long ranges in the panel will "+
			"stay as slow as they were", "site", siteID, "err", err)
		return
	case report.Skipped != "":
		// Not logged. "Already up to date" is what almost every cycle
		// after the first says, and an hourly line saying so is a line
		// nobody reads in the file that matters on the day something
		// goes wrong.
	case !report.CaughtUpTo.IsZero():
		logger.Info("rollup: still catching up, and further along than last cycle",
			"site", siteID, "buckets", report.Buckets,
			"through", report.Through.UTC().Format(time.RFC3339))
	default:
		logger.Info("rollup: buckets materialized", "site", siteID,
			"buckets", report.Buckets,
			"through", report.Through.UTC().Format(time.RFC3339))
	}

	// Pruned on the same cycle and by the same number retention was
	// given, because a rollup that outlives the rows it summarizes is a
	// second, longer history: the dashboard would draw traffic for weeks
	// whose detail pages are empty, and a customer would have no way to
	// tell which of the two the product meant.
	n, err := r.Prune(ctx, siteID, keepDays)
	if err != nil {
		logger.Warn("rollup: could not prune, so the rollup may now cover a longer "+
			"history than the rows it summarizes", "site", siteID,
			"keep_days", keepDays, "err", err)
		return
	}
	if n > 0 {
		logger.Info("rollup: buckets pruned", "site", siteID, "buckets", n,
			"keep_days", keepDays)
	}
}
