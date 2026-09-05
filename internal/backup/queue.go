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

// Request is one row of the queue.
type Request struct {
	ID          int64
	RequestedAt time.Time
	Actor       Actor
	OperationID string

	Sets []string

	State State

	ClaimedAt  *time.Time
	ClaimedBy  string
	FinishedAt *time.Time
	ErrorChain string

	// BackupID is the catalogue row this produced, nil until it has.
	BackupID *int64
}

// InFlight reports whether this request is still going.
func (r *Request) InFlight() bool {
	return r != nil && (r.State == StatePending || r.State == StateRunning)
}

const requestColumns = `id, requested_at, actor_kind, actor_id, actor_label,
	operation_id, sets, state, claimed_at, claimed_by, finished_at,
	error_chain, backup_id`

func scanRequest(row pgx.Row) (*Request, error) {
	var r Request
	var state string
	err := row.Scan(&r.ID, &r.RequestedAt, &r.Actor.Kind, &r.Actor.ID, &r.Actor.Label,
		&r.OperationID, &r.Sets, &state, &r.ClaimedAt, &r.ClaimedBy,
		&r.FinishedAt, &r.ErrorChain, &r.BackupID)
	if err != nil {
		return nil, err
	}
	r.State = State(state)
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

	row := pool.QueryRow(ctx, `
		INSERT INTO panel_backup_requests
		  (actor_kind, actor_id, actor_label, operation_id, sets)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING `+requestColumns,
		a.Kind, a.ID, a.Label, operationID, Normalise(sets))

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
	if err := validateSets(r.Sets); err != nil {
		return r, fmt.Errorf("%w (claimed as request %d)", err, r.ID)
	}
	return r, nil
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

// Finish records how it went.
func Finish(ctx context.Context, pool *pgxpool.Pool, id int64, state State,
	cause error, backupID *int64) error {

	chain := ""
	if cause != nil {
		chain = cause.Error()
	}
	_, err := pool.Exec(ctx, `
		UPDATE panel_backup_requests
		   SET state = $2, finished_at = now(), error_chain = $3, backup_id = $4
		 WHERE id = $1`, id, string(state), chain, backupID)
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
	schema_version, state, device`

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
		if err := rows.Scan(&b.ID, &b.TakenAt, &b.Sets, &b.Bytes, &b.SHA256,
			&b.Version, &b.SchemaAt, &b.State, &b.Device); err != nil {
			return nil, fmt.Errorf("backup: listing: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
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
		if err := rows.Scan(&b.ID, &b.TakenAt, &b.Sets, &b.Bytes, &b.SHA256,
			&b.Version, &b.SchemaAt, &b.State, &b.Device, &b.Path); err != nil {
			return nil, fmt.Errorf("backup: listing with paths: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
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
