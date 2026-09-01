// Package upgrade is the request queue between the panel, which may not
// run DDL, and the applier, which may.
//
// # The shape, and why it is this shape
//
// The panel holds no DDL privilege at all - see release/sql/grants.sql,
// which contains no ALTER, no CREATE and no OWNER anywhere - and that is
// a property B6 and H5 established deliberately. An upgrade button that
// migrated the database directly would have to undo it.
//
// So the button writes a row saying what it wants, and a separate
// component with the authority reads the row, applies the schema and
// writes back what happened. DDL stays unreachable from the panel
// process on every code path, including ones added later by somebody who
// has not read this.
//
// # Ask and answer are different privileges
//
// panel_user holds INSERT and SELECT here; schema_admin holds SELECT and
// UPDATE. Neither holds both. A compromised panel can therefore ask for
// an upgrade - which is a button any signed-in customer can press anyway
// - and cannot fabricate the result of one.
package upgrade

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// State is where a request has got to.
type State string

const (
	// StatePending is written by the panel and read by the applier.
	StatePending State = "pending"
	// StateRunning means an applier has claimed it. Claiming is an
	// UPDATE guarded by the previous state, so the database picks the
	// winner rather than two processes agreeing between themselves.
	StateRunning State = "running"
	// StateSucceeded means the schema was applied and the version row
	// updated.
	StateSucceeded State = "succeeded"
	// StateFailed means it was not, and error_chain says why.
	//
	// A failed request is finished, not stuck: the in-flight index only
	// counts pending and running, so a customer can press the button
	// again after reading what went wrong. That is the "tekrar
	// denenebiliyor" half of the done criterion.
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

	FromVersion   int
	ToVersion     int
	ToFingerprint string

	State      State
	ClaimedAt  *time.Time
	ClaimedBy  string
	FinishedAt *time.Time
	ErrorChain string

	AppliedVersion *int
}

// InFlight reports whether this request is still going.
func (r *Request) InFlight() bool {
	return r != nil && (r.State == StatePending || r.State == StateRunning)
}

// ErrAlreadyInFlight is returned when a request is already waiting or
// running.
//
// Its own error because the panel says something different for it: not
// "that failed" but "one is already going, here it is". Two customers
// pressing the button within a second of each other is the ordinary way
// to get here, and it is not a fault.
var ErrAlreadyInFlight = errors.New("upgrade: a request is already in flight")

// ErrNothingToDo is returned by Claim when no request is waiting. Not an
// error condition - it is what the applier sees on almost every run.
var ErrNothingToDo = errors.New("upgrade: no request is waiting")

const columns = `id, requested_at, actor_kind, actor_id, actor_label, operation_id,
	from_version, to_version, to_fingerprint, state,
	claimed_at, claimed_by, finished_at, error_chain, applied_version`

func scan(row pgx.Row) (*Request, error) {
	var r Request
	var state string
	err := row.Scan(&r.ID, &r.RequestedAt, &r.Actor.Kind, &r.Actor.ID, &r.Actor.Label,
		&r.OperationID, &r.FromVersion, &r.ToVersion, &r.ToFingerprint, &state,
		&r.ClaimedAt, &r.ClaimedBy, &r.FinishedAt, &r.ErrorChain, &r.AppliedVersion)
	if err != nil {
		return nil, err
	}
	r.State = State(state)
	return &r, nil
}

// Ask records that somebody wants the schema brought up to date.
//
// The panel's half. It records what was believed at the moment of
// asking - the version the database reported, and the version and
// fingerprint this binary expects - rather than leaving them to be
// looked up later, because the answer to "what did we think we were
// doing" must not change when somebody deploys a different binary an
// hour afterwards.
func Ask(ctx context.Context, pool *pgxpool.Pool, a Actor, operationID string,
	fromVersion, toVersion int, toFingerprint string) (*Request, error) {

	row := pool.QueryRow(ctx, `
		INSERT INTO panel_upgrade_requests
		  (actor_kind, actor_id, actor_label, operation_id,
		   from_version, to_version, to_fingerprint)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING `+columns,
		a.Kind, a.ID, a.Label, operationID, fromVersion, toVersion, toFingerprint)

	r, err := scan(row)
	if err != nil {
		// 23505 is unique_violation, which here can only be the
		// one-in-flight index. Matched on the code rather than the
		// message, which is localised and has changed between releases.
		if isUniqueViolation(err) {
			return nil, ErrAlreadyInFlight
		}
		return nil, fmt.Errorf("upgrade: ask: %w", err)
	}
	return r, nil
}

// Latest is the most recent request, or nil when there has never been
// one.
//
// nil rather than an error for "never asked": it is the state every
// deployment starts in, and a page that shows it should say "no upgrade
// has been requested", not "could not read the upgrade log".
func Latest(ctx context.Context, pool *pgxpool.Pool) (*Request, error) {
	r, err := scan(pool.QueryRow(ctx,
		`SELECT `+columns+` FROM panel_upgrade_requests ORDER BY id DESC LIMIT 1`))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("upgrade: latest: %w", err)
	}
	return r, nil
}

// Claim takes the waiting request, if there is one.
//
// The applier's half, and the guard against two of them running at once.
// The state is part of the UPDATE's WHERE rather than checked first and
// updated after: between a SELECT and an UPDATE another applier can do
// both, and the window is exactly as long as the round trip.
//
// `FOR UPDATE SKIP LOCKED` is not used, deliberately - there is only
// ever one row to take, so a second claimant should be told it lost
// rather than handed the next row along.
func Claim(ctx context.Context, pool *pgxpool.Pool, by string) (*Request, error) {
	r, err := scan(pool.QueryRow(ctx, `
		UPDATE panel_upgrade_requests
		SET state = 'running', claimed_at = now(), claimed_by = $1
		WHERE id = (SELECT id FROM panel_upgrade_requests
		            WHERE state = 'pending' ORDER BY id LIMIT 1)
		  AND state = 'pending'
		RETURNING `+columns, by))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrNothingToDo
	case err != nil:
		return nil, fmt.Errorf("upgrade: claim: %w", err)
	}
	return r, nil
}

// Finish records the outcome.
//
// appliedVersion is what the database actually reached, which on a
// failure may be neither where it started nor where it was going - a
// migration that applied four files of six leaves a real schema that is
// not either version, and saying so is the whole point of recording it
// separately from the state.
func Finish(ctx context.Context, pool *pgxpool.Pool, id int64, state State,
	appliedVersion *int, errChain string) error {

	tag, err := pool.Exec(ctx, `
		UPDATE panel_upgrade_requests
		SET state = $2, finished_at = now(), applied_version = $3, error_chain = $4
		WHERE id = $1`, id, string(state), appliedVersion, errChain)
	if err != nil {
		return fmt.Errorf("upgrade: finish %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		// Not a lost update to shrug at: the row this applier claimed is
		// gone or was changed underneath it, and the migration it just
		// ran is now unrecorded.
		return fmt.Errorf("upgrade: finish %d: the request is no longer there", id)
	}
	return nil
}

// Requeue puts a claimed request back into the queue, with a note saying
// why it did not run this time.
//
// The difference from Finish(StateFailed) is the difference between "it
// did not work" and "not now", and conflating them costs a customer a
// button press for a condition that clears by itself. The applier gives
// way to traffic on purpose - see applier.lockTimeout - so a busy table
// is an ordinary outcome of an ordinary run, not a fault.
//
// The note goes in error_chain because that is the column the health
// page already reads. The page labels it by state, so a pending row's
// note reads as the last attempt rather than as a failure - a wait
// presented as an error is how a working system gets restarted at three
// in the morning.
func Requeue(ctx context.Context, pool *pgxpool.Pool, id int64, note string) error {
	tag, err := pool.Exec(ctx, `
		UPDATE panel_upgrade_requests
		SET state = 'pending', claimed_at = NULL, claimed_by = '', error_chain = $2
		WHERE id = $1 AND state = 'running'`, id, note)
	if err != nil {
		return fmt.Errorf("upgrade: requeue %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("upgrade: requeue %d: the request is no longer claimed", id)
	}
	return nil
}

// ReleaseStaleClaims puts back requests an applier took and never
// finished.
//
// A process killed mid-migration leaves a row in `running` for ever, and
// the one-in-flight index then refuses every later request - a button
// that is permanently greyed out because of a crash weeks ago. Anything
// claimed longer ago than the timeout is moved to failed, which is
// honest (nobody knows whether it finished) and lets the next one
// through.
//
// The timeout has to be longer than a real migration, not longer than a
// fast one. Fifteen minutes: applying this schema takes under a second
// on the machines measured, and the gap is for a database under load, a
// lock wait, or a slow disk - none of which should look like a crash.
func ReleaseStaleClaims(ctx context.Context, pool *pgxpool.Pool, olderThan time.Duration) (int64, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE panel_upgrade_requests
		SET state = 'failed', finished_at = now(),
		    error_chain = 'the applier that claimed this never reported back; ' ||
		                  'whether the migration finished is not known'
		WHERE state = 'running' AND claimed_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int64(olderThan/time.Second)))
	if err != nil {
		return 0, fmt.Errorf("upgrade: release stale claims: %w", err)
	}
	return tag.RowsAffected(), nil
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
