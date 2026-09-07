package backup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The queue: the panel asks, the upgrader answers.
//
// The third table in this repository with this exact shape, and the
// third time for the same reason: the asking side cannot do the work.
// For schema migrations it is DDL, for releases it is running code, and
// here it is reading rows - panel_user has no SELECT on the analytics
// tables, so it could not produce a dump if it wanted to.
//
// What is different here is the direction of the danger. The other two
// queues protect the machine from the panel. This one also protects the
// *data* from the file: see schema.sql on why the row carries no path.

// State is where a request has got to.
type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateSucceeded State = "succeeded"
	StateFailed    State = "failed"
)

var (
	// ErrAlreadyInFlight means one is already queued or running.
	ErrAlreadyInFlight = errors.New("backup: a backup is already in flight")
	// ErrNothingToDo means the queue is empty, which is not a failure.
	ErrNothingToDo = errors.New("backup: nothing to do")
)

// StaleAfter is how long a claim may go unfinished before the sweep
// takes it.
//
// An hour, and it is deliberately longer than the release queue's twenty
// minutes. That one moves a package of tens of megabytes over a network;
// this one reads every row a customer has and writes them compressed to
// a disk that may be the same spindle the database is on. A large
// deployment's traffic table is the slowest thing this product does.
//
// The cost of the number being too small is the worst available: a sweep
// that reclaimed a live claim would let a second backup start while the
// first was still writing, and both would be writing to the same
// temporary name.
const StaleAfter = time.Hour

// Actor is who asked, in the shape the audit log records one.
type Actor struct {
	Kind  string
	ID    *int64
	Label string
}

// Work is what a request asks the upgrader to do.
//
// A closed set, and the database carries the same list in a CHECK: a
// row naming work this build does not know is one no process could ever
// carry out, and it must not reach the queue at all.
type Work string

const (
	// WorkTake is "make me a backup".
	WorkTake Work = "al"
	// WorkVerify is "check the one I am pointing at".
	WorkVerify Work = "dogrula"
	// WorkRestore is "put the one I am pointing at into the side
	// database". Never the live one - see restore.go.
	WorkRestore Work = "geri_yukle"
)

// Request is one row of the queue.
type Request struct {
	ID          int64
	RequestedAt time.Time
	Actor       Actor
	OperationID string

	// Work says which of the two things this row is. Empty is not a
	// state a row can be in - the column has a default and a CHECK - so
	// a zero value here means a scan that did not happen.
	Work Work
	Sets []string
	// TargetID is the catalogue row this is about, for the kinds that
	// are about one. Nil for a take.
	TargetID *int64

	State State

	ClaimedAt  *time.Time
	ClaimedBy  string
	FinishedAt *time.Time
	ErrorChain string

	// BackupID is the catalogue row this produced, nil until it has.
	BackupID *int64
	// Result is what the work produced, in the words the page shows.
	// Empty for work whose whole outcome is "it happened".
	Result string
}

// InFlight reports whether this request is still going.
func (r *Request) InFlight() bool {
	return r != nil && (r.State == StatePending || r.State == StateRunning)
}

const requestColumns = `id, requested_at, actor_kind, actor_id, actor_label,
	operation_id, kind, sets, target_id, state, claimed_at, claimed_by,
	finished_at, error_chain, result, backup_id`

func scanRequest(row pgx.Row) (*Request, error) {
	var r Request
	var state, kind string
	err := row.Scan(&r.ID, &r.RequestedAt, &r.Actor.Kind, &r.Actor.ID, &r.Actor.Label,
		&r.OperationID, &kind, &r.Sets, &r.TargetID, &state, &r.ClaimedAt, &r.ClaimedBy,
		&r.FinishedAt, &r.ErrorChain, &r.Result, &r.BackupID)
	if err != nil {
		return nil, err
	}
	r.State = State(state)
	r.Work = Work(kind)
	return &r, nil
}

// Ask records that somebody wants a backup taken.
//
// The sets are checked here, before the row exists. A request naming a
// set this build does not know can never be carried out, and it should
// not occupy the one in-flight slot while somebody waits for an upgrader
// run to be told so.
//
// KindOf rather than TablesFor, because there are two artifacts now and
// only one of them resolves to tables. It refuses everything TablesFor
// refused - an unknown name, an empty list - and one thing more: a
// request naming the configuration alongside the data. That refusal
// belongs here as much as anywhere, because this is the last point
// before the row exists, and a row is what the upgrader obeys.
func Ask(ctx context.Context, pool *pgxpool.Pool, a Actor, operationID string,
	sets []string) (*Request, error) {

	if err := validateSets(sets); err != nil {
		return nil, err
	}
	return write(ctx, pool, a, operationID, WorkTake, Normalise(sets), nil)
}

// AskRestore records that somebody wants one put into the side
// database.
//
// The same shape as AskVerify and deliberately so: both name a file
// that already exists and neither chooses any sets. What separates them
// is what the upgrader does, and where - see restore.go on why the
// destination is a config file and never this row.
func AskRestore(ctx context.Context, pool *pgxpool.Pool, a Actor, operationID string,
	targetID int64) (*Request, error) {

	if targetID <= 0 {
		return nil, fmt.Errorf("backup: no backup was named to restore")
	}
	return write(ctx, pool, a, operationID, WorkRestore, nil, &targetID)
}

// AskVerify records that somebody wants one checked.
//
// Its own function rather than a `kind` argument on Ask, because the
// two take different things and the database refuses a row that mixes
// them: a take names sets and no target, a verification names a target
// and no sets. Two functions is how that becomes a compiler question
// rather than a runtime one.
func AskVerify(ctx context.Context, pool *pgxpool.Pool, a Actor, operationID string,
	targetID int64) (*Request, error) {

	if targetID <= 0 {
		return nil, fmt.Errorf("backup: no backup was named to check")
	}
	return write(ctx, pool, a, operationID, WorkVerify, nil, &targetID)
}

// write is the INSERT both entry points share.
func write(ctx context.Context, pool *pgxpool.Pool, a Actor, operationID string,
	work Work, sets []string, targetID *int64) (*Request, error) {

	if sets == nil {
		// Not null: the column is NOT NULL and its CHECK counts the
		// elements. An explicit empty array is what "this request is
		// not about sets" looks like in the row.
		sets = []string{}
	}
	row := pool.QueryRow(ctx, `
		INSERT INTO panel_backup_requests
		  (actor_kind, actor_id, actor_label, operation_id, kind, sets, target_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING `+requestColumns,
		a.Kind, a.ID, a.Label, operationID, string(work), sets, targetID)

	r, err := scanRequest(row)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrAlreadyInFlight
		}
		return nil, fmt.Errorf("backup: ask: %w", err)
	}
	return r, nil
}

// Latest is the most recent request, or nil when there has never been
// one.
//
// nil rather than an error for "never asked": it is the state every
// deployment starts in, and a page showing it should say "no backup has
// been requested", not "could not read the backup log".
func Latest(ctx context.Context, pool *pgxpool.Pool) (*Request, error) {
	r, err := scanRequest(pool.QueryRow(ctx,
		`SELECT `+requestColumns+` FROM panel_backup_requests ORDER BY id DESC LIMIT 1`))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("backup: latest: %w", err)
	}
	return r, nil
}

// Claim takes the waiting request, if there is one.
//
// The state is part of the UPDATE's WHERE rather than checked first and
// updated after: between a SELECT and an UPDATE another upgrader can do
// both, and the window is exactly as long as the round trip.
//
// The sets are checked again on the way out. Ask ran in the panel's
// process; this runs in the upgrader's, and the row in between was
// written by a role this process does not trust to have been honest. A
// claimant that reads its instructions out of a table validates them
// there.
func Claim(ctx context.Context, pool *pgxpool.Pool, by string) (*Request, error) {
	r, err := scanRequest(pool.QueryRow(ctx, `
		UPDATE panel_backup_requests
		   SET state = 'running', claimed_at = now(), claimed_by = $1
		 WHERE id = (SELECT id FROM panel_backup_requests
		              WHERE state = 'pending'
		              ORDER BY id LIMIT 1)
		   AND state = 'pending'
		RETURNING `+requestColumns, by))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, ErrNothingToDo
	case err != nil:
		return nil, fmt.Errorf("backup: claim: %w", err)
	}
	if err := validateRequest(r); err != nil {
		return r, fmt.Errorf("%w (claimed as request %d)", err, r.ID)
	}
	return r, nil
}

// validateRequest is what the answering side checks about a row it just
// claimed.
//
// One question, and it is the one SQL cannot ask: are the *names* in
// this row ones this build knows. A row written by a panel of a
// different version - naming a set that has since been renamed, or work
// this build does not do - satisfies every constraint on the table and
// is still something this process cannot carry out.
//
// Deliberately not repeating what the database already refuses. A
// verification with no target cannot be inserted at all
// (panel_backup_requests_kind_fields), so a check for it here would be
// a branch nothing can reach - measured by mutation: removing it left
// every test green. The nil is still checked where it is
// *dereferenced*, in Runner.check, which is a different reason.
func validateRequest(r *Request) error {
	switch r.Work {
	case WorkTake:
		return validateSets(r.Sets)
	case WorkVerify, WorkRestore:
		return nil
	default:
		return fmt.Errorf("backup: %q is not work this build knows how to do", r.Work)
	}
}

// validateSets is what both ends of the queue check, and it is one
// function so that they cannot drift apart.
//
// KindOf says the names are real and that the request does not name the
// configuration and the data together. TablesFor says a data request
// resolves to something to copy - a check that does not apply to the
// other artifact, whose contents are a directory rather than tables.
func validateSets(sets []string) error {
	kind, err := KindOf(sets)
	if err != nil {
		return err
	}
	if kind != KindData {
		return nil
	}
	_, err = TablesFor(sets)
	return err
}

// Outcome is everything a finished request records.
//
// A struct rather than five positional arguments, and it grew into one
// the moment a restore needed to say something on success: State, Cause
// and BackupID read the same way round and a fourth string beside them
// would be a call nobody could check by eye.
type Outcome struct {
	State State
	// Cause is what went wrong, nil on success.
	Cause error
	// Result is what the work produced, in the words the page shows.
	// Empty for work whose whole outcome is "it happened".
	Result string
	// BackupID is the catalogue row this produced, for a take.
	BackupID *int64
}

// Finish records how it went.
func Finish(ctx context.Context, pool *pgxpool.Pool, id int64, o Outcome) error {
	chain := ""
	if o.Cause != nil {
		chain = o.Cause.Error()
	}
	_, err := pool.Exec(ctx, `
		UPDATE panel_backup_requests
		   SET state = $2, finished_at = now(), error_chain = $3, result = $4,
		       backup_id = $5
		 WHERE id = $1`, id, string(o.State), chain, o.Result, o.BackupID)
	if err != nil {
		return fmt.Errorf("backup: finish: %w", err)
	}
	return nil
}

// ExpireStale frees the in-flight slot held by a claim nobody finished.
//
// A process killed mid-copy leaves its row `running`, and the
// one-in-flight index then refuses every later request. Without this the
// symptom is a button permanently dead because of a crash weeks ago,
// with nothing on the page explaining why.
func ExpireStale(ctx context.Context, pool *pgxpool.Pool, age time.Duration) (int64, error) {
	tag, err := pool.Exec(ctx, `
		UPDATE panel_backup_requests
		   SET state = 'failed', finished_at = now(),
		       error_chain = 'the upgrader claimed this and never finished'
		 WHERE state = 'running'
		   AND claimed_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int(age.Seconds())))
	if err != nil {
		return 0, fmt.Errorf("backup: expire: %w", err)
	}
	return tag.RowsAffected(), nil
}

// Catalogue

// Backup is one row of panel_backups: a file that exists on the disk.
type Backup struct {
	ID       int64
	TakenAt  time.Time
	Sets     []string
	Bytes    int64
	SHA256   string
	Version  string
	SchemaAt int64
	State    string
	// Device is the filesystem the file is on, zero when unknown. See
	// panel_backups.device.
	Device int64

	// VerifiedAt is when this file was last checked, nil when it never
	// has been. Nil is not "fine": it is the state every backup starts
	// in and the one this phase exists about.
	VerifiedAt *time.Time
	// VerifyProblems is what that check found, empty when it found
	// nothing. Several lines when it found several things.
	VerifyProblems string

	// Path is where the file is, and is empty for every reader that is
	// not the upgrader.
	//
	// Not by convention: panel_user is not granted this column, so a
	// SELECT naming it is refused by the database. List leaves it out
	// for that reason and ListWithPaths asks for it - see there.
	Path string
}

// catalogueColumns is what anybody may read.
const catalogueColumns = `id, taken_at, sets, bytes, sha256, binary_version,
	schema_version, state, device, verified_at, verify_problems`

// Record writes the catalogue row for a file that now exists.
func Record(ctx context.Context, pool *pgxpool.Pool, res Result) (int64, error) {
	var id int64
	err := pool.QueryRow(ctx, `
		INSERT INTO panel_backups
		  (taken_at, sets, bytes, sha256, path, binary_version, schema_version, device)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id`,
		res.TakenAt, res.Sets, res.Bytes, res.SHA256, res.Path,
		res.BinaryVersion, res.SchemaVersion, res.Device).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("backup: recording the catalogue row: %w", err)
	}
	return id, nil
}

// List is every backup, newest first, without paths.
//
// The columns are named rather than `SELECT *`, and that is what makes
// this callable by the panel at all: the panel's role has no grant on
// `path`, so a star would be refused. Naming them also means a column
// added later is not silently handed to a reader nobody thought about.
func List(ctx context.Context, pool *pgxpool.Pool) ([]Backup, error) {
	rows, err := pool.Query(ctx,
		`SELECT `+catalogueColumns+` FROM panel_backups ORDER BY taken_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("backup: listing: %w", err)
	}
	defer rows.Close()

	var out []Backup
	for rows.Next() {
		var b Backup
		if err := rows.Scan(backupTargets(&b)...); err != nil {
			return nil, fmt.Errorf("backup: listing: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// backupTargets is where catalogueColumns lands, in that order.
//
// One function rather than the same list written out at each call site.
// There are three of them now, and adding a column meant editing three
// Scan calls in lockstep - where getting it wrong lands every value one
// field to the left, silently, in types that mostly accept each other.
func backupTargets(b *Backup) []any {
	return []any{&b.ID, &b.TakenAt, &b.Sets, &b.Bytes, &b.SHA256,
		&b.Version, &b.SchemaAt, &b.State, &b.Device, &b.VerifiedAt, &b.VerifyProblems}
}

// ListWithPaths is List for the one caller that may see where the files
// are.
//
// Separated rather than a flag, so the call site says which one it is.
// A boolean parameter would put the decision at the caller's keyboard;
// two functions put it in the name, and the database refuses this one
// for anybody but the upgrader anyway.
func ListWithPaths(ctx context.Context, pool *pgxpool.Pool) ([]Backup, error) {
	rows, err := pool.Query(ctx,
		`SELECT `+catalogueColumns+`, path FROM panel_backups ORDER BY taken_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("backup: listing with paths: %w", err)
	}
	defer rows.Close()

	var out []Backup
	for rows.Next() {
		var b Backup
		if err := rows.Scan(append(backupTargets(&b), &b.Path)...); err != nil {
			return nil, fmt.Errorf("backup: listing with paths: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// WithPath is one catalogue row, for the caller that may see where the
// file is.
//
// Its own query rather than filtering ListWithPaths in Go: the row a
// verification names is one row, and reading every backup on the
// machine to find it would be a query whose cost grows with a number
// nobody bounded.
func WithPath(ctx context.Context, pool *pgxpool.Pool, id int64) (Backup, error) {
	var b Backup
	err := pool.QueryRow(ctx,
		`SELECT `+catalogueColumns+`, path FROM panel_backups WHERE id = $1`, id).
		Scan(append(backupTargets(&b), &b.Path)...)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Backup{}, fmt.Errorf("backup: there is no backup %d in the catalogue", id)
	case err != nil:
		return Backup{}, fmt.Errorf("backup: reading catalogue row %d: %w", id, err)
	}
	return b, nil
}

// MarkVerified records what a check found.
//
// Written even when the check found problems - especially then. The
// point of the column is that somebody can look at the list and see
// which files have been opened and what happened, and a verdict only
// recorded on success would leave a bad backup looking like an
// unchecked one.
func MarkVerified(ctx context.Context, pool *pgxpool.Pool, id int64, problems string) error {
	_, err := pool.Exec(ctx,
		`UPDATE panel_backups SET verified_at = now(), verify_problems = $2 WHERE id = $1`,
		id, problems)
	if err != nil {
		return fmt.Errorf("backup: recording the check of backup %d: %w", id, err)
	}
	return nil
}

// MarkMissing records that a file the catalogue names is no longer on
// the disk.
//
// Marked rather than deleted. An operator with a shell can remove a
// backup, and a row that simply vanished would leave the page saying
// nothing at all - where "there was a backup here and it is gone" is
// the sentence somebody needs to read.
func MarkMissing(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	_, err := pool.Exec(ctx,
		`UPDATE panel_backups SET state = 'missing' WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("backup: marking %d missing: %w", id, err)
	}
	return nil
}

// Forget removes a catalogue row whose file this process has just
// deleted.
//
// # Why this one deletes where MarkMissing marks
//
// The difference is who removed the file. A backup an operator deleted
// with a shell leaves a row saying "there was one here and it is gone",
// because that sentence is the only trace of a thing somebody may not
// have meant to do. A backup this process deleted on purpose, because
// the configured age limit came round, leaves nothing: the row would
// say a file is missing, which is true and misleading - it went where
// the setting said it should.
//
// Only schema_admin may run it. panel_backups carries a DELETE policy
// naming that role alone, and the reason is in the schema: forgetting
// the row is the half that follows deleting the file, and only the
// upgrader can do that.
func Forget(ctx context.Context, pool *pgxpool.Pool, id int64) error {
	if _, err := pool.Exec(ctx, `DELETE FROM panel_backups WHERE id = $1`, id); err != nil {
		return fmt.Errorf("backup: forgetting %d: %w", id, err)
	}
	return nil
}

// NotePolicy records the age limit in force, so the panel can say it.
//
// Written by the upgrader on every pass rather than once at startup: the
// row is only as true as the last process that wrote it, and an operator
// who edits upgrader.toml and restarts the timer should see the page
// change without anybody clearing a cache.
//
// The panel cannot write this. See panel_backup_policy in schema.sql for
// why that is the point rather than an omission.
func NotePolicy(ctx context.Context, pool *pgxpool.Pool, keepDays int) error {
	if keepDays < 0 {
		keepDays = 0
	}
	_, err := pool.Exec(ctx, `
		INSERT INTO panel_backup_policy (id, keep_days, noted_at)
		VALUES (1, $1, now())
		ON CONFLICT (id) DO UPDATE SET keep_days = $1, noted_at = now()`, keepDays)
	if err != nil {
		return fmt.Errorf("backup: recording the age limit: %w", err)
	}
	return nil
}

// Policy is what the panel reads back.
type Policy struct {
	// KeepDays is zero when backups are kept indefinitely.
	KeepDays int
	// NotedAt is when an upgrader last said so, and the zero time when
	// none ever has - which is a deployment whose upgrader has not run
	// since this version was installed, not one with no limit.
	NotedAt time.Time
}

// KeepsForever reports whether no age limit is in force.
func (p Policy) KeepsForever() bool { return p.KeepDays <= 0 }

// Known reports whether an upgrader has ever written the row.
//
// Separate from KeepsForever because the two are different sentences and
// only one of them is about backups: an unwritten row means nobody has
// told the panel anything, and a page that read it as "kept forever"
// would be making a promise on behalf of a process that has not run.
func (p Policy) Known() bool { return !p.NotedAt.IsZero() }

// ReadPolicy returns the age limit the panel may show.
//
// A missing row is not an error: it is every deployment between
// installing this version and the first upgrader pass.
func ReadPolicy(ctx context.Context, pool *pgxpool.Pool) (Policy, error) {
	var p Policy
	err := pool.QueryRow(ctx,
		`SELECT keep_days, noted_at FROM panel_backup_policy WHERE id = 1`).
		Scan(&p.KeepDays, &p.NotedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Policy{}, nil
	}
	if err != nil {
		return Policy{}, fmt.Errorf("backup: reading the age limit: %w", err)
	}
	return p, nil
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}

// BytesByDevice is what the backups occupy, per filesystem.
//
// # Why the sum happens here rather than over List
//
// The panel already reads the catalogue to draw the list, and adding the
// bytes up in Go would be free. It would also be a second place that
// decides what counts, and the two would drift: the list shows missing
// rows deliberately, and a bar must not.
//
// Files that are gone are excluded. A bar that included them would be a
// claim about a disk that the disk disagrees with, made on the one page
// somebody opens to find out how full the disk is.
//
// Rows with device zero are excluded from the map and returned as the
// second value, so a caller can show them in a total without attributing
// them to a filesystem. Zero means a backup taken before the column
// existed, or one whose filesystem could not be read - see
// panel_backups.device.
func BytesByDevice(ctx context.Context, pool *pgxpool.Pool) (map[int64]int64, int64, error) {
	rows, err := pool.Query(ctx, `
		SELECT device, sum(bytes)::BIGINT
		FROM panel_backups
		WHERE state <> 'missing'
		GROUP BY device`)
	if err != nil {
		return nil, 0, fmt.Errorf("backup: summing by filesystem: %w", err)
	}
	defer rows.Close()

	out := map[int64]int64{}
	var unplaced int64
	for rows.Next() {
		var device, bytes int64
		if err := rows.Scan(&device, &bytes); err != nil {
			return nil, 0, fmt.Errorf("backup: summing by filesystem: %w", err)
		}
		if device == 0 {
			unplaced += bytes
			continue
		}
		out[device] = bytes
	}
	return out, unplaced, rows.Err()
}
