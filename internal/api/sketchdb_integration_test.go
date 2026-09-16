//go:build integration

// The visitor-sketch suite's own database.
//
// # Why these tests do not share the development database
//
// Because the thing under test is an extension. timescaledb_toolkit
// supplies the hyperloglog type, and installing it changes what every
// session in that database can see - which is the same argument the
// compression suite makes about its own physical change, and that one
// was learned from a cluster in recovery.
//
// It is also the more faithful fixture, for a second reason. The shared
// database does *not* have the extension, so the ordinary integration
// job measures the other half of this phase for free: on that database
// the sketch tables do not exist, the refresh reports itself
// unavailable, and every visitor count comes back exact. Both modes are
// therefore measured rather than argued about, and neither needs the
// other's absence simulated.
//
// # And it is built the way an install is
//
// Every schema file applied in order, then release/sql/grants.sql - so
// the privileges under test are the ones a deployment gets, including
// the guarded GRANT block that only fires where the extension is
// present. The refresher below runs as `collector` and the reader as
// `analytics_reader`, because a sketch written by a superuser would
// prove nothing about either.

package api

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/schemafiles"
	"github.com/cruciblelab/crucible-analytic/internal/storage"
)

// sketchDBName is fixed rather than random, so a run that dies leaves
// one database to find rather than a growing pile.
const sketchDBName = "ca_sketch_suite"

var (
	sketchOnce  sync.Once
	sketchDSN   string
	sketchWhyNo string
)

// sketchDB builds the suite's database once and returns a superuser DSN
// for it, or skips the test saying why.
//
// Lazily rather than in a TestMain, because this package has no TestMain
// and adding one would build a database for every test in it - including
// the several hundred that have nothing to do with sketches.
func sketchDB(t *testing.T) string {
	t.Helper()
	sketchOnce.Do(buildSketchDB)
	if sketchWhyNo != "" {
		t.Skip(sketchWhyNo)
	}
	return sketchDSN
}

func buildSketchDB() {
	super := os.Getenv("CA_SUPERUSER_DSN")
	if super == "" {
		sketchWhyNo = "set CA_SUPERUSER_DSN; the visitor-sketch suite builds a database of its own"
		return
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, super)
	if err != nil {
		sketchWhyNo = fmt.Sprintf("connecting as the superuser: %v", err)
		return
	}
	defer admin.Close()

	// The precondition, asked of the cluster rather than derived from a
	// CREATE EXTENSION that failed.
	//
	// This project has a rule about that, paid for in O1: a skip that
	// comes from the failure of the call under test is a skip that hides
	// the failure. So the question is put to pg_available_extensions,
	// and anything else going wrong below is reported as what it is.
	var available bool
	if err := admin.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_available_extensions
		                WHERE name = 'timescaledb_toolkit')`).Scan(&available); err != nil {
		sketchWhyNo = fmt.Sprintf("asking for timescaledb_toolkit: %v", err)
		return
	}
	if !available {
		sketchWhyNo = "this PostgreSQL cluster does not have timescaledb_toolkit available, " +
			"so the visitor sketches cannot be built here. The rest of the suite covers " +
			"the other half of O3 - a deployment without the extension - and the sketch " +
			"path is measured where the extension exists (locally, and in the nightly " +
			"workflow). Install timescaledb-toolkit-postgresql-16 to run these."
		return
	}

	for _, sql := range []string{
		// FORCE on the drop as well: a scratch database left by a run
		// that died makes the next run fail in its fixture, before any
		// test has asserted anything.
		`DROP DATABASE IF EXISTS ` + sketchDBName + ` WITH (FORCE)`,
		`CREATE DATABASE ` + sketchDBName,
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			sketchWhyNo = fmt.Sprintf("%s: %v", sql, err)
			return
		}
	}

	dsn := swapSketchDatabase(super, sketchDBName)
	if err := installSketchDB(ctx, dsn); err != nil {
		sketchWhyNo = fmt.Sprintf("building %s: %v", sketchDBName, err)
		return
	}
	sketchDSN = dsn
}

// installSketchDB does what release/install.sh does, in the same order.
func installSketchDB(ctx context.Context, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	for _, sql := range []string{
		`CREATE EXTENSION IF NOT EXISTS timescaledb`,
		// Before the schema files, which is the order install.sh uses -
		// and the order that matters: the sketch tables are created
		// only where this type already exists.
		`CREATE EXTENSION IF NOT EXISTS timescaledb_toolkit`,
		`GRANT CONNECT ON DATABASE ` + sketchDBName + ` TO ` + strings.Join(sketchRoles, ", "),
		`GRANT USAGE ON SCHEMA public TO ` + strings.Join(sketchRoles, ", "),
		`GRANT CREATE ON SCHEMA public TO schema_admin`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			return fmt.Errorf("%s: %w", sql, err)
		}
	}

	// From the applier's own list, so a mutation to any schema file is
	// applied here too.
	for _, f := range schemafiles.InOrder {
		if _, err := pool.Exec(ctx, f.SQL); err != nil {
			return fmt.Errorf("applying %s: %w", f.Path, err)
		}
	}

	// And the privilege matrix, read from disk rather than embedded: it
	// is not a schema file, and a second copy here is a second thing to
	// keep in step with the file install.sh actually runs.
	root, err := sketchRepoRoot()
	if err != nil {
		return err
	}
	grants, err := os.ReadFile(filepath.Join(root, "release", "sql", "grants.sql"))
	if err != nil {
		return fmt.Errorf("reading grants.sql: %w", err)
	}
	if _, err := pool.Exec(ctx, string(grants)); err != nil {
		return fmt.Errorf("applying release/sql/grants.sql: %w", err)
	}
	return nil
}

var sketchRoles = []string{"collector", "beacon_writer", "analytics_reader", "panel_user", "schema_admin"}

func sketchRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// swapSketchDatabase points a DSN at another database on the same
// server. Built from the one connection string rather than asking for a
// second environment variable: an operator who set one and not the other
// would get a suite that wrote into the live database.
func swapSketchDatabase(dsn, name string) string {
	cut := strings.LastIndex(dsn, "/")
	if cut < 0 {
		return dsn
	}
	rest := ""
	if q := strings.Index(dsn[cut:], "?"); q >= 0 {
		rest = dsn[cut:][q:]
	}
	return dsn[:cut+1] + name + rest
}

// sketchDSNFor is one service role's connection string for this suite's
// database.
//
// The passwords are the development cluster's convention - each role's
// password is its own name - and overridable per role, the way the other
// integration suites here do it. Built rather than derived from the
// superuser DSN by substitution: that string may carry its own password,
// and a textual swap would produce something that neither connects nor
// says why.
func sketchDSNFor(role string) string {
	if dsn := os.Getenv("CA_DSN_" + role); dsn != "" {
		return swapSketchDatabase(dsn, sketchDBName)
	}
	return fmt.Sprintf("postgres://%s:%s@localhost:5432/%s?sslmode=disable",
		role, role, sketchDBName)
}

// sketchPoolAs connects to the suite's database as one service role.
func sketchPoolAs(t *testing.T, role string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), sketchDSNFor(role))
	if err != nil {
		t.Fatalf("pgxpool.New as %s: %v", role, err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping as %s: %v", role, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// sketchFixture is one test's slice of the suite's database: a store to
// read through, a refresher to build sketches with, and a site nobody
// else touches.
type sketchFixture struct {
	Site      string
	Store     *Store
	Sketch    *storage.Sketch
	Collector *pgxpool.Pool
	Admin     *pgxpool.Pool
}

// newSketchFixture prepares one site.
//
// Each test gets a site_id of its own and deletes its rows afterwards,
// so the tests in this file may run in any order and in parallel with
// each other - the one thing they share is the database's shape, which
// none of them changes.
func newSketchFixture(t *testing.T, site string) *sketchFixture {
	t.Helper()
	dsn := sketchDB(t)

	admin, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New as the superuser: %v", err)
	}
	t.Cleanup(admin.Close)

	clean := func() {
		ctx := context.Background()
		for _, sql := range []string{
			`DELETE FROM traffic_snapshots WHERE site_id = $1`,
			`DELETE FROM visitor_sketch WHERE site_id = $1`,
			`DELETE FROM visitor_sketch_state WHERE site_id = $1`,
			`DELETE FROM traffic_rollup WHERE site_id = $1`,
			`DELETE FROM traffic_rollup_state WHERE site_id = $1`,
		} {
			if _, err := admin.Exec(ctx, sql, site); err != nil {
				t.Logf("cleaning %s: %v", sql, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)

	// Read as analytics_reader, which is the role the read API runs as.
	// A store opened as the owner would read through the GRANTs rather
	// than against them.
	store, err := NewStore(context.Background(), sketchDSNFor("analytics_reader"))
	if err != nil {
		t.Fatalf("NewStore as analytics_reader: %v", err)
	}
	t.Cleanup(store.Close)

	collector := sketchPoolAs(t, "collector")
	return &sketchFixture{
		Site:      site,
		Store:     store,
		Sketch:    storage.NewSketch(collector),
		Collector: collector,
		Admin:     admin,
	}
}

// Seed inserts one row per (address, instant), as the collector.
//
// As the collector and not as the owner, deliberately: a fixture that
// writes with more rights than the product has is a fixture that cannot
// notice a missing GRANT.
func (f *sketchFixture) Seed(t *testing.T, rows []sketchRow) {
	t.Helper()
	for _, r := range rows {
		if _, err := f.Collector.Exec(context.Background(), `
			INSERT INTO traffic_snapshots
			    (time, site_id, ip, ja4, prev_window_count, curr_window_count,
			     request_rate, bot_score)
			VALUES ($1, $2, $3::inet, '', 0, 1, 1.0, $4)`,
			r.At, f.Site, r.IP, r.Score,
		); err != nil {
			t.Fatalf("seeding %s at %s: %v", r.IP, r.At, err)
		}
	}
}

// sketchRow is one seeded snapshot.
type sketchRow struct {
	IP    string
	At    time.Time
	Score int
}

// Refresh materializes the sketches and fails the test if it could not.
func (f *sketchFixture) Refresh(t *testing.T) storage.SketchReport {
	t.Helper()
	report, err := f.Sketch.Refresh(context.Background(), f.Site)
	if err != nil {
		t.Fatalf("Sketch.Refresh: %v", err)
	}
	if report.Unavailable {
		t.Fatal("the refresh reported the sketch unavailable on a database built with " +
			"timescaledb_toolkit. Either the schema's conditional CREATE TABLE did not " +
			"fire, or the extension went missing between the fixture and here - and both " +
			"would make every assertion below vacuous.")
	}
	return report
}

// Visitors asks for the three figures over a range, with the estimate
// allowed (budget 0) or forbidden (budget very large).
//
// Two named calls rather than one with a boolean, because "which branch
// am I measuring" is the question every assertion in this file turns on.
func (f *sketchFixture) Estimated(t *testing.T, from, to time.Time, cutoff int) visitors {
	t.Helper()
	// 1 and not 0: zero reads as the measured budget, so that a Store
	// nobody configured behaves like the binary's.
	return f.visitors(t, from, to, cutoff, 1)
}

func (f *sketchFixture) Counted(t *testing.T, from, to time.Time, cutoff int) visitors {
	t.Helper()
	return f.visitors(t, from, to, cutoff, 1<<40)
}

func (f *sketchFixture) visitors(t *testing.T, from, to time.Time, cutoff, budget int) visitors {
	t.Helper()
	// The Store's own budget, moved for the duration of one call. The
	// rest of the path - the state row, the span arithmetic, the two
	// statements - is the production one.
	previous := f.Store.exactRows
	f.Store.exactRows = budget
	defer func() { f.Store.exactRows = previous }()

	state, err := f.Store.sketchStateOf(context.Background(), f.Site)
	if err != nil {
		t.Fatalf("sketchStateOf: %v", err)
	}
	// Two rows, always, so the branch is decided by the budget and never
	// by how big the fixture happened to be: against a budget of 1 two
	// rows are over it, and against a budget of 2^40 they are not.
	out, err := f.Store.visitorsOver(context.Background(), f.Site, from, to, cutoff, 2, state)
	if err != nil {
		t.Fatalf("visitorsOver: %v", err)
	}
	return out
}
