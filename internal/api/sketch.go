package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/cruciblelab/crucible-analytic/internal/storage"
)

// The read half of O3: how many different visitors, and which of the two
// answers to that question a given response carries.
//
// # There are two answers, and the response says which
//
// A distinct count is exact and gets slower with the range. A merged
// HyperLogLog is approximate and stays fast. This product keeps both,
// and the rule that stops them from becoming two truths is not that one
// of them wins - it is that every response names the one it used, and
// the panel prints it.
//
// The alternative was considered and rejected: always estimating. It
// would have made a number that is exact today - a 24-hour range on any
// deployment, a 90-day range on a small one - approximate for no gain.
// "As little error as possible" was the owner's criterion, and an
// estimate nobody needed is error nobody needed.

// VisitorCountMethod says how a response's visitor figures were
// computed. A caller that shows the numbers has to be able to say which
// of these it is showing.
type VisitorCountMethod string

const (
	// VisitorCountExact means distinct addresses were counted, one by
	// one, over every row in the range.
	VisitorCountExact VisitorCountMethod = "exact"
	// VisitorCountEstimated means whole UTC days came from stored
	// sketches, the ragged ends came from the raw table, and the answer
	// is the estimate of their union.
	VisitorCountEstimated VisitorCountMethod = "estimated"
)

// VisitorSketchRelativeError is the standard relative error a caller
// should attach to an estimated figure, as a fraction.
//
// 1,04/sqrt(65536), which is HyperLogLog's documented standard error at
// this register count - and it is reported as the *dense-regime* figure
// deliberately. A small site's sketches stay sparse and are very nearly
// exact; that accuracy disappears as the site grows. A margin that only
// holds while a customer is small is not a margin, so the one the
// product quotes is the one it can keep.
//
// Measured against twenty genuinely different draws per precision: 0,36 %
// at this register count in the dense regime, against the 0,41 % below.
// The constant is the theory rather than the measurement, because the
// theory is the bound the measurement stayed inside - quoting the
// measurement would be quoting the luckier of the two.
const VisitorSketchRelativeError = 0.0041

// exactVisitorRowBudget is the largest range, in rows, that is counted
// exactly.
//
// # Why there is a threshold rather than always estimating
//
// Because the exact count is better when it is affordable, and it very
// often is. The cost is linear in rows and it was measured cold
// (PostgreSQL stopped, caches dropped, restarted; three repeats, median)
// on one site's 11,0 million rows over 91 days:
//
//	 123 k rows    0,33 s
//	 863 k rows    0,78 s
//	3,70 M rows    2,25 s
//	11,0 M rows    6,14 s
//
// About 0,53 s per million rows. The panel gives one call 5 s, so four
// million rows - some 2,1 s here - spends two fifths of that budget and
// leaves the rest for a slower machine than this one. Beyond it the
// estimate answers in 0,50 s whatever the range.
//
// # What this number is not
//
// It is not a promise about a customer's hardware. It is a proxy,
// calibrated here, for a cost this code cannot measure before paying it.
// Rows are the term that dominates, and the row count is already known
// by then - the rollup counts them - so the decision costs nothing to
// take.
//
// What a wrong guess costs in each direction is bounded and unequal: too
// high and one long request is slow, which is what today's product does
// on every long request; too low and a number that could have been exact
// is approximate, and says so. Neither is silent.
const exactVisitorRowBudget = 4_000_000

// rowBudget is this store's exact-count budget, with zero reading as the
// measured constant.
//
// So that a Store assembled by hand - which the tests in this package do
// - takes the same branch the binary's would. A test seam whose unset
// value changes behaviour is a seam that silently rewrites every test
// that did not know about it.
func (s *Store) rowBudget() int {
	if s.exactRows == 0 {
		return exactVisitorRowBudget
	}
	return s.exactRows
}

// visitors is the three figures and how they were produced.
type visitors struct {
	Unique, Bot, Human int
	Method             VisitorCountMethod
}

// sketchSpan is the part of [from, to) the sketches may answer for: the
// whole UTC days that lie entirely inside it and entirely behind the
// watermark.
//
// ok is false when that is nothing - no watermark, or a range too short
// to contain a whole day, which is every single local day in every zone
// but UTC. The caller then counts exactly, which is also what it does
// when the range is small enough to afford it.
func sketchSpan(from, to, watermark time.Time) (dayFrom, dayTo time.Time, ok bool) {
	end := to
	if watermark.Before(end) {
		end = watermark
	}
	dayFrom = dayUp(from)
	dayTo = dayDown(end)
	if !dayFrom.Before(dayTo) {
		return time.Time{}, time.Time{}, false
	}
	return dayFrom, dayTo, true
}

// dayDown and dayUp move an instant onto the UTC day grid.
//
// Truncate rounds down to a multiple of the duration measured from the
// zero instant, which is a UTC midnight - so multiples of 24 hours land
// on UTC midnights and nowhere else. That is exactly the grid
// time_bucket('1 day', ...) uses, and it is why this arithmetic is
// correct here and would be wrong for a customer's own day: a UTC day is
// always 24 hours and a local one is not, which internal/panel/ui had to
// learn the hard way in O2a.
//
// The .UTC() is for the value's own sake rather than for the arithmetic
// - Truncate ignores the location - so that a bound printed in a log or
// a failure message reads in the zone the sketches are actually keyed
// by.
func dayDown(t time.Time) time.Time { return t.UTC().Truncate(24 * time.Hour) }

func dayUp(t time.Time) time.Time {
	if d := dayDown(t); d.Equal(t) {
		return t
	}
	return dayDown(t).Add(24 * time.Hour)
}

// The two statements the visitor figures come from.
const (
	// exactVisitors is the definition: one row per address, its peak
	// score over the whole range, counted.
	//
	// An address counts as a bot if *any* snapshot of it in range scored
	// at or above the threshold - a burst that later decays should not
	// erase the fact that it happened.
	exactVisitors = `
		WITH per_ip AS (
		    SELECT ip, max(bot_score) AS peak_score
		      FROM traffic_snapshots
		     WHERE site_id = $1 AND time >= $2 AND time < $3
		     GROUP BY ip
		)
		SELECT (SELECT count(*) FROM per_ip),
		       (SELECT count(*) FROM per_ip WHERE peak_score >= $4)`

	// estimatedVisitors merges the stored days with the ragged ends.
	//
	// # Why the edges are grouped by address and not by day
	//
	// Because the union does not care where the parts were cut. An
	// address is in the range's bot set if it reached the cutoff
	// anywhere in the range, and a maximum over the range is the
	// maximum of the parts' maxima - so one group per address over both
	// edges together is not an approximation of the per-day shape, it
	// is the same set.
	//
	// # Why an empty edge needs no branch
	//
	// hyperloglog() over no rows is NULL, rollup() skips a NULL input,
	// and distinct_count() of NULL is NULL - which the caller reads as
	// zero. So a range with no ragged end, a day with no bots, and a
	// site with no rows at all take the same path as everything else.
	estimatedVisitors = `
		WITH edge_ip AS (
		    SELECT ip, max(bot_score) AS peak
		      FROM traffic_snapshots
		     WHERE site_id = $1
		       AND ((time >= $2 AND time < $3) OR (time >= $4 AND time < $5))
		     GROUP BY ip
		), parts AS (
		    SELECT ips, bot_ips
		      FROM visitor_sketch
		     WHERE site_id = $1 AND day >= $6 AND day < $7
		    UNION ALL
		    SELECT hyperloglog($8::int, ip),
		           hyperloglog($8::int, ip) FILTER (WHERE peak >= $9)
		      FROM edge_ip
		)
		SELECT distinct_count(rollup(ips)), distinct_count(rollup(bot_ips))
		  FROM parts`
)

// visitorsOver answers the three visitor figures for [from, to).
//
// rows is how many raw rows the range holds, which the caller already
// knows from the rollup: it decides whether the exact count is
// affordable. watermark and cutoff come from the sketch's state row.
func (s *Store) visitorsOver(ctx context.Context, siteID string, from, to time.Time,
	botScoreMin, rows int, state sketchState) (visitors, error) {

	dayFrom, dayTo, ok := sketchSpan(from, to, state.Watermark)
	switch {
	case rows <= s.rowBudget():
		// Affordable, so exact. First, because this is the branch that
		// keeps the common ranges exact and it must not be reachable
		// only when the sketch happens to be missing.
		ok = false
	case !state.Usable(botScoreMin):
		// Either this deployment has no sketches, or the ones it has
		// were built at another cutoff than the one being asked for.
		// One condition, two situations, and the same answer: count.
		ok = false
	}

	if !ok {
		var out visitors
		out.Method = VisitorCountExact
		if err := s.pool.QueryRow(ctx, exactVisitors,
			siteID, from, to, botScoreMin).Scan(&out.Unique, &out.Bot); err != nil {
			return visitors{}, fmt.Errorf("api: visitor counts: %w", err)
		}
		out.Human = out.Unique - out.Bot
		return out, nil
	}

	// The three parts are contiguous and disjoint by construction -
	// [from, dayFrom), [dayFrom, dayTo), [dayTo, to) - so no address is
	// offered to the union twice. Not that it would matter to the
	// arithmetic: a union counts an address once however many parts it
	// appears in, which is exactly what a distinct count cannot do by
	// addition and why this table exists.
	var unique, bot *int
	if err := s.pool.QueryRow(ctx, estimatedVisitors,
		siteID, from, dayFrom, dayTo, to, dayFrom, dayTo,
		storage.SketchPrecision, botScoreMin,
	).Scan(&unique, &bot); err != nil {
		return visitors{}, fmt.Errorf("api: visitor estimate: %w", err)
	}

	out := visitors{Method: VisitorCountEstimated}
	if unique != nil {
		out.Unique = *unique
	}
	if bot != nil {
		out.Bot = *bot
	}
	out.Bot, out.Human = splitVisitors(out.Unique, out.Bot)
	return out, nil
}

// splitVisitors makes two independent estimates into three consistent
// figures.
//
// # The problem it solves
//
// The bot set is contained in the unique set by definition, and nothing
// in the arithmetic knows that: they are two sketches, estimated apart.
// When almost every address in a range reached the cutoff, the bot
// estimate can land above the unique one - both inside their margins -
// and the human figure computed from them would be *negative*. A
// customer cannot interpret that, and it would be the page's own numbers
// contradicting each other rather than merely being approximate.
//
// So containment is imposed: the figure that is smaller by definition is
// clamped to the one that contains it, and the third is a subtraction.
// Bot plus human is then unique, always, and the inconsistency - at most
// one margin - lands in the half where no reader can see it.
//
// # Why it is a function
//
// Because a branch that needs an unlucky pair of estimates to be
// reached is a branch no fixture can be relied on to reach. Written
// here, its own test hands it the pair directly. A condition whose
// triggering state never appears in a table is a condition nobody has
// measured.
func splitVisitors(unique, bot int) (int, int) {
	if bot > unique {
		bot = unique
	}
	return bot, unique - bot
}

// sketchState is one site's sketch watermark and the cutoff its rows
// were built at.
//
// The zero value is what three different situations produce - no sketch
// tables on this deployment, no rows for this site yet, no refresh
// completed - and deliberately so: all three mean "nothing to merge",
// and a read path that told them apart would be a read path with three
// behaviours where the product has one.
type sketchState struct {
	Watermark time.Time
	// Cutoff is the bot threshold the stored sketches mean. Zero when
	// there is no state row at all, which no request can match to a
	// usable sketch, because a zero watermark already refuses it.
	Cutoff int
}

// Usable reports whether the stored sketches answer the question being
// asked.
//
// Two comparisons, and the second covers two situations that would
// otherwise each need a rule: a caller asking for a threshold other than
// the product's default, and a product whose default has moved since
// these rows were written. Both mean the stored bot set is not the one
// the response would be labelled with.
//
// The unique figure would survive either - it does not depend on a
// threshold - but it is refused with the rest, because a response whose
// three numbers came from two different definitions is a response that
// cannot say how it was computed.
func (s sketchState) Usable(botScoreMin int) bool {
	return !s.Watermark.IsZero() && s.Cutoff == botScoreMin
}

// sketchStateOf reads one site's sketch state.
//
// A missing table is a missing sketch, not an error - the same contract
// rollupWatermark has, and for the same reason: the tables arrive with
// schema 24 and only where timescaledb_toolkit is installed, so a
// deployment without either must keep answering rather than fail.
func (s *Store) sketchStateOf(ctx context.Context, siteID string) (sketchState, error) {
	var out sketchState
	var (
		before time.Time
		cutoff int
	)
	err := s.pool.QueryRow(ctx, `
		SELECT materialized_before, bot_score_min
		  FROM visitor_sketch_state WHERE site_id = $1`, siteID).Scan(&before, &cutoff)
	switch {
	case err == nil:
		return sketchState{Watermark: before, Cutoff: cutoff}, nil
	case errors.Is(err, pgx.ErrNoRows):
		// The tables exist; this site has no sketches yet, which is the
		// state every site is in for the first hour of its life.
		return out, nil
	case storage.IsMissingTable(err):
		// No tables at all: timescaledb_toolkit is not installed here.
		// The same answer as the line above, and that is the point -
		// see the type's comment.
		return out, nil
	default:
		return out, fmt.Errorf("api: sketch state: %w", err)
	}
}
