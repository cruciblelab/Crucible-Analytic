// Package relupdate is the queue behind "move this deployment to a
// newer version".
//
// # What it is, in one sentence
//
// The panel writes a row saying which version somebody wants; the
// upgrader reads it, fetches that version from an address in its own
// configuration, checks our signature, installs, and writes back what
// happened.
//
// # Why a queue rather than the panel doing it
//
// The schema queue exists because the panel cannot run DDL. This one
// exists because the panel must not run code, which is the stronger
// form of the same rule: a panel that could install binaries would, once
// compromised, own the machine - and the panel is what faces the
// internet.
//
// So the split is the same and the reasons stack:
//
//   - The panel may INSERT and SELECT. It asks, and it reads the answer.
//   - The upgrader may SELECT and UPDATE. It answers, and cannot ask.
//   - The address packages come from is in upgrader.toml, never in a
//     request, so a compromised panel cannot choose a source.
//   - The signing key is in upgrader.toml too, so it could not use a
//     chosen source if it could choose one.
//
// The last two are separate on purpose. Either alone would be an
// argument; together they are a property, because breaking it needs both
// the panel and a file the panel cannot read.
package relupdate

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// State is where a request has got to.
type State string

const (
	// StatePending is written by the panel and read by the upgrader.
	StatePending State = "pending"
	// StateRunning means an upgrader has claimed it.
	StateRunning State = "running"
	// StateSucceeded means the new version is installed and started.
	StateSucceeded State = "succeeded"
	// StateFailed means it is not. Which does not mean the machine is
	// broken: see Request.RolledBack, which is the difference between
	// "we did not manage it" and "we did not manage it and left you
	// something that does not run".
	StateFailed State = "failed"
)

var (
	// ErrAlreadyInFlight is returned when a request is already waiting
	// or running. Not a fault: two people pressing the button within a
	// second of each other is the ordinary way to get here.
	ErrAlreadyInFlight = errors.New("relupdate: a request is already in flight")
	// ErrNothingToDo is what the upgrader sees on almost every run.
	ErrNothingToDo = errors.New("relupdate: no request is waiting")
	// ErrBadVersion means the version string is not one this code will
	// put in a URL. See ValidVersion.
	ErrBadVersion = errors.New("relupdate: that is not a version this can fetch")
)

// version is the shape a version string may have.
//
// # Why this is strict rather than forgiving
//
// The value becomes part of an address the upgrader fetches. Anything
// that can carry a slash can carry a path, anything that can carry a
// dot-dot can climb out of one, and anything that can carry a colon or
// a second scheme can change the host. A version is a short, boring
// token and there is no reason for it to be anything else.
//
// So: `v`, three numbers, and optionally a build-metadata tag of
// letters, digits and dots - which is exactly what VERSIONING.md
// defines, including the `+L3` phase codes. Nothing else, and the
// anchors are the load-bearing part: `^` and `$` are what stop
// "v1.2.3/../../etc" from matching the front of the string and passing.
//
// Not a substitute for the signature, and not sold as one. A version
// that passes this can still name a package we never made, and that
// package still has to verify against the key in upgrader.toml. This
// check is about what a *request* can make the fetcher do before any
// bytes arrive.
var version = regexp.MustCompile(`^v[0-9]{1,6}\.[0-9]{1,6}\.[0-9]{1,6}(\+[A-Za-z0-9.]{1,32})?$`)

// ValidVersion reports whether v is a version this will fetch.
//
// Exported because two callers need the same answer and must not each
// have their own idea of it: the panel refuses early so somebody gets a
// sentence rather than a failed request, and the upgrader refuses again
// because a row in the database is not evidence about how it got there.
func ValidVersion(v string) bool { return version.MatchString(v) }

// Actor is who asked, in the shape the audit log records one.
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

	FromVersion string
	ToVersion   string

	State State

	ClaimedAt  *time.Time
	ClaimedBy  string
	FinishedAt *time.Time
	ErrorChain string

	InstalledVersion string
	RolledBack       bool
}

// InFlight reports whether this request is still going.
func (r *Request) InFlight() bool {
	return r != nil && (r.State == StatePending || r.State == StateRunning)
}

const columns = `id, requested_at, actor_kind, actor_id, actor_label, operation_id,
	from_version, to_version, state, claimed_at, claimed_by, finished_at,
	error_chain, installed_version, rolled_back`

func scan(row pgx.Row) (*Request, error) {
	var r Request
	var state string
	err := row.Scan(&r.ID, &r.RequestedAt, &r.Actor.Kind, &r.Actor.ID, &r.Actor.Label,
		&r.OperationID, &r.FromVersion, &r.ToVersion, &state, &r.ClaimedAt,
		&r.ClaimedBy, &r.FinishedAt, &r.ErrorChain, &r.InstalledVersion, &r.RolledBack)
	if err != nil {
		return nil, err
	}
	r.State = State(state)
	return &r, nil
}

// Ask records that somebody wants this deployment moved to a version.
//
// The version is checked here, before the row exists. A request that
// could never be fetched should not occupy the in-flight slot and should
// not need an upgrader run to be told so.
func Ask(ctx context.Context, pool *pgxpool.Pool, a Actor, operationID,
	fromVersion, toVersion string) (*Request, error) {

	if !ValidVersion(toVersion) {
		return nil, fmt.Errorf("%w: %q", ErrBadVersion, toVersion)
	}

	row := pool.QueryRow(ctx, `
		INSERT INTO panel_release_requests
		  (actor_kind, actor_id, actor_label, operation_id, from_version, to_version)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING `+columns,
		a.Kind, a.ID, a.Label, operationID, fromVersion, toVersion)

	r, err := scan(row)
	if err != nil {
		// 23505 is unique_violation, which here can only be the
		// one-in-flight index. Matched on the code rather than the
		// message, which is localised and has changed between releases.
		if isUniqueViolation(err) {
			return nil, ErrAlreadyInFlight
		}
		return nil, fmt.Errorf("relupdate: ask: %w", err)
	}
	return r, nil
}

// Latest is the most recent request, or nil when there has never been
// one.
//
// nil rather than an error for "never asked": it is the state every
// deployment starts in, and a page showing it should say "no update has
// been requested", not "could not read the update log".
func Latest(ctx context.Context, pool *pgxpool.Pool) (*Request, error) {
	r, err := scan(pool.QueryRow(ctx,
		`SELECT `+columns+` FROM panel_release_requests ORDER BY id DESC LIMIT 1`))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("relupdate: latest: %w", err)
	}
	return r, nil
}

// Claim takes the waiting request, if there is one.
//
// The state is part of the UPDATE's WHERE rather than checked first and
// updated after: between a SELECT and an UPDATE another upgrader can do
// both, and the window is exactly as long as the round trip.
//
// The version is checked again on the way out, and this is not
// belt-and-braces. Ask ran in the panel's process; this runs in the
// upgrader's, and the row in between was written by a role the upgrader
// does not trust to have been honest. A claimant that reads its
// instructions out of a table has to validate them there.
func Claim(ctx context.Context, pool *pgxpool.Pool, by string) (*Request, error) {
	r, err := scan(pool.QueryRow(ctx, `
		UPDATE panel_release_requests
		   SET state = 'running', claimed_at = now(), claimed_by = $1
		 WHERE id = (SELECT id FROM panel_release_requests
		              WHERE state = 'pending'
		              ORDER BY id LIMIT 1)
		   AND state = 'pending'
		RETURNING `+columns, by))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrNothingToDo
	case err != nil:
		return nil, fmt.Errorf("relupdate: claim: %w", err)
	}
	if !ValidVersion(r.ToVersion) {
		return r, fmt.Errorf("%w: %q (claimed as request %d)",
			ErrBadVersion, r.ToVersion, r.ID)
	}
	return r, nil
}

// Finish records how it went.
//
// installed is what is running now, which on a rollback is the version
// this machine came from rather than the one that was asked for. Saying
// "failed" and leaving the version blank would leave the one question
// that matters unanswered.
func Finish(ctx context.Context, pool *pgxpool.Pool, id int64, state State,
	cause error, installed string, rolledBack bool) error {

	chain := ""
	if cause != nil {
		chain = cause.Error()
	}
	_, err := pool.Exec(ctx, `
		UPDATE panel_release_requests
		   SET state = $2, finished_at = now(), error_chain = $3,
		       installed_version = $4, rolled_back = $5
		 WHERE id = $1`, id, string(state), chain, installed, rolledBack)
	if err != nil {
		return fmt.Errorf("relupdate: finish: %w", err)
	}
	return nil
}

// ExpireStale frees the in-flight slot held by a claim nobody finished.
//
// A process killed mid-download leaves its row `running`, and the
// one-in-flight index then refuses every later request. Without this the
// symptom is a button permanently dead because of a crash weeks ago,
// with nothing on the page explaining why.
//
// Only rows claimed longer ago than age, and only ones still running: a
// sweep that could touch a live claim would cut off an update while it
// was replacing binaries, which is the worst moment available.
func ExpireStale(ctx context.Context, pool *pgxpool.Pool, age time.Duration) (int64, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE panel_release_requests
		   SET state = 'failed', finished_at = now(),
		       error_chain = 'the upgrader claimed this and never finished'
		 WHERE state = 'running'
		   AND claimed_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int(age.Seconds())))
	if err != nil {
		return 0, fmt.Errorf("relupdate: expire: %w", err)
	}
	return tag.RowsAffected(), nil
}

// StaleAfter is how long a claim may go unfinished before the sweep
// takes it.
//
// Twenty minutes, and the number comes from the slowest thing this can
// legitimately do: fetch a package over a link somebody is also serving
// a website on, unpack it, and start five services. An upgrade that has
// been running for twenty minutes is not slow, it is gone.
//
// Deliberately far above the schema queue's equivalent. That one runs a
// migration measured in milliseconds; this one moves megabytes.
const StaleAfter = 20 * time.Minute

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}
