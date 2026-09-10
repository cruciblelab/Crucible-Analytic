package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cruciblelab/crucible-analytic/internal/storage"
)

// The read half of O2: where the pre-aggregated answer stops and the raw
// table takes over.
//
// # The rule, and why it is one function
//
// A summary over [from, to) is answered from two places. That is a
// second source of truth for the same numbers, which is the failure
// C9.3 was about, so the arrangement has to make disagreement
// impossible rather than unlikely.
//
// Three properties do that, and all three are here rather than spread
// across the queries:
//
//   - The rollup answers only for buckets it has actually materialized.
//     The watermark is read from the database, not guessed from the
//     clock, so a refresh that is behind makes the answer slower and
//     never shorter.
//   - The rollup answers only for buckets that lie *entirely* inside
//     [from, to). A request whose bounds fall mid-bucket - which the API
//     permits, even though the panel's own bounds are local midnights -
//     leaves a ragged edge at each end, and those edges are the raw
//     table's.
//   - Every row is counted exactly once, because the three sub-ranges
//     are contiguous and disjoint by construction: [from, rollFrom),
//     [rollFrom, rollTo), [rollTo, to).
//
// What is deliberately *not* here: a fallback that answers from the
// rollup alone when the raw rows are gone, or from the raw table alone
// when the rollup looks complete. Either would be a second shape for the
// same question.

// rollupSpan is the part of [from, to) the rollup may answer for.
//
// ok is false when the rollup can answer nothing - no watermark yet, or
// no whole bucket inside the range - and then the raw table answers the
// whole range, which is what this endpoint did before O2.
// A zero watermark needs no branch of its own: it clamps end to the zero
// instant, and the comparison at the bottom then rejects the span for the
// same reason it rejects a range with no whole bucket in it. Written that
// way rather than guarded separately, because a guard the arithmetic
// already makes unreachable is a line no test can hold.
func rollupSpan(from, to, watermark time.Time) (rollFrom, rollTo time.Time, ok bool) {
	end := to
	if watermark.Before(end) {
		end = watermark
	}
	rollFrom = alignUp(from)
	rollTo = alignDown(end)
	if !rollFrom.Before(rollTo) {
		return time.Time{}, time.Time{}, false
	}
	return rollFrom, rollTo, true
}

// alignDown and alignUp move an instant onto the rollup's bucket grid.
//
// Go's Truncate and PostgreSQL's time_bucket have to agree on where that
// grid falls, and they are two implementations of it. That they do is
// held by a test against the real database rather than by this comment:
// see TestGoAndPostgresPutTheBucketGridInTheSamePlace.
func alignDown(t time.Time) time.Time { return t.Truncate(storage.RollupBucket) }

func alignUp(t time.Time) time.Time {
	if d := alignDown(t); d.Equal(t) {
		return t
	}
	return alignDown(t).Add(storage.RollupBucket)
}

// aggregable is the four numbers the rollup holds, over some range.
type aggregable struct {
	Snapshots       int
	PeakRequestRate float64
	AvgRequestRate  float64
	PeakWindow      int
}

// The three SQL fragments the answer is built from.
//
// # Why fragments and not one string with a flag in it
//
// The rollup contributes to the answer or it does not, and when it does
// not the statement must not *name* traffic_rollup at all: PostgreSQL
// resolves a relation name while parsing, before any WHERE has a chance
// to be false, so a `WHERE false AND ...` guard does not make a missing
// table survivable. That is not a hypothetical - it is what a real
// integration test reported the first time this was written that way,
// on a database that had not applied schema 20 yet.
//
// So there are two statements. What there is not is two definitions of
// the arithmetic: rawAggregate and rollupFinal each appear once and both
// statements are assembled from them, so a change to how a figure is
// computed cannot reach one path and miss the other.
const (
	// rawAggregate answers from traffic_snapshots, grouped per flush
	// because max_window is a maximum over flushes and the rollup stores
	// it that way. One level of grouping here and one max() in
	// rollupFinal give the same number a single pass gives.
	//
	// Two disjoint sub-ranges, which are the ragged ends the rollup
	// cannot claim: [$2, $3) and [$4, $5). When the rollup answers
	// nothing the first is the whole range and the second is empty.
	rawAggregate = `
		SELECT count(*)                                   AS snapshots,
		       sum(request_rate)                          AS sum_rate,
		       max(request_rate)                          AS max_rate,
		       sum(prev_window_count + curr_window_count) AS max_window
		  FROM traffic_snapshots
		 WHERE site_id = $1
		   AND ((time >= $2 AND time < $3) OR (time >= $4 AND time < $5))
		 GROUP BY time`

	// rolledAggregate answers from traffic_rollup, for whole buckets
	// inside the range: [$6, $7).
	rolledAggregate = `
		SELECT snapshots, sum_rate, max_rate, max_window
		  FROM traffic_rollup
		 WHERE site_id = $1 AND bucket >= $6 AND bucket < $7`

	// rollupFinal reduces the parts to the four figures. The average is
	// a sum over a count rather than an average of averages, which would
	// weight a quiet bucket the same as a busy one.
	rollupFinal = `
		SELECT COALESCE(sum(snapshots), 0),
		       COALESCE(max(max_rate), 0),
		       COALESCE(sum(sum_rate) / NULLIF(sum(snapshots), 0), 0),
		       COALESCE(max(max_window), 0)
		  FROM parts`
)

// aggregableOver answers the four aggregable figures for [from, to),
// reading whatever the rollup has materialized and the raw table for the
// rest.
//
// The two sources are read in one statement whenever both contribute.
// Not for atomicity - the sub-ranges are disjoint by timestamp, so a row
// landing mid-query is counted exactly once either way - but because a
// second round trip for a range the rollup already covers is the cost
// this whole phase exists to remove.
func (s *Store) aggregableOver(ctx context.Context, siteID string, from, to time.Time,
	watermark time.Time) (aggregable, error) {

	rollFrom, rollTo, ok := rollupSpan(from, to, watermark)
	headTo, tailFrom := to, to
	if ok {
		headTo, tailFrom = rollFrom, rollTo
	}

	args := []any{siteID, from, headTo, tailFrom, to}
	query := "WITH parts AS (" + rawAggregate + ")" + rollupFinal
	if ok {
		args = append(args, rollFrom, rollTo)
		query = "WITH parts AS (" + rawAggregate +
			" UNION ALL " + rolledAggregate + ")" + rollupFinal
	}

	var out aggregable
	err := s.pool.QueryRow(ctx, query, args...).
		Scan(&out.Snapshots, &out.PeakRequestRate, &out.AvgRequestRate, &out.PeakWindow)
	if err != nil {
		return aggregable{}, fmt.Errorf("api: aggregable over range: %w", err)
	}
	return out, nil
}

// rollupWatermark reads how far the rollup has been materialized for one
// site.
//
// A missing table is a missing rollup, not an error. The rollup arrived
// in schema 20, and a deployment that has not applied it yet - the
// window between installing a new binary and pressing the upgrade button
// - must keep answering from the raw table rather than failing. The
// panel's own Health page is what tells that customer to upgrade.
func (s *Store) rollupWatermark(ctx context.Context, siteID string) (time.Time, error) {
	var t *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT materialized_before FROM traffic_rollup_state WHERE site_id = $1`,
		siteID).Scan(&t)
	switch {
	case err == nil && t != nil:
		return *t, nil
	case err == nil:
		return time.Time{}, nil
	case errors.Is(err, pgx.ErrNoRows), isMissingTable(err):
		return time.Time{}, nil
	default:
		return time.Time{}, fmt.Errorf("api: rollup watermark: %w", err)
	}
}

// isMissingTable reports whether err is PostgreSQL's undefined_table.
//
// Matched on the structured code rather than on the message text: the
// message is localised and has been reworded between releases, and this
// decides whether a deployment gets its numbers or an error page.
func isMissingTable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42P01"
}
