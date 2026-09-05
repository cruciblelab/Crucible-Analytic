package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The half that empties the queue.
//
// # Why this exists before anything can press the button
//
// Because the last time this project built a queue, five phases went by
// with Ask, Claim, Finish and ExpireStale all tested, a fetcher, an
// installer, a rollback and a panel button - and nothing called Claim.
// Pressing the button wrote a row no process ever read. The page said
// "Sırada" and went on saying it.
//
// An invariant in internal/invariants now fails when a package defines
// Claim and nothing outside it calls the runner. This file is what
// answers it, and it is written in the same commit as Claim rather than
// after it, so the queue never exists without a consumer.
//
// *Her halkası test edilmiş bir zincir, test edilmiş bir zincir
// değildir.*

// Runner takes one queued request and carries it out.
type Runner struct {
	Pool *pgxpool.Pool
	// Dir is where backups are written, from upgrader.toml. Never from
	// the request: see schema.sql.
	Dir string
	// Name identifies this upgrader in the claim, so two of them are
	// distinguishable in the row.
	Name string
	// BinaryVersion and SchemaVersion are stamped into the manifest.
	BinaryVersion string
	SchemaVersion int
	// Now names the file, so a test can place it on the clock.
	Now    func() time.Time
	Logger *slog.Logger
}

// RunOnce claims one request and finishes it.
//
// Returns ErrNothingToDo when the queue is empty, which is not a failure
// and is what a caller should not log.
func (r Runner) RunOnce(ctx context.Context) (*Request, error) {
	if freed, err := ExpireStale(ctx, r.Pool, StaleAfter); err != nil {
		r.logger().Warn("backup: could not expire stale claims", "err", err)
	} else if freed > 0 {
		r.logger().Warn("backup: released a claim nobody finished", "requests", freed)
	}

	req, err := Claim(ctx, r.Pool, r.name())
	switch {
	case errors.Is(err, ErrNothingToDo):
		return nil, ErrNothingToDo
	case err != nil:
		return nil, err
	case req == nil:
		return nil, ErrNothingToDo
	}

	log := r.logger().With("request", req.ID, "sets", req.Sets)
	log.Info("backup: starting")

	id, runErr := r.carryOut(ctx, req, log)

	state := StateSucceeded
	if runErr != nil {
		state = StateFailed
	}
	if finErr := Finish(ctx, r.Pool, req.ID, state, runErr, id); finErr != nil {
		// The work already happened. Reporting the write failure rather
		// than the work's own outcome would lose the more important of
		// the two.
		log.Error("backup: could not record the outcome", "err", finErr)
	}
	if runErr != nil {
		log.Error("backup: failed", "err", runErr)
		return req, runErr
	}
	log.Info("backup: done", "backup", id)
	return req, nil
}

// carryOut is the estimate, then the copy. Separated so RunOnce always
// records an outcome, whatever happens in here.
func (r Runner) carryOut(ctx context.Context, req *Request, log *slog.Logger) (*int64, error) {
	if r.Dir == "" {
		// Checked after the claim rather than before, deliberately. A
		// request queued on a deployment with no backup directory has to
		// end up *failed with a reason on the row*: the page is where
		// somebody is waiting, and "nothing is configured" is exactly
		// the answer they need.
		return nil, fmt.Errorf("the upgrader has no backup directory configured; " +
			"set dir in upgrader.toml's [backup] section")
	}

	est, err := Measure(ctx, r.Pool, r.Dir, req.Sets)
	if err != nil {
		return nil, err
	}
	if !est.Fits() {
		// Refused before a byte is written, with the numbers. A backup
		// that filled the disk would stop the collector, and the
		// collector is in front of the customer's website - so the one
		// outage this feature can cause is the one it causes by working.
		return nil, fmt.Errorf("this backup needs about %d bytes and the disk has %d "+
			"available, keeping %d spare. It is short by %d. Nothing was written; "+
			"choose fewer sets, shorten the retention period, or make the disk bigger",
			est.FileBytes, est.AvailBytes, est.Margin, est.Short())
	}
	log.Info("backup: copying",
		"tables_bytes", est.TableBytes, "estimate", est.FileBytes, "available", est.AvailBytes)

	w := Writer{
		Pool:          r.Pool,
		Dir:           r.Dir,
		BinaryVersion: r.BinaryVersion,
		SchemaVersion: r.SchemaVersion,
	}
	res, err := w.Write(ctx, r.fileName(), req.Sets)
	if err != nil {
		return nil, err
	}

	id, err := Record(ctx, r.Pool, res)
	if err != nil {
		// The file is on the disk and the catalogue does not know about
		// it. Said as itself rather than as "the backup failed": the
		// data is safe, and what is broken is the record of it.
		return nil, fmt.Errorf("the backup was written to %s and the catalogue row could "+
			"not be added (%w). The file is there and nothing on the page will mention it",
			res.Path, err)
	}
	return &id, nil
}

// fileName is the name a backup takes on disk.
//
// The timestamp is in it, and the sets are not. A name that described
// the contents would be a name somebody read instead of the catalogue,
// and the catalogue is the thing that knows - a file renamed by hand
// would then be lying about itself.
//
// # Why it carries milliseconds
//
// It did not, and a test written for something else found out why it has
// to. Two backups taken in the same second got the same name, the second
// rename replaced the first file, and the catalogue was left with two
// rows - two dates, two sizes, two checksums - pointing at one file.
//
// The customer sees two backups and has one. Nothing reports an error at
// any point: rename onto an existing name is a silent, atomic success,
// which is exactly the property that makes it the right call everywhere
// else in this file.
//
// One second is not a hypothetical window. It is how long RunOnce takes
// on a small deployment, so two requests answered back to back land
// inside it - which is how the test produced this on the first run.
func (r Runner) fileName() string {
	return "yedek-" + r.now().UTC().Format("20060102-150405.000") + ".tar.gz"
}

// Sweep marks catalogue rows whose files are gone.
//
// Run alongside RunOnce rather than on its own schedule: the question
// "are the backups still there" is only asked by a page, and a page that
// showed a file somebody deleted last week would be worse than one that
// showed nothing.
func (r Runner) Sweep(ctx context.Context) (int64, error) {
	rows, err := ListWithPaths(ctx, r.Pool)
	if err != nil {
		return 0, err
	}
	var marked int64
	for _, b := range rows {
		if b.State != "present" {
			continue
		}
		if _, err := os.Stat(b.Path); err == nil {
			continue
		}
		if err := MarkMissing(ctx, r.Pool, b.ID); err != nil {
			return marked, err
		}
		marked++
	}
	return marked, nil
}

func (r Runner) name() string {
	if r.Name != "" {
		return r.Name
	}
	return "upgrader"
}

func (r Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r Runner) logger() *slog.Logger {
	if r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}
