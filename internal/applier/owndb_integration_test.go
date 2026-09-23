//go:build integration

// The applier suite's own database.
//
// # Why this suite does not share the development database
//
// The applier applies the schema: every file, as the role that owns it,
// taking the locks DDL takes. Run against the database a dozen other
// packages are using at the same time, those locks are other suites'
// locks too - and this repository's own rule is that a suite which
// changes a database's shape must not run where other suites run
// (internal/retention moved for the same reason, after a segfault).
//
// Measured before the move (PLAN's open-risk row for this package, six
// gate runs): the applier was mostly the one *holding* - applying a
// schema file over beacon_events kept the beacon suite's INSERTs
// waiting, 129 samples in one run - and when it was the one waiting, it
// waited on the backup producer's COPY sitting idle in transaction. Its
// own duration was bimodal, 5.4-7.6 s or 65.7-72.1 s with nothing in
// between, and every red run was a slow one.
//
// # How the move is done without touching a test
//
// internal/testdb resolves every role's connection from the environment
// first (CA_DSN_<role>, CA_SUPERUSER_DSN) and falls back to the shared
// database. So TestMain builds a database the way release/install.sh
// builds one, points those variables at it, and runs the suite. The
// environment is this test binary's own - go test runs each package in
// its own process - so no other package sees the change.
//
// The suite still takes its advisory locks. They now coordinate only
// this package's tests with each other, which is harmless, and
// internal/invariants still asks for them: it exempts a suite from them
// only when it never reaches for internal/testdb, and this one does.
package applier

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/schemafiles"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// applierDBName is fixed rather than random, so a run that dies leaves
// one database to find rather than a growing pile.
const applierDBName = "ca_applier_suite"

func TestMain(m *testing.M) {
	os.Exit(func() int {
		super := os.Getenv("CA_SUPERUSER_DSN")
		if super == "" {
			// Without a superuser there is nothing to build the database
			// with. The suite then runs where it always did, on the shared
			// database, and says so - rather than skipping every test and
			// reporting the package green.
			fmt.Fprintln(os.Stderr, "CA_SUPERUSER_DSN is not set; the applier suite runs on the shared database")
			return m.Run()
		}
		ctx := context.Background()
		admin, err := pgxpool.New(ctx, super)
		if err != nil {
			fmt.Fprintf(os.Stderr, "connecting as the superuser: %v\n", err)
			return 1
		}
		defer admin.Close()

		// FORCE on both, so a database left by a run that died does not
		// fail the next one in its fixture.
		for _, sql := range []string{
			`DROP DATABASE IF EXISTS ` + applierDBName + ` WITH (FORCE)`,
			`CREATE DATABASE ` + applierDBName,
		} {
			if _, err := admin.Exec(ctx, sql); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", sql, err)
				return 1
			}
		}
		defer func() {
			if _, err := admin.Exec(context.Background(),
				`DROP DATABASE IF EXISTS `+applierDBName+` WITH (FORCE)`); err != nil {
				fmt.Fprintf(os.Stderr, "dropping %s: %v\n", applierDBName, err)
			}
		}()

		// swapDatabase is compressed_integration_test.go's; one copy. It
		// returns a DSN it cannot parse unchanged, which is why every
		// role is asked below where it actually landed.
		own := swapDatabase(super, applierDBName)
		if err := landsOn(ctx, own, applierDBName); err != nil {
			fmt.Fprintf(os.Stderr, "CA_SUPERUSER_DSN: %v\n", err)
			return 1
		}
		if err := installApplierDB(ctx, own); err != nil {
			fmt.Fprintf(os.Stderr, "building %s: %v\n", applierDBName, err)
			return 1
		}

		// Every way internal/testdb has of reaching a database, pointed
		// here. A role left out would quietly open the shared database -
		// so the list is the one testdb itself declares.
		os.Setenv("CA_SUPERUSER_DSN", own)
		for _, role := range testdb.AllRoles {
			os.Setenv("CA_DSN_"+role, swapDatabase(testdb.DSN(role), applierDBName))
		}
		// Then asked back through testdb itself, after the fact: what the
		// suite will open, not what this function meant to set. Both
		// halves - a role's override, and the superuser's - are the kind
		// of line that can go missing without a test noticing, and
		// either would put the suite back on the shared database while
		// it passed.
		for _, role := range testdb.AllRoles {
			if err := landsOn(ctx, testdb.DSN(role), applierDBName); err != nil {
				fmt.Fprintf(os.Stderr, "%s: %v\n", role, err)
				return 1
			}
		}
		if err := landsOn(ctx, os.Getenv("CA_SUPERUSER_DSN"), applierDBName); err != nil {
			fmt.Fprintf(os.Stderr, "CA_SUPERUSER_DSN: %v\n", err)
			return 1
		}
		return m.Run()
	}())
}

// landsOn asks the database which one a DSN reaches. Asked rather than
// assumed: a role whose override did not take would open the shared
// database, and the suite would pass while measuring the old thing.
func landsOn(ctx context.Context, dsn, want string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	var got string
	if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&got); err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("connected to %s, want %s", got, want)
	}
	return nil
}

// installApplierDB does what release/install.sh does, in the same order:
// the extension, the roles' rights in this database, every schema file
// as the superuser, then release/sql/grants.sql - which hands ownership
// to schema_admin, the role the applier applies as.
func installApplierDB(ctx context.Context, dsn string) error {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS timescaledb`); err != nil {
		return fmt.Errorf("timescaledb: %w", err)
	}
	roles := strings.Join(testdb.AllRoles, ", ")
	for _, sql := range []string{
		`GRANT CONNECT ON DATABASE ` + applierDBName + ` TO ` + roles,
		`GRANT USAGE ON SCHEMA public TO ` + roles,
		`GRANT CREATE ON SCHEMA public TO schema_admin`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			return fmt.Errorf("%s: %w", sql, err)
		}
	}
	for _, f := range schemafiles.InOrder {
		if _, err := pool.Exec(ctx, f.SQL); err != nil {
			return fmt.Errorf("applying %s: %w", f.Path, err)
		}
	}
	grants, err := os.ReadFile(filepath.Join("..", "..", "release", "sql", "grants.sql"))
	if err != nil {
		return fmt.Errorf("reading grants.sql: %w", err)
	}
	if _, err := pool.Exec(ctx, string(grants)); err != nil {
		return fmt.Errorf("applying release/sql/grants.sql: %w", err)
	}
	return nil
}
