package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
)

// Putting a backup back, into a database that is not the live one.
//
// # Why a side database and not the live one
//
// PLAN.md F1: "Geri yüklemede panel gösterir, hazırlar, sorar; takası
// yapmaz." Writing over live data is the one irreversible thing this
// product could do, and the panel is the surface facing the internet -
// a stolen session must not be able to replace a customer's history
// with an older copy of it, quietly.
//
// So the panel prepares a restore into a database somebody else made,
// shows what came back, and stops. Swapping it in front of the services
// stays in the shell and is documented. The one step that cannot be
// undone is the step that already needed a shell.
//
// # And why the destination is a config file rather than a request
//
// The same reason `[backup] dir` is: a database named in the queue
// would be a database a compromised panel could choose, and this
// operation *wipes the database it is pointed at*. It comes from
// upgrader.toml, which the panel cannot open, and the account that can
// edit that file is the one that could do this by hand anyway.
//
// # What stops it wiping the live database
//
// Two things, and only the second of them is the enforcement.
//
// The marker table is the enforcement: a target that holds tables and
// does not carry the marker this feature writes is refused. The live
// database holds thirty of them and no marker, so it is refused, and so
// is any other real database somebody points this at by mistake. It
// needs no privilege and asks nothing of the operator.
//
// The same-database probe runs first and only to produce a better
// sentence - "this is the live database" rather than "this database has
// tables I did not make". It is not load-bearing: if it could not run
// at all, the marker check still refuses. Said explicitly because a
// check that looks like a guarantee and is not is worse than no check.

// SchemaFile is one file of the product's schema, in the shape
// internal/schemafiles produces.
//
// # Why this package does not import that one
//
// It cannot: internal/schemafiles embeds this package's own schema.sql,
// so importing it back would be a cycle. That is not an accident to
// work around - it is the dependency pointing the right way, and the
// answer is the one internal/applier already uses for its backup hook:
// the caller supplies the files, and cmd/upgrader is where the two
// halves are read together.
type SchemaFile struct {
	Path string
	SQL  string
}

// RestoreMarker is the table this feature writes into a target database
// to say that it owns it.
//
// The name is deliberately not one of the product's. A database this
// belongs to is a database whose whole contents this may destroy, and
// that claim needs a word that appears nowhere else.
const RestoreMarker = "panel_restore_target"

// restoreProbe is the advisory-lock key the same-database probe uses.
//
// PostgreSQL scopes an advisory lock to the database the session is
// connected to, which is exactly the question this has to answer.
//
// Written the other way round first - "cluster-wide" - and the
// measurement said no. Two sessions on one server, one holding the key
// in `analytics` and one trying for it in another database:
//
//	same cluster, other database: pg_try_advisory_lock = t
//	same cluster, same database:  pg_try_advisory_lock = f
//
// So a failed try means the other session is in *this* database, and
// that is the whole answer. Recorded rather than quietly corrected,
// because the wrong version of this sentence is one somebody could
// build a second check on.
//
// Its own key rather than the one internal/dblock names for schema
// application: this is held for milliseconds, and a collision with the
// applier would make a restore report the wrong thing about a database
// it never touched.
const restoreProbe = 0x796564656b // "yedek"

// ErrNoRestoreTarget means this deployment has nowhere to restore into.
var ErrNoRestoreTarget = errors.New("backup: no restore database is configured; " +
	"create one and name it in upgrader.toml's [backup] restore_dsn")

// ErrLiveDatabase means the target is the database this product runs on.
var ErrLiveDatabase = errors.New("backup: the restore database is the live database")

// ErrNotARestoreTarget means the target holds tables this feature did
// not put there.
var ErrNotARestoreTarget = errors.New("backup: the restore database holds tables that " +
	"were not put there by a restore")

// RestoreReport is what a restore put back, and what it cost.
type RestoreReport struct {
	// Database is the name of the target, for the sentence the page
	// shows. Not the DSN: that carries a password.
	Database string
	// SchemaOfFile is what the backup was taken from, SchemaOfBuild what
	// the tables were built as. They differ when an older backup is
	// restored, which is allowed - see the note on RestoreInto.
	SchemaOfFile  int
	SchemaOfBuild int
	// Tables is every table restored, with the count the manifest
	// claimed beside the count that arrived.
	Tables []Restored
	// Took is how long it ran, because "it worked" and "it worked in
	// forty minutes" are different answers for somebody deciding
	// whether to do it again on the real thing.
	Took time.Duration
}

// Rows is the total put back.
func (r RestoreReport) Rows() int64 {
	var n int64
	for _, t := range r.Tables {
		n += t.Rows
	}
	return n
}

// Summary is the report as the page and the log line show it.
func (r RestoreReport) Summary() string {
	parts := make([]string, 0, len(r.Tables))
	for _, t := range r.Tables {
		parts = append(parts, fmt.Sprintf("%s: %d", t.Table, t.Rows))
	}
	return strings.Join(parts, ", ")
}

// RestoreInto wipes the target, builds this build's schema in it, and
// puts the backup's rows back.
//
// # What happens to an older backup
//
// It is attempted, and that is the answer to the question PLAN.md left
// open. The tables are built from the schema this build embeds - there
// is no other schema to build, since internal/schemafiles carries one
// version - and the rows go in through COPY naming the columns the
// manifest recorded.
//
// So a column added since the backup was taken is absent from the COPY
// list and takes its default: the restore works. A column removed or
// renamed since is named in the list and does not exist: the restore
// stops, on that table, saying which one. Both outcomes are right and
// neither is a guess.
//
// Nothing is refused up front for being old. The attempt *is* the
// measurement, it happens in a database whose whole purpose is being
// wiped, and "your 2024 backup restores except for one column in
// beacon_events" is a far more useful sentence than "too old".
func RestoreInto(ctx context.Context, live, target *pgxpool.Pool, path string,
	schema []SchemaFile, log *slog.Logger) (RestoreReport, error) {

	started := time.Now()
	if target == nil {
		return RestoreReport{}, ErrNoRestoreTarget
	}
	if len(schema) == 0 {
		// Refused rather than restored into an empty database. A COPY
		// into a table that does not exist fails on the first one, so
		// this would be caught either way - but "no schema was supplied"
		// says which of the two things went wrong, and the other message
		// would send somebody to look at the backup file.
		return RestoreReport{}, errors.New("backup: no schema was supplied to build the " +
			"restore database with")
	}

	name, err := databaseName(ctx, target)
	if err != nil {
		return RestoreReport{}, err
	}
	report := RestoreReport{Database: name, SchemaOfBuild: schemaver.Version}

	if same, err := sameDatabase(ctx, live, target); err != nil {
		// Not fatal, because this probe is not the enforcement: the
		// marker check below refuses the live database whether or not
		// this ran. Logged so a deployment where it never works is
		// visible rather than silently relying on the second check.
		log.Warn("backup: could not tell whether the restore database is the live one",
			"err", err)
	} else if same {
		return report, fmt.Errorf("%w (%s). Nothing was touched", ErrLiveDatabase, name)
	}

	if err := checkTarget(ctx, target); err != nil {
		return report, err
	}

	// Everything below this line destroys the target.
	log.Warn("backup: wiping the restore database", "database", name)
	if err := wipe(ctx, target); err != nil {
		return report, err
	}
	// Every connection dropped, between emptying the database and
	// filling it again.
	//
	// # The measurement this line exists because of
	//
	// The second restore into the same database failed, and the first
	// did not:
	//
	//	extension "timescaledb" has already been loaded with another
	//	version (SQLSTATE 42710)
	//
	// Dropping the schema drops the extension, and PostgreSQL will not
	// let a backend that has already loaded the timescaledb library
	// create it again - the restriction is per connection, not per
	// database. The first restore ran on a connection that had never
	// seen it; the second reused the one that had.
	//
	// So the pool is reset and the schema goes in on new sessions.
	// Found by restoring twice, which is what somebody rehearsing a
	// recovery actually does.
	target.Reset()

	for _, f := range schema {
		if _, err := target.Exec(ctx, f.SQL); err != nil {
			return report, fmt.Errorf("backup: building %s in %s: %w", f.Path, name, err)
		}
	}
	if err := mark(ctx, target); err != nil {
		return report, err
	}

	m, err := ReadManifest(path)
	if err != nil {
		return report, err
	}
	report.SchemaOfFile = m.SchemaVersion
	if m.SchemaVersion != schemaver.Version {
		log.Warn("backup: restoring a backup from another schema",
			"file", m.SchemaVersion, "build", schemaver.Version)
	}

	rows, err := Restore(ctx, target, path)
	report.Tables = rows
	report.Took = time.Since(started)
	if err != nil {
		return report, err
	}
	log.Info("backup: restored", "database", name,
		"rows", report.Rows(), "tables", len(report.Tables), "took", report.Took)
	return report, nil
}

// databaseName is what the target calls itself.
func databaseName(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var name string
	if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil {
		return "", fmt.Errorf("backup: reading the restore database's name: %w", err)
	}
	return name, nil
}

// sameDatabase reports whether two pools are connected to the same
// database.
//
// # Why an advisory lock and not a comparison of the connection strings
//
// Because the strings can differ and mean the same thing - localhost
// against 127.0.0.1, a socket against a port, a name with and without
// its default, two pools built from one DSN. A restore that wiped the
// live database because two spellings did not match would be the worst
// failure this product is capable of.
//
// An advisory lock is scoped to the database the session is in - see
// restoreProbe, where that is measured rather than remembered. So
// holding the key on the live pool and trying for it on the target
// answers the question outright: it fails only when the target is in
// the same database. No privilege, no names, no guessing.
//
// # Why the names are not compared as well
//
// They were, and the comparison was removed: with the probe being
// exact, a name check in front of it is a second answer to a question
// that already has one, and a mutation showed it changed nothing.
// Names are still read - the report shows one - but nothing decides on
// them.
func sameDatabase(ctx context.Context, live, target *pgxpool.Pool) (bool, error) {
	if live == nil || target == nil {
		return false, errors.New("backup: no live pool to compare the target against")
	}

	//
	// Both sides *try* rather than wait. A live side that blocked would
	// hang a restore behind whoever held the key; worse, a live side
	// that waited and then succeeded while something else still held it
	// on the target's cluster would read as "same server" when it is
	// not. Failing to take it means the probe cannot answer, and it
	// says so rather than guessing.
	held, release, err := tryProbeLock(ctx, live)
	if err != nil {
		return false, fmt.Errorf("backup: probing the live database: %w", err)
	}
	defer release()
	if !held {
		return false, errors.New("backup: something else holds the probe lock, so this " +
			"cannot tell the two databases apart")
	}

	free, releaseTarget, err := tryProbeLock(ctx, target)
	if err != nil {
		return false, fmt.Errorf("backup: probing the restore database: %w", err)
	}
	defer releaseTarget()
	// The target took it, so the live pool's lock is not in its way:
	// two different PostgreSQLs that happen to use the same database
	// name. It could not, so they are the same one.
	//
	// The false direction of this is safe and the true direction is the
	// one that matters. A third process holding this key on the
	// target's cluster would make it read "same" and refuse a
	// legitimate restore; nothing else takes this key, and refusing is
	// the direction to be wrong in.
	return !free, nil
}

// tryProbeLock takes the cluster-wide probe lock without waiting, and
// returns the function that gives it back.
//
// The release closure rather than a *pgxpool.Conn, because the two
// failure paths need different endings: an unlock that worked returns
// the connection to the pool, and one that did not has to close it.
// Advisory locks are session-scoped, so a connection handed back still
// holding one would make the next probe answer the opposite of the
// truth.
func tryProbeLock(ctx context.Context, pool *pgxpool.Pool) (bool, func(), error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return false, func() {}, err
	}
	var got bool
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock($1)`, int64(restoreProbe)).Scan(&got); err != nil {
		conn.Release()
		return false, func() {}, err
	}
	if !got {
		conn.Release()
		return false, func() {}, nil
	}
	return true, func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock($1)`, int64(restoreProbe)); err != nil {
			conn.Hijack().Close(context.WithoutCancel(ctx))
			return
		}
		conn.Release()
	}, nil
}

// checkTarget refuses a database this feature does not own.
//
// The rule is narrow on purpose: empty is fine, and so is one this
// feature has wiped before. Anything else - a database with tables
// somebody else made - is refused, because the next thing this function
// does is destroy every one of them.
func checkTarget(ctx context.Context, target *pgxpool.Pool) error {
	var tables, markers int
	err := target.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE tablename <> $1),
		       count(*) FILTER (WHERE tablename =  $1)
		  FROM pg_tables WHERE schemaname = 'public'`, RestoreMarker).Scan(&tables, &markers)
	if err != nil {
		return fmt.Errorf("backup: looking at the restore database: %w", err)
	}
	if tables == 0 || markers == 1 {
		return nil
	}
	return fmt.Errorf("%w: it holds %d tables and no %s. Point restore_dsn at a database "+
		"made for this and nothing else - everything in it is destroyed on every "+
		"restore. Nothing was touched", ErrNotARestoreTarget, tables, RestoreMarker)
}

// wipe empties the target.
//
// DROP SCHEMA ... CASCADE rather than dropping the tables this build
// knows: a target left over from an older build holds tables this one
// has never heard of, and a restore into a database with a stranger's
// table in it is a restore whose result nobody can read. The whole
// schema goes, which is also the plainest thing to say in a document -
// "everything in that database is destroyed".
func wipe(ctx context.Context, target *pgxpool.Pool) error {
	for _, stmt := range []string{
		`DROP SCHEMA IF EXISTS public CASCADE`,
		`CREATE SCHEMA public`,
	} {
		if _, err := target.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("backup: emptying the restore database: %w. The account in "+
				"restore_dsn has to own that database - `CREATE DATABASE ... OWNER "+
				"schema_admin`", err)
		}
	}
	return nil
}

// mark writes the table that says this database belongs to the feature.
//
// After the wipe rather than before, because the wipe removes it: the
// marker records the *last* restore, and a target that was wiped and
// then failed half way through still carries it - which is right, since
// it is still a database this feature owns and may wipe again.
func mark(ctx context.Context, target *pgxpool.Pool) error {
	if _, err := target.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS `+RestoreMarker+` (
		    restored_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		    note TEXT NOT NULL DEFAULT ''
		)`); err != nil {
		return fmt.Errorf("backup: marking the restore database: %w", err)
	}
	if _, err := target.Exec(ctx, `INSERT INTO `+RestoreMarker+` (note) VALUES ($1)`,
		fmt.Sprintf("crucible-analytic restore, schema %d", schemaver.Version)); err != nil {
		return fmt.Errorf("backup: marking the restore database: %w", err)
	}
	return nil
}
