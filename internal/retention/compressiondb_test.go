//go:build integration

// The compression suite's own database.
//
// # Why these tests do not share the development database
//
// Compression is not a row a test writes and deletes. Turning it on
// changes how a table is physically stored, for every session, for as
// long as the test runs - and `go test ./...` runs a dozen packages
// against the same database at the same time.
//
// Measured, twice, both times as a red gate in packages nobody had
// touched:
//
//   - internal/applier went red on every test, because TimescaleDB
//     refuses ALTER COLUMN on a compressed hypertable and two schema
//     files carried one. That found a real product bug, which is fixed.
//   - PostgreSQL segfaulted - "server process was terminated by signal
//     11", running a DELETE against beacon_events while these tests had
//     it compressed - and took the whole cluster into recovery, failing
//     every suite still running. Not reproduced in isolation; the run it
//     happened in had also exhausted TimescaleDB's background workers.
//
// The second one is the argument. Whether or not that crash is this
// suite's fault, a suite that can leave the shared cluster in recovery
// is a suite that must not run there.
//
// # It is also the more faithful fixture
//
// The database is built the way release/install.sh builds one: every
// schema file applied by the superuser, then release/sql/grants.sql,
// which hands ownership of every routine to schema_admin. So the
// SECURITY DEFINER wrappers here run as the role they run as on a real
// deployment - which is exactly the difference that made the first
// version of ca_set_compression pass its tests and work nowhere.
package retention_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/retention"
	"github.com/cruciblelab/crucible-analytic/internal/schemafiles"
	"github.com/jackc/pgx/v5/pgxpool"
)

// compressionDBName is fixed rather than random, so a run that dies
// leaves one database to find rather than a growing pile.
const compressionDBName = "ca_compression_suite"

// ready is false when there is no superuser connection to build the
// database with. The tests then skip, one by one, saying so.
var ready bool

func TestMain(m *testing.M) {
	code := func() int {
		super := os.Getenv("CA_SUPERUSER_DSN")
		if super == "" {
			fmt.Fprintln(os.Stderr,
				"CA_SUPERUSER_DSN is not set; the compression suite needs a database of its own")
			return m.Run()
		}
		ctx := context.Background()
		admin, err := pgxpool.New(ctx, super)
		if err != nil {
			fmt.Fprintf(os.Stderr, "connecting as the superuser: %v\n", err)
			return 1
		}
		defer admin.Close()

		// FORCE on the drop as well as the create: a scratch database
		// left by a run that died makes the next run fail in its fixture,
		// before any test has asserted anything.
		for _, sql := range []string{
			`DROP DATABASE IF EXISTS ` + compressionDBName + ` WITH (FORCE)`,
			`CREATE DATABASE ` + compressionDBName,
		} {
			if _, err := admin.Exec(ctx, sql); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", sql, err)
				return 1
			}
		}
		defer func() {
			if _, err := admin.Exec(context.Background(),
				`DROP DATABASE IF EXISTS `+compressionDBName+` WITH (FORCE)`); err != nil {
				fmt.Fprintf(os.Stderr, "dropping %s: %v\n", compressionDBName, err)
			}
		}()

		if err := install(ctx, super); err != nil {
			fmt.Fprintf(os.Stderr, "building %s: %v\n", compressionDBName, err)
			return 1
		}
		ready = true
		return m.Run()
	}()
	os.Exit(code)
}

// install does what release/install.sh does, in the same order.
func install(ctx context.Context, super string) error {
	pool, err := pgxpool.New(ctx, swapDatabase(super, compressionDBName))
	if err != nil {
		return err
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS timescaledb`); err != nil {
		return fmt.Errorf("timescaledb: %w", err)
	}
	// The roles are cluster-wide and already exist; only their rights in
	// this database do not.
	for _, sql := range []string{
		`GRANT CONNECT ON DATABASE ` + compressionDBName + ` TO ` + strings.Join(allRoles, ", "),
		`GRANT USAGE ON SCHEMA public TO ` + strings.Join(allRoles, ", "),
		`GRANT CREATE ON SCHEMA public TO schema_admin`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			return fmt.Errorf("%s: %w", sql, err)
		}
	}

	// The schema files, from the same list the applier uses, so a
	// mutation to any of them is applied here too.
	for _, f := range schemafiles.InOrder {
		if _, err := pool.Exec(ctx, f.SQL); err != nil {
			return fmt.Errorf("applying %s: %w", f.Path, err)
		}
	}

	// And then the privilege matrix, which is the half that decides who
	// owns the wrappers. Read from disk rather than embedded: it is not a
	// schema file, and a second copy of it here is a second thing to keep
	// in step with the file install.sh actually runs.
	root, err := repoRoot()
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

var allRoles = []string{"collector", "beacon_writer", "analytics_reader", "panel_user", "schema_admin"}

// repoRoot walks up for go.mod, so this works from `go test ./...` and
// from the package directory alike.
func repoRoot() (string, error) {
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

// swapDatabase points a DSN at another database on the same server.
//
// Built from the one connection string rather than asking for a second
// environment variable: an operator who set one and not the other would
// get a suite that compressed the live database.
func swapDatabase(dsn, name string) string {
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

// need skips a test when the suite's database could not be built.
func need(t *testing.T) {
	t.Helper()
	if !ready {
		t.Skip("set CA_SUPERUSER_DSN; the compression suite builds a database of its own")
	}
}

// poolAs connects to the suite's database as one service role.
//
// The passwords are the development cluster's convention - each role's
// password is its own name - and overridable per role, the same way
// internal/retention's other integration tests do it.
func poolAs(t *testing.T, role string) *pgxpool.Pool {
	t.Helper()
	need(t)
	dsn := os.Getenv("CA_DSN_" + role)
	if dsn == "" {
		dsn = fmt.Sprintf("postgres://%s:%s@localhost:5432/%s", role, role, compressionDBName)
	} else {
		dsn = swapDatabase(dsn, compressionDBName)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New as %s: %v", role, err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping as %s: %v", role, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// adminPool is the superuser, on the suite's database.
func adminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	need(t)
	pool, err := pgxpool.New(context.Background(),
		swapDatabase(os.Getenv("CA_SUPERUSER_DSN"), compressionDBName))
	if err != nil {
		t.Fatalf("pgxpool.New as the superuser: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// ownerOf is the service that writes a table, which is the only role
// allowed to ask for its retention or its compression.
func ownerOf(table retention.Table) string {
	if table == retention.TableTrafficSnapshots {
		return "collector"
	}
	return "beacon_writer"
}
