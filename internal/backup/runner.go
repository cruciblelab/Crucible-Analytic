package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
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
	// RestorePool is the side database a backup is put back into, from
	// `[backup] restore_dsn`. Nil means this deployment cannot restore,
	// and a queued request fails with that sentence on the row.
	//
	// Never the live pool. Everything that makes that true is in
	// restore.go, and none of it is this field being named carefully.
	RestorePool *pgxpool.Pool
	// Schema is what the side database's tables are built from.
	//
	// Supplied rather than imported: internal/schemafiles embeds this
	// package's own schema.sql, so importing it back would be a cycle.
	// cmd/upgrader is where the two halves are put together, which is
	// the same answer internal/applier's backup hook uses.
	Schema []SchemaFile
	// Name identifies this upgrader in the claim, so two of them are
	// distinguishable in the row.
	Name string
	// BinaryVersion and SchemaVersion are stamped into the manifest.
	BinaryVersion string
	SchemaVersion int
	// KeepDays is the age limit from `[backup] keep_days`, zero when
	// this deployment keeps its backups indefinitely.
	//
	// Here rather than a parameter to Expire, for the same reason Dir
	// is here rather than a parameter to RunOnce: it is read from the
	// same file at the same moment as the rest, and a value that
	// travels separately is one a second caller can pass differently.
	KeepDays int
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
		if finErr := Finish(ctx, r.Pool, req.ID,
			Outcome{State: StateFailed, Cause: err}); finErr != nil {
			r.logger().Error("backup: could not record the outcome", "err", finErr)
		}
		return req, err
	case err != nil:
		return nil, err
	case req == nil:
		return nil, ErrNothingToDo
	}

	// The work is in the line as well as the sets, because a
	// verification has no sets and a journal that only said "sets=[]"
	// would leave somebody guessing which of the two things this run
	// was.
	log := r.logger().With("request", req.ID, "work", string(req.Work), "sets", req.Sets)
	log.Info("backup: starting")

	done, runErr := r.carryOut(ctx, req, log)

	done.State = StateSucceeded
	if runErr != nil {
		done.State = StateFailed
	}
	done.Cause = runErr
	if finErr := Finish(ctx, r.Pool, req.ID, done); finErr != nil {
		// The work already happened. Reporting the write failure rather
		// than the work's own outcome would lose the more important of
		// the two.
		log.Error("backup: could not record the outcome", "err", finErr)
	}
	if runErr != nil {
		log.Error("backup: failed", "err", runErr)
		return req, runErr
	}
	log.Info("backup: done", "backup", done.BackupID)
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
func (r Runner) carryOut(ctx context.Context, req *Request, log *slog.Logger) (Outcome, error) {
	switch req.Work {
	case WorkVerify:
		// Checked before the directory, because neither of these needs
		// one: they open a file the catalogue already names. A
		// deployment whose [backup] dir was cleared can still check and
		// restore the files it took while it had one.
		return Outcome{}, r.check(ctx, req, log)
	case WorkRestore:
		return r.putBack(ctx, req, log)
	}
	if r.Dir == "" {
		// Checked after the claim rather than before, deliberately. A
		// request queued on a deployment with no backup directory has to
		// end up *failed with a reason on the row*: the page is where
		// somebody is waiting, and "nothing is configured" is exactly
		// the answer they need.
		//
		// Wrapped rather than returned bare, so the row carries the
		// sentence and errors.Is still finds the sentinel.
		return Outcome{}, fmt.Errorf("%w", ErrNotConfigured)
	}
	id, err := r.write(ctx, req.Sets, log)
	return Outcome{BackupID: id}, err
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

// check opens one backup and reports whether it is what the catalogue
// says it is.
//
// # Why a bad file fails the request rather than succeeding with a note
//
// Because of what the person pressing the button is asking. "Doğrula"
// is a yes-or-no question, and the answer has to arrive where they are
// looking - which is the request's line on the page, not a column in a
// table further down.
//
// So the verdict lands in two places on purpose, and they agree because
// they come from one Verification: the request fails with the problems
// on its row, and the catalogue row records the same problems against
// the file. The first is for the person standing there now; the second
// is for whoever asks next month which backups have ever been opened.
//
// A file that could not be read at all is a different outcome and says
// so: the catalogue is left alone rather than being marked with a
// verdict nobody reached.
func (r Runner) check(ctx context.Context, req *Request, log *slog.Logger) error {
	if req.TargetID == nil {
		// Unreachable through the queue: the table's CHECK refuses a
		// verification with no target, so no such row can exist.
		//
		// Kept because this is the line that dereferences it, and the
		// alternative to a sentence here is a nil dereference in the
		// upgrader - which takes the process down mid-claim, leaving a
		// row running that nothing will finish. "Cannot happen" is the
		// reason to check cheaply, not the reason to skip it.
		return errors.New("backup: this request names no backup to check")
	}
	row, err := WithPath(ctx, r.Pool, *req.TargetID)
	if err != nil {
		return err
	}

	result, err := Verify(row.Path, row, r.Recipient)
	if err != nil {
		// The file could not be opened or decompressed. Nothing about
		// its contents was measured, so nothing is written to the
		// catalogue: "checked and unreadable" and "never checked" are
		// different states and the second must not be overwritten by a
		// verdict that was never reached.
		//
		// Except for the one case where it is a finding rather than an
		// accident: a file the catalogue names and the disk does not
		// have. Sweep already marks those, and saying it here as well
		// costs a sentence and answers the question that was asked.
		log.Error("backup: could not check", "backup", row.ID, "err", err)
		return err
	}

	problems := strings.Join(result.Problems, "\n")
	if markErr := MarkVerified(ctx, r.Pool, row.ID, problems); markErr != nil {
		// The check happened and its result could not be stored.
		// Reported as itself: what is broken is the record, and the
		// file's verdict is in the error below either way.
		log.Error("backup: could not record the check", "backup", row.ID, "err", markErr)
	}

	if !result.OK() {
		log.Error("backup: the file is not what the catalogue says it is",
			"backup", row.ID, "problems", len(result.Problems))
		return fmt.Errorf("this backup did not pass:\n%s", problems)
	}
	log.Info("backup: checked", "backup", row.ID,
		"bytes", result.Bytes, "rows", result.RowsFound(), "sealed", result.Secrets)
	return nil
}

// putBack restores one backup into the side database.
//
// # Why the outcome is a paragraph and not a state
//
// Because "it worked" is not what somebody who pressed this wants to
// know. They want which tables came back, with how many rows, from
// which schema version, and how long it took - the whole point of a
// rehearsal is the numbers it produces. So the report goes on the
// request row and the page shows it, and a restore that succeeded says
// more than one that failed.
//
// A failure part-way through still reports what got in before it
// stopped. That is deliberate: "beacon_events stopped at column X and
// the four tables before it are all there" is the sentence that tells
// somebody how bad it is.
func (r Runner) putBack(ctx context.Context, req *Request, log *slog.Logger) (Outcome, error) {
	if req.TargetID == nil {
		// Unreachable through the queue, for the reason check gives:
		// the table's CHECK refuses a restore with no target. Kept
		// because this is where it is dereferenced.
		return Outcome{}, errors.New("backup: this request names no backup to restore")
	}
	if r.RestorePool == nil {
		return Outcome{}, fmt.Errorf("%w", ErrNoRestoreTarget)
	}
	row, err := WithPath(ctx, r.Pool, *req.TargetID)
	if err != nil {
		return Outcome{}, err
	}
	if len(row.Sets) == 1 && row.Sets[0] == SetSirlar {
		// The configuration is not rows and there is nowhere to put it.
		// Refused with the sentence that says what to do instead,
		// because somebody pressing this on the wrong row has a
		// reasonable expectation and needs redirecting rather than
		// stopping.
		return Outcome{}, errors.New("backup: this is a secrets backup, not data. It " +
			"holds configuration files, so there is no database to put it into - open " +
			"it with `devpass -open` on a machine where you have the developer password")
	}

	report, err := RestoreInto(ctx, r.Pool, r.RestorePool, row.Path, r.Schema, log)
	out := Outcome{Result: restoreSummary(report)}
	if err != nil {
		return out, err
	}
	return out, nil
}

// restoreSummary is the report in the words the page shows.
//
// Built even for a failed restore, because a partial one is the case
// worth reading: it says how far it got. Empty only when nothing was
// attempted at all, which is what a refusal before the wipe looks like.
func restoreSummary(rep RestoreReport) string {
	if rep.Database == "" {
		return ""
	}
	lines := []string{fmt.Sprintf("veritabanı: %s", rep.Database)}
	if rep.SchemaOfFile != 0 && rep.SchemaOfFile != rep.SchemaOfBuild {
		lines = append(lines, fmt.Sprintf("yedeğin şeması %d, kurulan şema %d",
			rep.SchemaOfFile, rep.SchemaOfBuild))
	}
	if len(rep.Tables) > 0 {
		lines = append(lines, fmt.Sprintf("%d satır, %d tablo: %s",
			rep.Rows(), len(rep.Tables), rep.Summary()))
	}
	if rep.Took > 0 {
		lines = append(lines, fmt.Sprintf("süre: %s", rep.Took.Round(time.Millisecond)))
	}
	return strings.Join(lines, "\n")
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

// Expire deletes data backups older than KeepDays and forgets their
// rows, and returns how many went.
//
// KeepDays <= 0 does nothing at all, which is the default and the
// behaviour every deployment has today. See applier.BackupConfig for
// why that is the default rather than a number.
//
// # What this is actually for
//
// The retention policy deletes analytics rows past their age. A backup
// taken before that day still holds them. So a backup directory is the
// one place the retention number quietly does not apply, and a
// deployment that keeps backups forever keeps the data forever - past
// the promise made to the customer and to their visitors.
//
// # The newest one is never deleted
//
// Not even when it is older than the limit. A deployment whose last
// backup was taken four hundred days ago, under a three-hundred-day
// limit, would otherwise be swept to *no backup at all* - and "an old
// backup" and "no backup" are not points on the same scale. The limit
// exists to stop data outliving its retention, and the last file is the
// one whose deletion costs more than it saves.
//
// It is a guard rather than a config option because there is no
// deployment that wants the other behaviour, and an option would be a
// way to ask for it by accident.
//
// # Secrets backups are not touched
//
// They carry no visitor data, so the sentence above does not reach
// them, and they are what restores a machine rather than a database.
// See applier.BackupConfig.KeepDays.
//
// # The file goes first
//
// Delete, then forget. The other order leaves a file on disk that
// nothing knows about, which is a backup nobody will ever verify and
// nobody will ever delete. A file this cannot remove keeps its row, so
// the next sweep tries again and the page still lists it.
func (r Runner) Expire(ctx context.Context) (int64, error) {
	if r.KeepDays <= 0 {
		return 0, nil
	}
	rows, err := ListWithPaths(ctx, r.Pool)
	if err != nil {
		return 0, err
	}

	cutoff := r.now().Add(-time.Duration(r.KeepDays) * 24 * time.Hour)

	// The newest data backup, found before anything is deleted rather
	// than by relying on the list's order. ListWithPaths is ordered
	// today; a function that quietly depends on that is one that breaks
	// when somebody adds a second caller who wants a different order.
	var newest int64
	var newestAt time.Time
	for _, b := range rows {
		if !r.isData(b) {
			continue
		}
		if newest == 0 || b.TakenAt.After(newestAt) {
			newest, newestAt = b.ID, b.TakenAt
		}
	}

	var gone int64
	for _, b := range rows {
		if !r.isData(b) || b.ID == newest {
			continue
		}
		if !b.TakenAt.Before(cutoff) {
			continue
		}
		// A row already marked missing has no file to delete; the row
		// is still forgotten, because the sentence it was keeping -
		// "there was a backup here" - is about a file that is now past
		// the age at which this deployment keeps them anyway.
		if b.State == "present" {
			if err := os.Remove(b.Path); err != nil && !os.IsNotExist(err) {
				return gone, fmt.Errorf("backup: removing %s: %w", b.Path, err)
			}
		}
		if err := Forget(ctx, r.Pool, b.ID); err != nil {
			return gone, err
		}
		gone++
	}
	return gone, nil
}

// isData reports whether a catalogue row is a data backup.
//
// A row whose sets cannot be read at all is treated as *not* data,
// which is the safe direction: this function decides what to delete,
// and a row nobody can classify is a row to leave alone.
func (r Runner) isData(b Backup) bool {
	kind, err := KindOf(b.Sets)
	return err == nil && kind == KindData
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
