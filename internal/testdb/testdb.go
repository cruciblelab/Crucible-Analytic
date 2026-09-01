//go:build integration

// Package testdb hands an integration suite the connection its
// production code actually uses.
//
// # Why it exists
//
// Every integration suite in this project used to open one pool, as
// `collector`, against a development database that `collector` had
// created. That role therefore owned every table, and PostgreSQL grants
// an owner everything - so ten suites ran with authority no deployment
// gives any service, while describing themselves as tests of a
// role-separated design.
//
// What that hid, found in one afternoon by installing the package and
// running it:
//
//   - The retention feature had never worked on an installed
//     deployment. TimescaleDB checks hypertable *ownership*, and no
//     service owns one. Both tables grew forever.
//   - ip_asn_ranges and ip_country_ranges had no grants at all, so ASN
//     and country enrichment was silently off everywhere.
//   - The setup wizard reported the analytics tables as missing on
//     every correct install, because information_schema hides tables
//     the current role has no privilege on - and a failed required
//     check blocks handover, so the deployment could never be given to
//     its owner.
//
// None of the three is subtle. All three were invisible from a suite
// whose fixture was more privileged than production.
//
// # The rule
//
// A test uses the role its code runs as. Setting up and tearing down
// are a different job - they are what the installer and the schema
// owner do - and they get Admin, which skips rather than pretends when
// no superuser connection is offered.
package testdb

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DSN is where role connects.
//
// Overridable per role through the environment, because the passwords
// below are the development cluster's convention and nothing else.
func DSN(role string) string {
	if dsn := os.Getenv("CA_DSN_" + role); dsn != "" {
		return dsn
	}
	return fmt.Sprintf("postgres://%s:%s@localhost:5432/analytics", role, role)
}

// Pool opens a connection as one service role, closed when the test ends.
func Pool(t *testing.T, role string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), DSN(role))
	if err != nil {
		t.Fatalf("pgxpool.New as %s: %v", role, err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("ping as %s: %v\n"+
			"Is the database up and installed? release/install.sh creates the four roles "+
			"and applies the grants; the suites expect each role's password to be its own name.",
			role, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Admin is a connection that owns the schema.
//
// For the things no service may do and none should be able to: DDL, and
// removing rows from the analytics tables. Neither writer holds DELETE -
// see internal/retention/schema.sql for why that is deliberate - so a
// suite that seeds rows needs this to clear them again.
//
// Skips when unset rather than failing. A machine that can run a suite
// as its service roles may have no superuser connection to offer, and a
// test that cannot run is not a test that failed.
func Admin(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CA_SUPERUSER_DSN")
	if dsn == "" {
		t.Skip("set CA_SUPERUSER_DSN to a connection that owns the schema; this test writes rows only its owner can remove")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New (superuser): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// CleanSite removes one site's analytics rows, now and when the test
// ends.
//
// Both ends, because a suite that only cleans up afterwards inherits
// whatever a crashed previous run left behind - and a row from last time
// inside this run's window is a number nobody can explain.
func CleanSite(t *testing.T, admin *pgxpool.Pool, sites ...string) {
	t.Helper()
	wipe := func() {
		for _, table := range []string{"traffic_snapshots", "beacon_events"} {
			if _, err := admin.Exec(context.Background(),
				"DELETE FROM "+table+" WHERE site_id = ANY($1)", sites); err != nil {
				t.Logf("clearing %s for %v: %v", table, sites, err)
			}
		}
	}
	wipe()
	t.Cleanup(wipe)
}

// UpgradeQueueLock serialises the suites that share panel_upgrade_requests.
//
// internal/upgrade, internal/applier and internal/panel all write to that
// table, and `go test ./...` runs packages in parallel. Naming each
// suite's rows would not be enough: what they collide on is global by
// construction. idx_upgrade_one_in_flight permits exactly one
// pending-or-running row in the whole table, so a request inserted by any
// suite makes another's Ask fail with ErrAlreadyInFlight, and a Claim from
// either can take a row it did not write.
//
// That is the index doing its job. The suites take turns instead.
//
// Measured rather than predicted: running the packages together failed
// with "no request is waiting" inside a test that had just inserted one,
// while each passed alone.
//
// The number is arbitrary but must be the same everywhere; it is written
// out in hex so a stray copy is recognisable.
const UpgradeQueueLock = 0x75706772616465FF // "upgrade"

// SchemaVersionLock serialises the suites that write the schema_version
// row.
//
// A second lock rather than a wider use of the first, because they cover
// different things and one of them is not about the queue at all.
// schema_version is a single row - id = 1, by a CHECK constraint - which
// says what shape the database is in. internal/panel/web sets it to four
// different states to check what the health page says about each;
// internal/applier applies the schema and records the result, which
// overwrites it.
//
// Measured, and it took a while to see because the failure named neither
// package: the health page test reported "the page says beklediğiyle
// aynı, which belongs to another state", passed when run alone, and
// failed when the whole suite ran. The applier had rewritten the row
// underneath it, from another process, mid-assertion.
//
// The race predates the test that exposed it - the applier's own suite
// has always recorded a version - and stayed invisible while the window
// was a few milliseconds wide. A test that ran a second and a half of
// concurrent load made it certain instead of unlikely, which is the only
// reason it was found.
//
// # Ordering
//
// internal/applier takes UpgradeQueueLock first and then this one.
// Anything that needs both must do the same: two suites taking the same
// pair in opposite orders deadlock, and a deadlocked test suite looks
// like a hung machine rather than like a bug.
const SchemaVersionLock = 0x736368656D617601 // "schema" + 1

// FetchLogLock serialises the suites that share ip_range_fetches.
//
// A third lock, and the reason is the same as the other two: two
// packages touch one table and `go test ./...` runs them in parallel.
// internal/asnlookup writes rows and, in one test, revokes and re-grants
// privileges on the table to prove the upgrade path carries them;
// internal/panel reads it as panel_user. A read that lands inside that
// revoke fails with "permission denied", which reads as a broken grant
// rather than as two tests overlapping.
//
// # Ordering
//
// Nothing takes this together with the other two today. If something
// ever does, take them in the order they are declared here - two suites
// taking the same pair in opposite orders deadlock, and a deadlocked
// suite looks like a hung machine rather than like a bug.
const FetchLogLock = 0x66657463686C6F67 // "fetchlog"

// Lock holds a Postgres advisory lock until the test ends.
//
// Here rather than duplicated per package, which is where it started.
// internal/panel and internal/panel/web already carry a copy each of an
// older helper of this shape, with a comment explaining that they had no
// shared test package between them - and a test whose only job is to
// assert that the two copies' constants still agree. This package is that
// shared place; a third copy would have meant a third constant to keep in
// step.
//
// The connection is pinned with Acquire rather than borrowed per query.
// Advisory locks belong to a session, and a pool hands out whichever
// connection is free - so locking on one connection and unlocking on
// another silently leaks the lock and deadlocks the next run.
func Lock(t *testing.T, pool *pgxpool.Pool, key int64) {
	t.Helper()
	ctx := context.Background()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring a connection for the suite lock: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		conn.Release()
		t.Fatalf("taking the suite lock: %v", err)
	}
	t.Cleanup(func() {
		// Unlock before release, on the same connection. Releasing first
		// would return a connection that still holds the lock to the
		// pool, where it would be handed to somebody else still holding
		// it.
		if _, err := conn.Exec(context.Background(),
			`SELECT pg_advisory_unlock($1)`, key); err != nil {
			t.Logf("releasing the suite lock: %v", err)
		}
		conn.Release()
	})
}
