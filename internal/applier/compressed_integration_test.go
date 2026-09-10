//go:build integration

package applier

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/schemafiles"
	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The upgrade path on a deployment that compresses.
//
// # What this is for
//
// O1 turns compression on, on both hypertables, from the first minute
// the two services run. TimescaleDB refuses several ordinary forms of
// DDL on a hypertable that has compression enabled - measured on 2.17.2,
// and refused unconditionally, including when the statement would change
// nothing:
//
//	refused                        allowed
//	ALTER COLUMN ... DROP NOT NULL ADD COLUMN
//	ALTER COLUMN ... TYPE          DROP COLUMN
//	ADD CONSTRAINT ... CHECK       ALTER COLUMN ... SET DEFAULT
//	ENABLE ROW LEVEL SECURITY      CREATE INDEX, GRANT, OWNER TO
//
// Two statements in the schema were of the first kind, one per
// hypertable, and both had been there for months. They were harmless
// until compression existed. Afterwards they meant this: install works,
// the services start, compression comes on - and the *next* schema
// upgrade fails, permanently, on every deployment. That upgrade is the
// panel's Health page button, which is the only upgrade path a customer
// has.
//
// # Why it is a test and not a rule written down somewhere
//
// A list of forbidden DDL kept in a comment is a list somebody has to
// remember to read. This asks the database instead: apply every schema
// file to a database that is compressed exactly the way a deployment is,
// and require it to succeed. A future statement of the same shape fails
// here, when it is written, rather than at a customer's upgrade.
//
// The hypertables are read out of TimescaleDB's own catalogue rather
// than named, so a schema that adds a third one is covered by this test
// on the day it is added and without anybody editing it.
func TestAnUpgradeStillWorksOnACompressedDeployment(t *testing.T) {
	ctx := context.Background()
	pool := freshDatabase(t, "ca_upgrade_compressed")

	// The precondition, asked of the database rather than derived from a
	// failure of our own: an Apache-licensed build cannot compress, and
	// this test has nothing to say about one. Deriving it from an error
	// this test caused would be a skip that hides its own bug - the
	// lesson O1's first compression suite paid for.
	var license string
	if err := pool.QueryRow(ctx,
		`SELECT current_setting('timescaledb.license', true)`).Scan(&license); err != nil {
		t.Fatalf("asking the database about its license: %v", err)
	}
	if license != "timescale" {
		t.Skip("this TimescaleDB is the Apache build; compression does not exist here")
	}

	compressed := compressEveryHypertable(t, pool)
	if compressed == 0 {
		t.Fatal("no hypertable was compressed, so applying the schema below " +
			"proves nothing about a compressed deployment")
	}

	// Read back, because the claim is about a compressed database and
	// the setup above is the only thing that makes it one. A test that
	// never establishes its own condition passes without testing it.
	//
	// compression_enabled, not a count of hypertable_compression_settings:
	// that view carries a row for every hypertable whether or not
	// compression is on, so counting it is a check that cannot fail.
	// Measured after a mutation that stopped the fixture compressing
	// anything survived this assertion.
	var enabled int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM timescaledb_information.hypertables
		 WHERE hypertable_schema = 'public' AND compression_enabled`).
		Scan(&enabled); err != nil {
		t.Fatalf("reading back which hypertables are compressed: %v", err)
	}
	if enabled != compressed {
		t.Fatalf("compression is on for %d hypertables, turned it on for %d.\n"+
			"Applying the schema below would then say nothing about a "+
			"compressed deployment", enabled, compressed)
	}

	// The real path, not a loop of our own over the same slice: apply is
	// what the panel's button runs, with its lock timeout, its advisory
	// lock and its version row.
	a := &Applier{Pool: pool, Name: "compressed-upgrade-test"}
	reached, err := a.apply(ctx)
	if err != nil {
		t.Fatalf("the schema does not apply to a compressed deployment: %v\n\n"+
			"This is the panel's Health -> Schema upgrade button on every customer "+
			"whose TimescaleDB can compress, from their second upgrade onwards.\n"+
			"TimescaleDB refuses ALTER COLUMN, ALTER COLUMN TYPE, ADD CONSTRAINT CHECK "+
			"and ENABLE ROW LEVEL SECURITY on a compressed hypertable. If the statement "+
			"named above is one of those, guard it so it runs only when it would change "+
			"something - see the DO block in internal/storage/schema.sql", err)
	}
	if reached == nil || *reached != schemaver.Version {
		t.Errorf("reached version %v, want %d", reached, schemaver.Version)
	}
}

// compressEveryHypertable puts the database into the state a running
// deployment is in, and answers how many tables that took.
//
// Derived from TimescaleDB's catalogue rather than from a list here: the
// point of this fixture is to cover the hypertables the schema actually
// creates, including ones added after this was written.
//
// site_id is named as the segment where the table has one, because that
// is what ca_set_compression chooses and a segmented table is the state
// deployments are really in. Where a future hypertable has no site_id,
// plain compression is enough - the refusals this test is about are
// about compression being on, not about how it is arranged.
func compressEveryHypertable(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	ctx := context.Background()

	rows, err := pool.Query(ctx, `
		SELECT hypertable_name FROM timescaledb_information.hypertables
		 WHERE hypertable_schema = 'public' ORDER BY hypertable_name`)
	if err != nil {
		t.Fatalf("listing hypertables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scanning a hypertable name: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("listing hypertables: %v", err)
	}

	for _, table := range tables {
		var hasSiteID bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute
			                WHERE attrelid = $1::regclass AND attname = 'site_id'
			                  AND NOT attisdropped)`, table).Scan(&hasSiteID); err != nil {
			t.Fatalf("asking whether %s has a site_id: %v", table, err)
		}
		stmt := `ALTER TABLE ` + table + ` SET (timescaledb.compress)`
		if hasSiteID {
			stmt = `ALTER TABLE ` + table + ` SET (timescaledb.compress, ` +
				`timescaledb.compress_segmentby = 'site_id')`
		}
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("turning compression on for %s: %v", table, err)
		}
	}
	return len(tables)
}

// freshDatabase is a database of this test's own, schema applied once.
//
// Its own rather than the shared one, and the reason is the failure that
// found this bug: O1's compression suite left the shared database
// compressed, and every other package that applies a schema went red at
// once, in a run where nothing they own had changed. Turning compression
// on is not a change one suite may make to a database others use.
func freshDatabase(t *testing.T, name string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	admin := testdb.Admin(t)

	dsn := os.Getenv("CA_SUPERUSER_DSN")
	if dsn == "" {
		t.Skip("set CA_SUPERUSER_DSN; this test creates a database of its own")
	}

	// FORCE on both, not only the cleanup: a scratch database left by a
	// run that died makes the next run fail in its fixture, before the
	// test it belongs to has asserted anything.
	for _, sql := range []string{
		`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`,
		`CREATE DATABASE ` + name,
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(),
			`DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`); err != nil {
			t.Errorf("dropping %s: %v", name, err)
		}
	})

	pool, err := pgxpool.New(ctx, swapDatabase(dsn, name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS timescaledb`); err != nil {
		t.Fatalf("timescaledb in %s: %v", name, err)
	}
	// The install, before anything is compressed - which is the order a
	// real deployment does it in, and the reason the bug survived: the
	// first apply always works.
	for _, f := range schemafiles.InOrder {
		if _, err := pool.Exec(ctx, f.SQL); err != nil {
			t.Fatalf("applying %s to %s: %v", f.Path, name, err)
		}
	}
	return pool
}

// swapDatabase points a DSN at another database on the same server.
//
// Built from the one connection string rather than asking for a second
// environment variable: an operator who set one and not the other would
// get a test that compressed the live database.
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
