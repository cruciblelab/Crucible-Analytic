// Package rangerefresh is the request queue between the panel, which may
// not open outbound connections, and whichever service fetches the IP
// range datasets.
//
// # The shape, and why it is this shape
//
// The panel is the process a customer's browser reaches. A button that
// made it download a file would put an outbound fetch in exactly the
// process that must not have one - PLAN.md's permanent list forbids "any
// operation whose parameter is a hostname the deployment will connect
// to", and a hostname compiled in rather than typed in is the same
// operation with the parameter hidden.
//
// So the button writes a row. The collector (or the beacon, whichever
// has asn_lookup enabled) polls, claims it, runs the refresh it was
// already built to run, and writes back what happened.
//
// internal/upgrade is the same design for schema migrations, and this
// package deliberately mirrors it down to the function names: two queues
// that behave differently for no reason are two things to learn.
//
// # The one real difference from internal/upgrade
//
// An upgrader is installed with the package. A resolver only exists when
// a deployment has turned asn_lookup on - and it is off by default. So
// "nobody claimed it" is not a fault here, it is the ordinary outcome of
// pressing the button on a deployment that is not fetching anything, and
// the in-flight index would otherwise turn the first press into a
// permanent jam.
//
// ExpireStale is the answer, and the panel calls it before every ask.
package rangerefresh

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PollInterval is how often a fetcher looks for a button press.
//
// Thirty seconds, matching the upgrader's systemd timer, and for the
// same reason: it is the longest a person will wait in front of a page
// without deciding the button is broken.
//
// Here rather than in internal/asnlookup because both sides need it and
// only one of them may be imported by the panel. The panel must not
// import traffic-path code - M1 made a whole package to avoid it - and a
// poll interval is a property of this queue's contract rather than of
// whoever happens to be polling.
const PollInterval = 30 * time.Second

// UnclaimedAfter is how long a pending request may wait before the panel
// removes it.
//
// Four poll intervals, derived rather than chosen: the only thing that
// should ever reach it is a deployment where nothing is polling at all -
// asn_lookup off, or both services stopped - and a request that survives
// four polls was not going to be claimed.
//
// Written as a multiple so the two cannot drift into an expiry shorter
// than a poll, which would delete requests a fetcher was about to take
// and leave the customer watching nothing happen for a reason no page
// could explain.
const UnclaimedAfter = 4 * PollInterval

// State is where a request has got to.
type State string

const (
	// StatePending is written by the panel and read by the fetcher.
	StatePending State = "pending"
	// StateRunning means a fetcher has claimed it.
	StateRunning State = "running"
	// StateSucceeded means the refresh ran. It does not mean every file
	// arrived: a refresh in which the IPv6 file failed and the IPv4 one
	// worked is a refresh that happened, and FilesFailed is where that
	// shows. Collapsing "some files failed" into a failed request would
	// hide the half that worked.
	StateSucceeded State = "succeeded"
	// StateFailed means the refresh did not run at all - the fetcher
	// could not do the work, rather than the datasets being unreachable.
	StateFailed State = "failed"
)

// Actor is who pressed the button, in the shape panel_audit_log records
// one.
type Actor struct {
	Kind  string
	ID    *int64
	Label string
}

// Request is one row.
type Request struct {
	ID          int64
	RequestedAt time.Time
	Actor       Actor
	OperationID string

	State      State
	ClaimedAt  *time.Time
	ClaimedBy  string
	FinishedAt *time.Time

	FilesOK     int
	FilesFailed int
	ErrorChain  string
}

// InFlight reports whether this request is still going.
func (r *Request) InFlight() bool {
	return r != nil && (r.State == StatePending || r.State == StateRunning)
}

// Unclaimed reports whether this request has been waiting without any
// fetcher taking it.
//
// The state the panel has to be able to name: on a deployment with
// asn_lookup off, nothing is polling, and a row that sits at pending for
// ever needs a sentence rather than a spinner.
func (r *Request) Unclaimed(now time.Time, after time.Duration) bool {
	return r != nil && r.State == StatePending && now.Sub(r.RequestedAt) > after
}

// ErrAlreadyInFlight is returned when a request is already waiting or
// running.
//
// Its own error because the panel says something different for it: not
// "that failed" but "one is already going, here it is". Two customers
// pressing within a second of each other is the ordinary way to get
// here, and it is not a fault. It is also the "pressing twice does not
// start two fetches" half of M3's done criterion.
var ErrAlreadyInFlight = errors.New("rangerefresh: a request is already in flight")

// ErrNothingToDo is returned by Claim when no request is waiting. Not an
// error condition - it is what a fetcher sees on almost every poll.
var ErrNothingToDo = errors.New("rangerefresh: no request is waiting")

const columns = `id, requested_at, actor_kind, actor_id, actor_label, operation_id,
	state, claimed_at, claimed_by, finished_at, files_ok, files_failed, error_chain`

func scan(row pgx.Row) (*Request, error) {
	var r Request
	var state string
	err := row.Scan(&r.ID, &r.RequestedAt, &r.Actor.Kind, &r.Actor.ID, &r.Actor.Label,
		&r.OperationID, &state, &r.ClaimedAt, &r.ClaimedBy, &r.FinishedAt,
		&r.FilesOK, &r.FilesFailed, &r.ErrorChain)
	if err != nil {
		return nil, err
	}
	r.State = State(state)
	return &r, nil
}

// Ask writes a request.
//
// Returns ErrAlreadyInFlight when one is already waiting or running,
// which the in-flight index decides rather than this function: two
// callers checking first and inserting second would both find nothing
// and both insert.
func Ask(ctx context.Context, pool *pgxpool.Pool, a Actor, operationID string) (*Request, error) {
	r, err := scan(pool.QueryRow(ctx, `
		INSERT INTO ip_range_refresh_requests
		  (actor_kind, actor_id, actor_label, operation_id)
		VALUES ($1, $2, $3, $4)
		RETURNING `+columns, a.Kind, a.ID, a.Label, operationID))
	if isUniqueViolation(err) {
		return nil, ErrAlreadyInFlight
	}
	if err != nil {
		return nil, fmt.Errorf("rangerefresh: ask: %w", err)
	}
	return r, nil
}

// Latest is the most recent request, or nil when there has never been
// one.
func Latest(ctx context.Context, pool *pgxpool.Pool) (*Request, error) {
	r, err := scan(pool.QueryRow(ctx,
		`SELECT `+columns+` FROM ip_range_refresh_requests ORDER BY id DESC LIMIT 1`))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("rangerefresh: latest: %w", err)
	}
	return r, nil
}

// Claim takes the waiting request, if there is one.
//
// An UPDATE guarded by the state, so two fetchers polling at the same
// moment cannot both take it - which is not hypothetical: a deployment
// with asn_lookup on in both services really does run two resolvers
// against one database.
func Claim(ctx context.Context, pool *pgxpool.Pool, by string) (*Request, error) {
	r, err := scan(pool.QueryRow(ctx, `
		UPDATE ip_range_refresh_requests
		SET state = 'running', claimed_at = now(), claimed_by = $1
		WHERE id = (SELECT id FROM ip_range_refresh_requests
		            WHERE state = 'pending' ORDER BY id LIMIT 1)
		  AND state = 'pending'
		RETURNING `+columns, by))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrNothingToDo
	case err != nil:
		return nil, fmt.Errorf("rangerefresh: claim: %w", err)
	}
	return r, nil
}

// Finish records the outcome.
//
// filesOK and filesFailed are the summary the row carries so it says
// whether the refresh worked without the reader correlating timestamps
// against ip_range_fetches, where the detail is.
func Finish(ctx context.Context, pool *pgxpool.Pool, id int64, state State,
	filesOK, filesFailed int, errChain string) error {

	tag, err := pool.Exec(ctx, `
		UPDATE ip_range_refresh_requests
		SET state = $2, finished_at = now(),
		    files_ok = $3, files_failed = $4, error_chain = $5
		WHERE id = $1`, id, string(state), filesOK, filesFailed, errChain)
	if err != nil {
		return fmt.Errorf("rangerefresh: finish %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		// Not a lost update to shrug at: the row this fetcher claimed is
		// gone or changed underneath it, so a refresh that really ran is
		// now unrecorded and the in-flight slot may be free for a second
		// one.
		return fmt.Errorf("rangerefresh: finish %d: the request is no longer there", id)
	}
	return nil
}

// ExpireStale removes requests that were never claimed, and returns how
// many went.
//
// # Why deleting rather than marking failed
//
// internal/upgrade marks a stranded claim failed, because there the
// question "did the migration run" has to stay answerable - nobody knows
// whether it finished, and saying so is the honest record.
//
// Here the question does not arise. A pending row was never claimed, so
// nothing ran, so there is nothing to record: the row is a button press
// that reached a service which is not running. Keeping it would leave
// the history full of rows that mean "asn_lookup was off", which is not
// a fact about a refresh.
//
// Pending only. A row at running belongs to a fetcher that may still be
// working - a refresh downloads a hundred megabytes - and deleting it
// would free the in-flight slot while the work continues.
func ExpireStale(ctx context.Context, pool *pgxpool.Pool, olderThan time.Duration) (int64, error) {
	tag, err := pool.Exec(ctx, `
		DELETE FROM ip_range_refresh_requests
		WHERE state = 'pending' AND requested_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int64(olderThan/time.Second)))
	if err != nil {
		return 0, fmt.Errorf("rangerefresh: expire stale: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ReleaseStaleClaims puts back requests a fetcher took and never
// finished.
//
// A process killed mid-refresh leaves a row at running for ever, and the
// in-flight index then refuses every later request - a button
// permanently greyed out because of a crash weeks ago. Marked failed
// rather than pending, which is honest: nobody knows how far it got, and
// the fetch log says which files arrived.
//
// The timeout has to be longer than a real refresh, not longer than a
// fast one. Ten minutes: the whole set of files is about 124 MB measured
// against the real source, which is seconds on a good link and minutes
// on a bad one - and none of that should look like a crash.
func ReleaseStaleClaims(ctx context.Context, pool *pgxpool.Pool, olderThan time.Duration) (int64, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE ip_range_refresh_requests
		SET state = 'failed', finished_at = now(),
		    error_chain = 'the fetcher that claimed this never reported back; ' ||
		                  'whether the refresh finished is not known'
		WHERE state = 'running' AND claimed_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int64(olderThan/time.Second)))
	if err != nil {
		return 0, fmt.Errorf("rangerefresh: release stale claims: %w", err)
	}
	return tag.RowsAffected(), nil
}

// isUniqueViolation reports whether err is PostgreSQL's 23505.
//
// By code rather than by message text: the message is localised by the
// server's lc_messages, so a deployment whose PostgreSQL speaks Turkish
// would match nothing and a second press would surface as an unexplained
// database error instead of "one is already going".
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}
