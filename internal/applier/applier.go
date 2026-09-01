package applier

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/dblock"
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

// ErrBusy is returned when a table was locked and the upgrade gave way.
//
// A separate error rather than a plain failure because the two ask
// different things of whoever reads them. A failure needs somebody to
// look; this needs nobody to do anything, because the request is back in
// the queue and the next tick will try again.
var ErrBusy = errors.New("upgrade: the tables were busy, so the upgrade gave way and will retry")

// isLockTimeout reports whether err is PostgreSQL's lock_timeout
// cancellation (SQLSTATE 55P03, lock_not_available).
//
// By code rather than by message text: the message is localised by the
// server's lc_messages, so a deployment whose PostgreSQL speaks Turkish
// would match nothing and every busy moment would be reported to its
// owner as a failed upgrade.
func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}

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

	// A lock timeout is "not now", and recording it as a failure would
	// make the customer press the button again for a condition that
	// clears by itself. Back into the queue, with the reason on the row,
	// and the next tick tries again.
	//
	// Safe to retry because every schema file is re-runnable - the
	// property the applier already depends on for a partial failure, and
	// the one internal/schemafiles' tests exist to keep.
	if isLockTimeout(applyErr) {
		log.Warn("upgrade: the tables were busy, leaving the request for the next run",
			"err", applyErr, "reached", applied)
		note := "bir tabloya kilit alınamadı, yükseltme sıraya geri kondu: " + applyErr.Error()
		if err := upgrade.Requeue(ctx, a.Pool, req.ID, note); err != nil {
			return req, errors.Join(applyErr, err)
		}
		return req, fmt.Errorf("%w: %w", ErrBusy, applyErr)
	}

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

// lockTimeout is how long the applier will wait for any single lock.
//
// # Why this exists, measured
//
// A schema file runs as one implicit transaction, so it holds every lock
// it has taken until the file finishes. A panel write that touches two
// of those tables in the other order - INSERT INTO panel_operations,
// which needs a lock on panel_users for its foreign key - closes a cycle,
// and PostgreSQL resolves a cycle by killing somebody:
//
//	Process A waits for RowShareLock on panel_users; blocked by B.
//	Process B waits for ShareLock on panel_operations; blocked by A.
//
// That is a real deadlock report from a real run, not a construction.
// The victim is whichever process the detector reaches first, so without
// this it can be the customer's write - and the button that started the
// upgrade told them it was safe to press while their site was serving.
//
// # Why shorter than deadlock_timeout rather than longer
//
// Deadlock detection only runs after a process has already waited
// deadlock_timeout, which defaults to 1s. Capping the applier's wait
// below that means it gives up and rolls back before any detector runs:
// the cycle is broken by the upgrade yielding, so there is no deadlock
// to resolve and no victim to choose. The traffic always wins.
//
// 250ms against measured lock waits in the single-digit milliseconds. It
// is not tuned to be tight; it is tuned to be well under 1s while being
// two orders of magnitude over what an uncontended apply needs.
//
// # What it costs
//
// An upgrade that cannot get its lock fails and is retried on the next
// tick, rather than queueing. That is the honest behaviour: a statement
// waiting for ACCESS EXCLUSIVE blocks everything that arrives behind it,
// so an applier that waited patiently would be the outage it was meant
// to avoid. A schema change that genuinely needs a quiet moment now says
// so instead of taking one.
const lockTimeout = "250ms"

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
//
// One pinned connection rather than the pool, because lock_timeout is
// session state: set on a borrowed connection it would apply to whatever
// the pool handed out next and to nothing reliably.
func (a *Applier) apply(ctx context.Context) (*int, error) {
	before, err := schemaver.Read(ctx, a.Pool)
	if err != nil && !errors.Is(err, schemaver.ErrNoTable) {
		return nil, fmt.Errorf("reading the version before applying: %w", err)
	}
	reached := before.Version

	conn, err := a.Pool.Acquire(ctx)
	if err != nil {
		return &reached, fmt.Errorf("acquiring a connection to apply on: %w", err)
	}
	defer func() {
		// Back to the pool as it was found. pgxpool does not reset session
		// state on release, so a connection left with a 250ms lock_timeout
		// would impose it on whatever borrowed it next - and the symptom
		// would be an unrelated statement failing under load, months later.
		if _, err := conn.Exec(context.WithoutCancel(ctx), `RESET lock_timeout`); err != nil {
			a.logger().Warn("upgrade: could not reset lock_timeout; dropping the connection",
				"err", err)
			// Destroy rather than return it: a connection whose state is
			// unknown is worse than one fewer connection.
			conn.Conn().Close(context.WithoutCancel(ctx))
		}
		conn.Release()
	}()

	if _, err := conn.Exec(ctx, `SET lock_timeout = '`+lockTimeout+`'`); err != nil {
		return &reached, fmt.Errorf("setting lock_timeout: %w", err)
	}

	// One applier inside the schema at a time. See dblock.SchemaApply for
	// what two of them cost each other, and why the answer is a lock
	// rather than making each statement collision-free.
	//
	// After the lock_timeout above, deliberately. A second applier waits
	// here - which is the point, since an apply takes about 25ms and
	// waiting for it is cheaper than requeuing - but it waits for 250ms
	// and no longer, and then fails with 55P03. That is already the path
	// that puts the request back in the queue for the next tick. Waiting
	// without a bound would hold a claim while doing nothing, which is
	// how the stale claim that allows two appliers gets created in the
	// first place.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(dblock.SchemaApply)); err != nil {
		return &reached, fmt.Errorf("taking the schema lock: %w", err)
	}
	defer func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, int64(dblock.SchemaApply)); err != nil {
			// Session-scoped, so a connection that keeps the lock keeps it
			// until it closes - and the next applier would meet a lock
			// nobody holds on purpose. Dropping the connection releases it.
			a.logger().Warn("upgrade: could not release the schema lock; dropping the connection",
				"err", err)
			conn.Conn().Close(context.WithoutCancel(ctx))
		}
	}()

	for _, f := range schemafiles.InOrder {
		if _, err := conn.Exec(ctx, f.SQL); err != nil {
			// The file is named because it is the one thing that turns
			// "the upgrade failed" into something an operator can act
			// on.
			return &reached, fmt.Errorf("applying %s: %w", f.Path, err)
		}
	}

	if err := a.recordOn(ctx, conn); err != nil {
		return &reached, err
	}
	reached = schemaver.Version
	return &reached, nil
}

// recordOn writes what was applied.
//
// Last, and only after every file succeeded, because the row asserts
// that all of them were - the same reason schemaver's file is applied
// last of all in the list above.
//
// On the caller's connection rather than the pool: it is the one that
// carries the lock_timeout, and this write is the last thing standing
// between an applied schema and a database that does not admit it was.
func (a *Applier) recordOn(ctx context.Context, conn *pgxpool.Conn) error {
	_, err := conn.Exec(ctx, `
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
