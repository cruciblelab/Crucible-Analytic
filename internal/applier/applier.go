package applier

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/schemafiles"
	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
	"github.com/cruciblelab/crucible-analytic/internal/upgrade"
)

// ErrNotThisBinary is returned when the request asks for a schema this
// applier does not carry.
//
// The check that makes the whole arrangement safe. A deployment can end
// up with a new panel and an old applier - somebody upgrades the
// packages one at a time, or a systemd unit was not restarted - and the
// old applier must refuse rather than migrate the database to a shape
// nobody asked for. Refusing is recoverable in one command; applying the
// wrong schema is not.
var ErrNotThisBinary = errors.New("upgrade: this applier does not carry the requested schema")

// Applier runs the DDL a request asks for.
//
// It is the only thing in this repository that does. Its pool connects
// as schema_admin, the fifth role, which owns the tables - so it can
// ALTER them - and holds no CREATE ROLE, no access to other databases,
// and nothing outside this schema. That is a real reduction from the
// superuser the installer uses, and it is the reason a fifth role exists
// rather than a superuser DSN sitting on disk for the lifetime of a
// service.
type Applier struct {
	Pool   *pgxpool.Pool
	Logger *slog.Logger
	// Name identifies this applier in claimed_by. The hostname, so a
	// two-host deployment's rows say which one took it.
	Name string
}

func (a *Applier) logger() *slog.Logger {
	if a.Logger == nil {
		return slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return a.Logger
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// RunOnce claims a waiting request, applies it, and records the outcome.
//
// Returns ErrNothingToDo when there is nothing waiting, which is what
// almost every run reports and is not a failure.
func (a *Applier) RunOnce(ctx context.Context) (*upgrade.Request, error) {
	// Stale claims first, so a request stranded by a crash does not keep
	// the queue blocked for ever behind the one-in-flight index.
	if released, err := upgrade.ReleaseStaleClaims(ctx, a.Pool, 15*time.Minute); err != nil {
		a.logger().Warn("upgrade: could not release stale claims", "err", err)
	} else if released > 0 {
		a.logger().Warn("upgrade: released a claim whose applier never reported back",
			"count", released)
	}

	req, err := upgrade.Claim(ctx, a.Pool, a.Name)
	if err != nil {
		return nil, err
	}

	log := a.logger().With("request", req.ID, "from", req.FromVersion, "to", req.ToVersion)

	// The check before the work. This binary's fingerprint is a constant
	// compiled in beside the embedded schema, and internal/schemafiles
	// proves at test time that the two describe the same bytes.
	if req.ToFingerprint != schemaver.Fingerprint {
		err := fmt.Errorf("%w: the request asks for %s and this binary carries %s",
			ErrNotThisBinary, short(req.ToFingerprint), short(schemaver.Fingerprint))
		log.Error("upgrade: refused", "err", err)
		// Recorded as failed with the reason, not left running: the
		// operator's fix is to upgrade this component, and they can only
		// find that out if the row says so.
		if finishErr := upgrade.Finish(ctx, a.Pool, req.ID, upgrade.StateFailed, nil, err.Error()); finishErr != nil {
			return req, errors.Join(err, finishErr)
		}
		return req, err
	}

	log.Info("upgrade: applying")
	applied, applyErr := a.apply(ctx)

	if applyErr != nil {
		log.Error("upgrade: failed", "err", applyErr, "reached", applied)
		if err := upgrade.Finish(ctx, a.Pool, req.ID, upgrade.StateFailed, applied, applyErr.Error()); err != nil {
			return req, errors.Join(applyErr, err)
		}
		return req, applyErr
	}

	log.Info("upgrade: applied", "version", schemaver.Version)
	reached := schemaver.Version
	if err := upgrade.Finish(ctx, a.Pool, req.ID, upgrade.StateSucceeded, &reached, ""); err != nil {
		return req, err
	}
	req.State = upgrade.StateSucceeded
	return req, nil
}

// apply runs every embedded schema file, in order, and records the
// version.
//
// Not one transaction. PostgreSQL supports transactional DDL and it is
// tempting, but the files contain CREATE INDEX and TimescaleDB calls
// that a single failing statement would roll back wholesale - turning a
// migration that got nine files in and stumbled on the tenth into one
// that reports failure and leaves no trace of the nine. The recorded
// applied_version is the honest account instead, and every file in this
// repository is written to be re-runnable: IF NOT EXISTS on every
// CREATE, DROP POLICY before every CREATE POLICY.
//
// That re-runnability is what makes retrying safe, and it is a property
// of the SQL rather than of this function - which is why the schema
// files say so in their own comments.
func (a *Applier) apply(ctx context.Context) (*int, error) {
	before, err := schemaver.Read(ctx, a.Pool)
	if err != nil && !errors.Is(err, schemaver.ErrNoTable) {
		return nil, fmt.Errorf("reading the version before applying: %w", err)
	}
	reached := before.Version

	for _, f := range schemafiles.InOrder {
		if _, err := a.Pool.Exec(ctx, f.SQL); err != nil {
			// The file is named because it is the one thing that turns
			// "the upgrade failed" into something an operator can act
			// on.
			return &reached, fmt.Errorf("applying %s: %w", f.Path, err)
		}
	}

	if err := a.record(ctx); err != nil {
		return &reached, err
	}
	reached = schemaver.Version
	return &reached, nil
}

// record writes what was applied.
//
// Last, and only after every file succeeded, because the row asserts
// that all of them were - the same reason schemaver's file is applied
// last of all in the list above.
func (a *Applier) record(ctx context.Context) error {
	_, err := a.Pool.Exec(ctx, `
		INSERT INTO schema_version (id, version, fingerprint, applied_at, applied_by)
		VALUES (1, $1, $2, now(), $3)
		ON CONFLICT (id) DO UPDATE
		SET version = EXCLUDED.version,
		    fingerprint = EXCLUDED.fingerprint,
		    applied_at = EXCLUDED.applied_at,
		    applied_by = EXCLUDED.applied_by`,
		schemaver.Version, schemaver.Fingerprint, "upgrader/"+a.Name)
	if err != nil {
		return fmt.Errorf("recording the schema version: %w", err)
	}
	return nil
}

// short trims a fingerprint for a message. The whole thing is 64
// characters and nobody compares them by eye past the first few.
func short(fingerprint string) string {
	if len(fingerprint) <= 12 {
		return fingerprint
	}
	return fingerprint[:12] + "…"
}
