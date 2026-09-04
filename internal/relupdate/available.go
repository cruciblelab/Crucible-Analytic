package relupdate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The row the upgrader writes and the panel reads.
//
// See manifest.go for why this exists at all: the panel cannot ask the
// release source itself without holding the key, and a version it
// displayed on its own authority would be a version with no signature
// behind it.
//
// # Why a failed check does not erase the last good answer
//
// Record writes both halves of the story - when we last looked, and
// when we last got an answer - so a source that goes down for a day
// leaves the page saying "the newest we know of is v0.21.0, and the last
// four checks failed" rather than "there is no newer version".
//
// The second sentence is the dangerous one. It is indistinguishable from
// "you are up to date", and it would be produced by exactly the
// situation where somebody most wants to know they are not: a release
// host an attacker has taken offline.

// Available is what the last check found.
type Available struct {
	// Version is the newest version the source published, empty when no
	// check has ever succeeded.
	Version string
	// ReleasedAt is when the source says it was released, zero when the
	// manifest did not say.
	ReleasedAt time.Time
	// NotesURL is where a person can read what changed, empty when
	// absent.
	NotesURL string

	// CheckedAt is when the upgrader last completed a check of any
	// outcome.
	CheckedAt time.Time
	// SucceededAt is when a check last produced a verified answer, zero
	// when none ever has.
	SucceededAt time.Time
	// Error is the last failure, empty when the last check worked.
	Error string
}

// Known reports whether a check has ever succeeded.
//
// The distinction the page draws first: a deployment that has never had
// an answer is not a deployment that is up to date.
func (a Available) Known() bool { return a.Version != "" && !a.SucceededAt.IsZero() }

// Stale reports whether the last successful check is older than age.
//
// Taken as a parameter rather than fixed here, because "old" is a
// property of how often the checker runs and this package does not
// decide that.
func (a Available) Stale(now time.Time, age time.Duration) bool {
	if a.SucceededAt.IsZero() {
		return true
	}
	return now.Sub(a.SucceededAt) > age
}

const availableColumns = `version, released_at, notes_url, checked_at, succeeded_at, error`

// ReadAvailable returns the last recorded answer.
//
// A missing row is the zero value and no error: a deployment where the
// upgrader has never run is the ordinary state on the day it is
// installed, and reporting it as a failure would put a warning on a
// page whose only problem is that it is new.
func ReadAvailable(ctx context.Context, pool *pgxpool.Pool) (Available, error) {
	var out Available
	var released, succeeded *time.Time
	err := pool.QueryRow(ctx,
		`SELECT `+availableColumns+` FROM panel_release_available WHERE id = 1`).
		Scan(&out.Version, &released, &out.NotesURL, &out.CheckedAt, &succeeded, &out.Error)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Available{}, nil
	case err != nil:
		return Available{}, fmt.Errorf("relupdate: reading the available version: %w", err)
	}
	if released != nil {
		out.ReleasedAt = released.UTC()
	}
	if succeeded != nil {
		out.SucceededAt = succeeded.UTC()
	}
	return out, nil
}

// RecordAvailable writes a successful check.
func RecordAvailable(ctx context.Context, pool *pgxpool.Pool, m Manifest, now time.Time) error {
	var released *time.Time
	if !m.Released.IsZero() {
		r := m.Released.UTC()
		released = &r
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO panel_release_available
		  (id, version, released_at, notes_url, checked_at, succeeded_at, error)
		VALUES (1, $1, $2, $3, $4, $4, '')
		ON CONFLICT (id) DO UPDATE SET
		  version = EXCLUDED.version,
		  released_at = EXCLUDED.released_at,
		  notes_url = EXCLUDED.notes_url,
		  checked_at = EXCLUDED.checked_at,
		  succeeded_at = EXCLUDED.succeeded_at,
		  error = ''`,
		m.Version, released, m.Notes, now.UTC())
	if err != nil {
		return fmt.Errorf("relupdate: recording the available version: %w", err)
	}
	return nil
}

// RecordCheckFailure writes that a check ran and did not produce an
// answer.
//
// Only checked_at and error move. The version, its date and
// succeeded_at are left exactly as they were, because they are still
// the last thing we actually know - and a failed check is not evidence
// that the previous answer became untrue.
func RecordCheckFailure(ctx context.Context, pool *pgxpool.Pool, cause string, now time.Time) error {
	if len(cause) > 500 {
		// Bounded, because this reaches a page. The interesting part of
		// a transport error is its beginning; the tail is a stack of
		// wrapping that says the same thing in four ways.
		cause = cause[:500] + "..."
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO panel_release_available (id, checked_at, error)
		VALUES (1, $1, $2)
		ON CONFLICT (id) DO UPDATE SET
		  checked_at = EXCLUDED.checked_at,
		  error = EXCLUDED.error`,
		now.UTC(), cause)
	if err != nil {
		return fmt.Errorf("relupdate: recording a failed check: %w", err)
	}
	return nil
}
