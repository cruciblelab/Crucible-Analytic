package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/scoring"
)

// SketchPrecision is the number of registers in every visitor sketch.
//
// # Why 65.536, measured rather than chosen
//
// A HyperLogLog trades size for accuracy along a curve, and the owner's
// criterion was "as little error as possible, with a sensible ratio". So
// the curve was measured: four precisions, twenty genuinely different
// draws each, in the dense regime - which is where a growing site ends
// up, and the only regime whose error a product can promise.
//
//	p=4096     1,15 % avg   2,08 % worst    279 kB/90d    5,5 ms merge
//	p=16384    0,57 % avg   1,38 % worst   1,08 MB/90d   19,5 ms merge
//	p=65536    0,36 %                      4,40 MB/90d   87,8 ms merge
//	p=262144   (never left sparse)         8,40 MB/90d   9.537 ms merge
//
// This one halves p=16384's error while both of its cost ratios stay
// small: 0,55 % of what the raw table takes for the same site and
// window, and 1,8 % of the 5 s the panel's client will wait. The next
// step up was excluded by measurement rather than by taste - its merge
// alone eats the whole budget.
//
// # The trap that decided it
//
// p=65536 first measured 0,010 % error on a 451.000-address union, forty
// times better than theory. Rather than believe it, the merged sketch's
// byte size was asked for: 733 kB against a dense 48 kB, so it had
// stayed *sparse*, and a sparse HyperLogLog is very nearly exact. That
// accuracy disappears as a site grows - the same precision falls to
// dense at a few million addresses and lands back at 0,36 % - and
// staying sparse is also what makes the merge slow. Choosing on it would
// have been choosing a number that quietly gets worse as a customer
// succeeds.
const SketchPrecision = 65536

// SketchBucket is the width of one stored sketch: one UTC day.
//
// # Why a day, when traffic_rollup needed quarter-hours
//
// The rollup's grid has to divide a local midnight, because its buckets
// are all the read path has. A sketch is three orders of magnitude
// bigger than four numbers - 48 kB each - so the same grid would mean
// 4,4 MB per site per day and 8.640 sketches merged per 90-day query.
//
// A day works because the ragged ends a local midnight leaves - at most
// two partial UTC days - are read from the raw table and turned into
// sketches of their own before the merge. Sketches union, so an address
// appearing both in an edge and in a whole day is counted once. That is
// the property this whole design rests on; see internal/api/sketch.go.
//
// Not a time.Duration, and deliberately: a day is not 24 hours in every
// zone, and the one place that would matter - a customer's day boundary
// - is handled by those edges rather than by arithmetic here. What this
// names is a UTC calendar day, which time_bucket computes and Go's
// Truncate does not.
const SketchBucket = "1 day"

// sketchSettle is how far behind the present the sketch stops.
//
// The reasoning is rollupSettle's, unchanged: a materialized bucket is
// treated as final, and a traffic_snapshots row carries the collector's
// clock rather than the database's, so "this bucket is over" is not a
// question either clock answers alone. An hour is not a bound on skew -
// there is none - but a margin wide enough that reaching it means
// something is wrong a sketch is not the right place to notice.
const sketchSettle = time.Hour

// sketchRecheck is how far back a refresh recomputes days it has already
// written.
//
// One day rather than the rollup's hour, because a bucket here is a day:
// pulling the watermark back by less than one would recompute nothing.
// So each cycle rewrites the most recently settled day, which gives a
// row arriving with up to a day of skew somewhere to land.
//
// Bounded work, and bounded by the data rather than by the history: one
// day's rows per cycle whatever the size of the table.
const sketchRecheck = 24 * time.Hour

// sketchMaxSpan is the most history one refresh will materialize.
//
// Thirty days, for the reason rollupMaxSpan gives at length: an uncapped
// catch-up commits nothing until it finishes, so one that is interrupted
// has made no progress and the next attempt starts from the same place.
//
// Measured on 11,0 million rows over 91 days: the whole backfill is one
// statement of 19 s, so a 30-day call is a few seconds and every cycle
// leaves the sketch further along than it found it. What the customer
// sees meanwhile is what they see today - the read path answers the
// uncovered part exactly, from the raw table.
const sketchMaxSpan = 30 * 24 * time.Hour

// IsMissingTable reports whether err is PostgreSQL's undefined_table.
//
// Matched on the structured code rather than on the message text: the
// message is localised and has been reworded between releases, and this
// decides whether a deployment gets a slow-but-correct answer or an
// error page.
//
// Here rather than beside each caller, because there are two - this
// package's prune and internal/api's rollup watermark - and a second
// copy of a predicate nobody compares is the shape this project has
// found broken more than once.
func IsMissingTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}

// SketchReport is what one site's refresh did.
type SketchReport struct {
	SiteID string
	// From and Through are the day range recomputed, and Through is the
	// new watermark: every day before it is materialized.
	From, Through time.Time
	// Days counts the sketches written or rewritten.
	Days int64
	// CaughtUpTo is set, and equal to Through, when this call hit
	// sketchMaxSpan and so stopped short of the settled horizon.
	CaughtUpTo time.Time
	// Rebuilt is true when the stored cutoff did not match the product's
	// and the watermark was reset, so this call started the site's
	// sketches again from the beginning.
	Rebuilt bool
	// Skipped explains why nothing was done, when nothing was.
	Skipped string
	// Unavailable is true when this deployment has no sketch tables,
	// which means timescaledb_toolkit is not installed. Distinguished
	// from an ordinary skip because it is a state rather than a moment:
	// it will be true on every cycle until an operator installs the
	// extension, and it costs the customer the fast path.
	Unavailable bool
}

// Materialized reports whether this refresh wrote anything.
func (r SketchReport) Materialized() bool { return r.Skipped == "" && !r.Unavailable }

// Sketch keeps visitor_sketch in step with traffic_snapshots.
//
// Like Rollup, it runs as the collector's own role and needs no elevated
// wrapper: it reads a table this role already reads and writes a table
// derived from it.
type Sketch struct {
	pool *pgxpool.Pool
	// now is the clock, injectable so a test can place the settle margin
	// somewhere it can see.
	now func() time.Time
	// saidUnavailable makes the "no extension" line appear once per
	// process rather than once per cycle. An hourly line saying the same
	// permanent thing is a line nobody reads in the file that matters on
	// the day something goes wrong - but saying it *never*, the way an
	// ordinary skip is silent, would mean an operator has no way at all
	// to learn why long ranges are slow.
	saidUnavailable sync.Once
}

// NewSketch builds a refresher over an existing pool.
func NewSketch(pool *pgxpool.Pool) *Sketch {
	return &Sketch{pool: pool, now: time.Now}
}

// Refresh brings one site's sketches up to the settled horizon.
func (s *Sketch) Refresh(ctx context.Context, siteID string) (SketchReport, error) {
	out := SketchReport{SiteID: siteID}
	if siteID == "" {
		return out, fmt.Errorf("storage: sketch: empty site id")
	}

	// Does this deployment have the sketch at all?
	//
	// Asked as its own statement, and asked with to_regclass, which
	// takes the table's *name* as a string and so does not need it to
	// exist. Letting the query below fail with undefined_table would
	// have been one round trip fewer and a worse answer: PostgreSQL
	// resolves every relation in a statement while parsing, so that
	// error cannot tell "no timescaledb_toolkit" from "no
	// traffic_snapshots" - and reporting the wrong one of those to an
	// operator points them at the wrong half of their deployment.
	var installed bool
	if err := s.pool.QueryRow(ctx,
		`SELECT to_regclass('visitor_sketch') IS NOT NULL`).Scan(&installed); err != nil {
		return out, fmt.Errorf("storage: sketch installed: %w", err)
	}
	if !installed {
		// timescaledb_toolkit is not installed, so
		// internal/storage/schema.sql created neither table. Not an
		// error: the read path counts exactly from the raw table, which
		// is what every deployment did before this phase.
		out.Unavailable = true
		return out, nil
	}

	// The horizon: the start of the newest UTC day that is already
	// settled. In SQL rather than in Go, so the truncation is the same
	// time_bucket the refresh and the read path use - three callers, one
	// definition of where a day starts.
	var horizon time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT time_bucket($1::interval, $2::timestamptz)`,
		SketchBucket, s.now().Add(-sketchSettle),
	).Scan(&horizon); err != nil {
		return out, fmt.Errorf("storage: sketch horizon: %w", err)
	}

	// Where to start, and the cutoff the stored rows were built at. The
	// watermark pulled back by the recheck window; or, when the stored
	// cutoff is not the product's, from the site's oldest row, because
	// every row for this site now means something else.
	var (
		from  *time.Time
		start *time.Time
	)
	if err := s.pool.QueryRow(ctx, `
		SELECT (SELECT materialized_before - $2::interval FROM visitor_sketch_state
		         WHERE site_id = $1 AND bot_score_min = $4),
		       (SELECT time_bucket($3::interval, min(time)) FROM traffic_snapshots
		         WHERE site_id = $1)`,
		siteID, sketchRecheck, SketchBucket, scoring.BotCutoff,
	).Scan(&from, &start); err != nil {
		return out, fmt.Errorf("storage: sketch start: %w", err)
	}
	if start == nil {
		// No rows for this site. No watermark written either: one would
		// claim that days before the horizon are materialized, and for
		// a site whose rows arrive tomorrow the read path would believe
		// it.
		out.Skipped = "no rows for this site yet"
		return out, nil
	}
	if from == nil {
		// Either the first time this site is seen, or the cutoff moved.
		// Both mean the same work, so they are one branch - but they are
		// reported apart, because "catching up from scratch" and
		// "rebuilding because the product's definition changed" are
		// different sentences to find in a log.
		out.Rebuilt = s.storedCutoffDiffers(ctx, siteID)
		from = start
	}
	if !from.Before(horizon) {
		out.From, out.Through = *from, *from
		out.Skipped = "already materialized up to the settled horizon"
		return out, nil
	}
	if capped := from.Add(sketchMaxSpan); capped.Before(horizon) {
		horizon = capped
		out.CaughtUpTo = horizon
	}
	out.From, out.Through = *from, horizon

	// One statement per call, and the shape matters twice over.
	//
	// The inner grouping is by (day, address): an address counts as a
	// bot if *any* snapshot of it reached the cutoff, and a maximum over
	// a range is the maximum of the per-day maxima - which is exactly
	// what lets the daily bot sketches be unioned later without
	// changing what the word means.
	//
	// The FILTER is how the second sketch stays one pass. A day with no
	// bots yields NULL rather than an empty sketch, and rollup() skips a
	// NULL input, so the read path needs no branch for it.
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO visitor_sketch (site_id, day, ips, bot_ips)
		WITH per_ip_day AS (
		    SELECT time_bucket($4::interval, time) AS day,
		           ip,
		           max(bot_score) AS peak
		      FROM traffic_snapshots
		     WHERE site_id = $1 AND time >= $2 AND time < $3
		     GROUP BY 1, 2
		)
		SELECT $1, day,
		       hyperloglog($5::int, ip),
		       hyperloglog($5::int, ip) FILTER (WHERE peak >= $6)
		  FROM per_ip_day
		 GROUP BY day
		ON CONFLICT (site_id, day) DO UPDATE SET
		    ips     = excluded.ips,
		    bot_ips = excluded.bot_ips`,
		siteID, *from, horizon, SketchBucket, SketchPrecision, scoring.BotCutoff)
	if err != nil {
		return out, fmt.Errorf("storage: sketch upsert: %w", err)
	}
	out.Days = tag.RowsAffected()

	// The watermark moves only after the sketches are written, and the
	// cutoff moves with it. A watermark ahead of the rows it claims is
	// the one failure that makes the read path lie rather than merely be
	// slow.
	//
	// The guard is on the watermark alone and not on the cutoff: a
	// rebuild walks forward from the beginning, so its watermark is
	// *behind* the stored one and has to be allowed to replace it.
	// Without the cutoff in the comparison that would be a step
	// backwards; with it, it is the only way a changed cutoff ever
	// finishes.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO visitor_sketch_state (site_id, materialized_before, bot_score_min, refreshed_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (site_id) DO UPDATE SET
		    materialized_before = excluded.materialized_before,
		    bot_score_min       = excluded.bot_score_min,
		    refreshed_at        = excluded.refreshed_at
		 WHERE visitor_sketch_state.materialized_before < excluded.materialized_before
		    OR visitor_sketch_state.bot_score_min <> excluded.bot_score_min`,
		siteID, horizon, scoring.BotCutoff); err != nil {
		return out, fmt.Errorf("storage: sketch watermark: %w", err)
	}
	return out, nil
}

// storedCutoffDiffers reports whether a state row exists for this site at
// some other cutoff.
//
// Only ever asked to choose a sentence for the log, so a failed query is
// not an error: the refresh itself has already decided what to do, and
// turning a logging detail into a failure would stop a cycle that was
// about to succeed.
func (s *Sketch) storedCutoffDiffers(ctx context.Context, siteID string) bool {
	var differs bool
	if err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM visitor_sketch_state
		                WHERE site_id = $1 AND bot_score_min <> $2)`,
		siteID, scoring.BotCutoff).Scan(&differs); err != nil {
		return false
	}
	return differs
}

// Prune drops sketches older than the raw rows they summarize.
//
// The reasoning is Rollup.Prune's: a stored aggregate that outlives the
// rows behind it is a second, longer history, and the dashboard would
// draw visitors for days whose detail pages are empty. Same age, same
// number the caller passed to retention.
func (s *Sketch) Prune(ctx context.Context, siteID string, keepDays int) (int64, error) {
	if keepDays <= 0 {
		return 0, fmt.Errorf("storage: sketch prune: %d days is not a retention", keepDays)
	}
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM visitor_sketch
		 WHERE site_id = $1
		   AND day < time_bucket($3::interval, now() - make_interval(days => $2::int))`,
		siteID, keepDays, SketchBucket)
	switch {
	case IsMissingTable(err):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("storage: sketch prune: %w", err)
	}
	return tag.RowsAffected(), nil
}

// LogRefresh brings one site's sketches up to date, prunes what retention
// has dropped, and says what happened - once, at the level the outcome
// deserves.
//
// # What a failure here means, and why it is a warning
//
// The same as Rollup.LogRefresh: nothing, to the numbers. The read path
// counts exactly, from the raw table, for everything the sketch does not
// claim, so a refresh that never runs leaves a deployment exactly as
// accurate as it was and exactly as slow as it was. This runs inside the
// collector, which is on the traffic path, and stopping a collector to
// fix a dashboard's speed would be the wrong trade in every direction.
func (s *Sketch) LogRefresh(ctx context.Context, logger *slog.Logger, siteID string, keepDays int) {
	report, err := s.Refresh(ctx, siteID)
	switch {
	case err != nil:
		logger.Warn("visitor sketch: could not refresh, so long ranges in the panel "+
			"will stay as slow as they were", "site", siteID, "err", err)
		return
	case report.Unavailable:
		// Once per process. A permanent state deserves to be findable
		// and does not deserve an hourly line.
		s.saidUnavailable.Do(func() {
			logger.Info("visitor sketch: not installed on this database, so visitor "+
				"counts over long ranges are computed exactly and slowly. "+
				"Install the timescaledb_toolkit extension and run the schema "+
				"upgrade to turn it on; see KURULUM.md.", "site", siteID)
		})
		return
	case report.Skipped != "":
		// Not logged, for the reason the rollup gives: "already up to
		// date" is what every cycle after the first says.
	case report.Rebuilt:
		logger.Info("visitor sketch: the bot cutoff changed, so the sketches are "+
			"being rebuilt from the start of this site's history",
			"site", siteID, "cutoff", scoring.BotCutoff, "days", report.Days,
			"through", report.Through.UTC().Format(time.RFC3339))
	case !report.CaughtUpTo.IsZero():
		logger.Info("visitor sketch: still catching up, and further along than last cycle",
			"site", siteID, "days", report.Days,
			"through", report.Through.UTC().Format(time.RFC3339))
	default:
		logger.Info("visitor sketch: days materialized", "site", siteID,
			"days", report.Days,
			"through", report.Through.UTC().Format(time.RFC3339))
	}

	n, err := s.Prune(ctx, siteID, keepDays)
	if err != nil {
		logger.Warn("visitor sketch: could not prune, so it may now cover a longer "+
			"history than the rows it summarizes", "site", siteID,
			"keep_days", keepDays, "err", err)
		return
	}
	if n > 0 {
		logger.Info("visitor sketch: days pruned", "site", siteID, "days", n,
			"keep_days", keepDays)
	}
}
