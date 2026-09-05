package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/devseal"
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
	// ConfDir is the directory a secrets backup collects, and it is
	// derived from the path of this process's own config file rather
	// than configured.
	//
	// Nothing to set means nothing to point at the wrong place, and
	// nothing a request could name. The panel writes what to include;
	// where it is read from is no more the asking side's decision than
	// where it is written to.
	ConfDir string
	// Recipient is who can open a secrets backup. Unset means this
	// deployment does not take them - see ErrNoRecipient.
	Recipient devseal.Recipient
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
	case err != nil && req != nil:
		// Claimed, and then refused by the check Claim runs on the way
		// out: the row names something this build cannot carry out.
		//
		// Recorded here rather than returned bare, and that is a defect
		// this closes. The UPDATE already happened, so the row is
		// `running` with nobody working on it. Returning without
		// finishing it left it that way until ExpireStale released it
		// - into the next run, which claimed it and failed the same
		// check, forever, while the page said "Alınıyor".
		//
		// Reachable exactly where Claim says it is: a row written by a
		// panel of a different version, naming a set this build
		// renamed. That is the case the check exists for, and a check
		// whose only outcome is a wedged queue is not one.
		r.logger().Error("backup: claimed a request this build cannot carry out",
			"request", req.ID, "sets", req.Sets, "err", err)
		if finErr := Finish(ctx, r.Pool, req.ID, StateFailed, err, nil); finErr != nil {
			r.logger().Error("backup: could not record the outcome", "err", finErr)
		}
		return req, err
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

// ErrNotConfigured means this deployment has no backup directory.
//
// A sentinel rather than a sentence, because two callers need to tell it
// apart from every other failure and they want opposite things. A queued
// request must *fail with the reason on the row* - the page is where
// somebody is waiting and "nothing is configured" is the answer they
// need. A backup taken automatically before a schema upgrade must let
// the upgrade go ahead: a deployment that never configured backups is
// not one that just lost one.
var ErrNotConfigured = errors.New("backup: no directory is configured; set dir in " +
	"upgrader.toml's [backup] section")

// Take writes one backup and records it, without going through the
// queue.
//
// # Why this is not RunOnce
//
// RunOnce answers a request somebody made. This is for a backup nobody
// asked for by name: the one taken automatically before a schema
// upgrade, where the trigger is the upgrade and there is no row in the
// queue to claim, finish or show.
//
// Everything else is identical, and deliberately so - same estimate,
// same refusal when it will not fit, same file, same catalogue row. A
// backup taken by the machine that a person could not find beside the
// ones they took themselves would be a backup they do not know they
// have.
// # Why it takes no logger
//
// It did, and the first test written for it segfaulted: the parameter
// went straight to write, write calls log.Info, and a nil *slog.Logger
// panics on the first call. The caller that would have crashed is the
// applier, mid schema upgrade, which is the worst place in this product
// to panic - the claim is taken, the request is in flight, and the
// process dies without recording anything.
//
// The Runner already carries a Logger with a nil-safe accessor, so the
// parameter was a second way to say the same thing and only one of them
// was safe. Callers who want a line saying why a backup happened copy
// the Runner with a decorated logger; it is a value, and that is one
// statement. See cmd/upgrader.
func (r Runner) Take(ctx context.Context, sets []string) (*int64, error) {
	if r.Dir == "" {
		return nil, ErrNotConfigured
	}
	return r.write(ctx, sets, r.logger())
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
		//
		// Wrapped rather than returned bare, so the row carries the
		// sentence and errors.Is still finds the sentinel.
		return nil, fmt.Errorf("%w", ErrNotConfigured)
	}
	return r.write(ctx, req.Sets, log)
}

// write is the estimate, the copy and the catalogue row - the part both
// callers share.
func (r Runner) write(ctx context.Context, sets []string, log *slog.Logger) (*int64, error) {
	// Which of the two artifacts this is, decided here and not by the
	// caller. Both entry points - the queue and the pre-upgrade copy -
	// come through this function, so the refusal to put the
	// configuration and the traffic in one file is on the path every
	// backup takes rather than on the paths somebody remembered.
	kind, err := KindOf(sets)
	if err != nil {
		return nil, err
	}
	if kind == KindSecrets {
		return r.writeSecrets(ctx, log)
	}

	est, err := Measure(ctx, r.Pool, r.Dir, sets)
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
	res, err := w.Write(ctx, r.fileName(), sets)
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

// writeSecrets is the same three steps for the other artifact.
//
// Separate from write's body rather than folded into it with
// conditionals, because almost nothing is shared: different sizes,
// different source, different file. What is shared is that both refuse
// before writing when the disk cannot take it, and both record what
// they made - and those two are the reason this is here rather than in
// a package of its own.
func (r Runner) writeSecrets(ctx context.Context, log *slog.Logger) (*int64, error) {
	w := SecretsWriter{
		Pool:          r.Pool,
		ConfDir:       r.ConfDir,
		Dir:           r.Dir,
		Recipient:     r.Recipient,
		BinaryVersion: r.BinaryVersion,
		SchemaVersion: r.SchemaVersion,
	}
	if !w.Recipient.IsSet() {
		return nil, fmt.Errorf("%w", ErrNoRecipient)
	}

	// Collected before the estimate because the estimate is of these
	// bytes. The data backup can ask Postgres how big its tables are
	// without reading them; there is no equivalent question to ask a
	// directory that does not involve opening the files.
	files, _, err := CollectSecrets(w.ConfDir)
	if err != nil {
		return nil, err
	}
	est, err := MeasureSecrets(r.Dir, files)
	if err != nil {
		return nil, err
	}
	if !est.Fits() {
		return nil, fmt.Errorf("this backup needs about %d bytes and the disk has %d "+
			"available, keeping %d spare. It is short by %d. Nothing was written",
			est.FileBytes, est.AvailBytes, est.Margin, est.Short())
	}
	log.Info("backup: sealing the configuration",
		"files", len(files), "estimate", est.FileBytes, "available", est.AvailBytes)

	res, err := w.Write(ctx, r.secretsFileName())
	if err != nil {
		return nil, err
	}
	id, err := Record(ctx, r.Pool, res)
	if err != nil {
		return nil, fmt.Errorf("the secrets backup was written to %s and the catalogue row "+
			"could not be added (%w). The file is there and nothing on the page will "+
			"mention it", res.Path, err)
	}
	// Deliberately no line naming the files. The journal is read by
	// whoever has the machine, and "which configuration files exist" is
	// the one thing about this backup that is worth not saying twice.
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

// secretsFileName is the same shape with a different word.
//
// Different, because the two files must never be confused for one
// another by anybody sorting a directory: they have different contents,
// different protections and different restore procedures, and the one
// thing somebody does with a backup directory before anything else is
// look at the names.
func (r Runner) secretsFileName() string {
	return "sirlar-" + r.now().UTC().Format("20060102-150405.000") + ".tar.gz"
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

// Writable reports whether backups can actually be written here.
//
// # Why a probe and not a permission check
//
// The condition that broke every systemd install is invisible to any
// check short of writing: ProtectSystem=strict leaves the directory's
// mode and owner exactly as they were and remounts the filesystem
// read-only underneath. Mode 0700, owned by this account, on a
// filesystem nothing may write to. Anything that reads metadata says
// "writable".
//
// Found the same way internal/botdata found its own version of this,
// which is why this is written the same way: create a file, remove it,
// report what happened.
//
// # Why at startup rather than at the first press
//
// Because of who is standing there. A backup that fails when the button
// is pressed fails in front of the customer, with a sentence about a
// read-only filesystem that is not theirs to fix. Worse since the schema
// upgrade started taking one first: a directory that cannot be written
// stops upgrades too.
//
// At startup it is a line in the journal, addressed to the operator, at
// the moment they installed or reconfigured the thing.
//
// Returns nil when no directory is configured. That deployment takes no
// backups and needs no warning about a feature it did not turn on.
func (r Runner) Writable() error {
	if r.Dir == "" {
		return nil
	}
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return fmt.Errorf("backup: creating %s: %w", r.Dir, err)
	}
	probe, err := os.CreateTemp(r.Dir, ".yedek-yoklama-*")
	if err != nil {
		return fmt.Errorf("backup: writing to %s: %w", r.Dir, err)
	}
	name := probe.Name()
	_ = probe.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("backup: removing %s: %w", name, err)
	}
	return nil
}
