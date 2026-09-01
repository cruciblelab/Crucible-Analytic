package asnlookup

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/rangerefresh"
)

// The other half of M3's button: the panel writes a row, this reads it.
//
// # Why a second ticker
//
// Run already refreshes on asn_lookup.refresh_interval_seconds, which
// defaults to a week. A button whose effect might not arrive until next
// Tuesday is not a button, so the request queue is polled on its own,
// much shorter interval.
//
// The two are deliberately separate rather than one fast ticker doing
// both. A refresh downloads about 124 MB from a third party; running it
// every thirty seconds to notice a button press would be a denial of
// service aimed at the people who publish the data for free. Polling a
// table with an index on it costs nothing worth measuring.
//
// # What "answering" means here
//
// The fetcher does not do anything new. It runs the refresh it was
// already built to run, which writes its own per-file rows into
// ip_range_fetches (M2) - so the detail of what happened is recorded by
// the same code whether the refresh was scheduled or asked for.
//
// This file only decides *when*, and writes the one-line summary back
// onto the request so the panel can say whether it worked without
// correlating timestamps.

// requestPollInterval is rangerefresh.PollInterval, named locally so the
// ticker below reads like the other one.
//
// The value lives in the queue package because the panel needs it too -
// to know how long an unclaimed request has to wait before it is written
// off - and the panel may not import this package. See M1 on why.
const requestPollInterval = rangerefresh.PollInterval

// staleClaimAfter is when a claimed request is assumed abandoned.
//
// Ten minutes against a refresh measured at 4.7 seconds for all ten
// files on a good link. The gap is for a slow link and a busy machine,
// neither of which should look like a crash - the same reasoning
// internal/upgrade's fifteen minutes uses, with a smaller number because
// this work is smaller.
const staleClaimAfter = 10 * time.Minute

// answerRequests takes one waiting request, if there is one, and runs
// the refresh it asks for.
//
// Everything here is best-effort in the same sense recordFetch is: a
// queue that cannot be read must not stop the scheduled refresh, because
// the scheduled refresh is the product and the button is a convenience.
func (r *Resolver) answerRequests(ctx context.Context) {
	if r.pool == nil {
		return
	}

	// Stale claims first, so a request stranded by a crash does not hold
	// the one-in-flight slot for ever. Same ordering as the upgrader,
	// and for the same reason.
	if released, err := rangerefresh.ReleaseStaleClaims(ctx, r.pool, staleClaimAfter); err != nil {
		r.logger().Warn("asnlookup: could not release stale refresh claims", "err", err)
	} else if released > 0 {
		r.logger().Warn("asnlookup: released a refresh claim whose fetcher never reported back",
			"count", released)
	}

	req, err := rangerefresh.Claim(ctx, r.pool, fetcherName())
	if errors.Is(err, rangerefresh.ErrNothingToDo) {
		return
	}
	if err != nil {
		r.logger().Warn("asnlookup: could not read the refresh queue", "err", err)
		return
	}

	r.logger().Info("asnlookup: refreshing because somebody asked",
		"request", req.ID, "by", req.Actor.Label)

	// The counts come from the fetch log rather than from the refresh,
	// and the reason is that the refresh already writes them. Reading
	// them back is what makes the summary and the detail the same facts:
	// a summary computed separately is a second answer to one question.
	began := time.Now()
	r.refresh(ctx)
	ok, failed := r.fetchOutcomesSince(ctx, began)

	state := rangerefresh.StateSucceeded
	chain := ""
	if ok == 0 {
		// Not one file arrived. That is a refresh that did not happen, as
		// opposed to one where a family failed - and the two have to read
		// differently or "it worked" stops meaning anything.
		state = rangerefresh.StateFailed
		chain = "no dataset file could be fetched; see the fetch log for each one's reason"
	}
	if err := rangerefresh.Finish(ctx, r.pool, req.ID, state, ok, failed, chain); err != nil {
		r.logger().Warn("asnlookup: the refresh ran and could not be recorded",
			"request", req.ID, "err", err)
	}
}

// fetchOutcomesSince counts the fetch-log rows this refresh wrote.
//
// Keyed on time rather than on a request id, because ip_range_fetches
// deliberately does not know about requests: a fetch row means the same
// thing whether it came from the timer or the button, and threading a
// request id through the refresh path would make the two different.
func (r *Resolver) fetchOutcomesSince(ctx context.Context, since time.Time) (ok, failed int) {
	err := r.pool.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE outcome = 'succeeded'),
		  count(*) FILTER (WHERE outcome <> 'succeeded')
		FROM ip_range_fetches
		WHERE started_at >= $1`, since).Scan(&ok, &failed)
	if err != nil {
		// Zero and zero, which Finish reads as "nothing arrived" and
		// records as a failure. Wrong in the safe direction: a refresh
		// reported as failed sends somebody to look at the fetch log,
		// where the truth is; one reported as succeeded sends nobody
		// anywhere.
		r.logger().Warn("asnlookup: could not summarise the refresh", "err", err)
		return 0, 0
	}
	return ok, failed
}

// fetcherName identifies this process in claimed_by.
//
// The hostname, so a two-host deployment's rows say which one took it -
// the same choice cmd/upgrader makes. A failure to read it is not worth
// failing over: the field is for a person reading a row, and "unknown"
// is a worse answer than a hostname but a better one than no refresh.
func fetcherName() string {
	if name, err := os.Hostname(); err == nil && name != "" {
		return name
	}
	return "unknown"
}
