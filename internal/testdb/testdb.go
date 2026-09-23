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
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/dblock"
	"github.com/cruciblelab/crucible-analytic/internal/resources"
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
	pool, err := resources.Open(context.Background(), DSN(role))
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
	pool, err := resources.Open(context.Background(), dsn)
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
	// The rollup tables are on this list, and leaving them off was the
	// first thing O2 got wrong.
	//
	// traffic_rollup holds the same history as traffic_snapshots in
	// pre-aggregated form, so a row this helper does not delete is a row
	// the summary still counts - traffic in a range whose detail rows are
	// gone. In a test that shows up as a number nobody can explain; in a
	// deployment it is the same fault, which is why the rollup is pruned
	// by the retention age as well.
	//
	// Named rather than derived, like the two above. The derived version
	// would be "every table with a site_id column", which reaches the
	// panel's membership and settings tables - rows a CleanSite call has
	// no business removing.
	wipe := func() {
		for _, table := range []string{
			"traffic_snapshots", "beacon_events", "traffic_rollup", "traffic_rollup_state",
		} {
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
//
// And this one before AccountsLock: internal/panel/web's schema tests
// hold both, and write the row before they build the server that takes
// AccountsLock.
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

// RefreshQueueLock serialises the suites that share
// ip_range_refresh_requests.
//
// A fourth lock, and this table needs one more than the others: a
// partial unique index permits exactly one pending-or-running row in the
// whole table, so a request inserted by any suite makes another's Ask
// fail with ErrAlreadyInFlight, and a Claim from either can take a row
// it did not write. Naming each suite's rows would not help - what they
// collide on is global by construction.
//
// The same shape as UpgradeQueueLock, for the same index, one table
// over. Four packages touch this one: internal/rangerefresh,
// internal/asnlookup, internal/panel and internal/panel/web.
//
// # Ordering
//
// Nothing takes this together with the others today. If something ever
// does, take them in the order they are declared here.
const RefreshQueueLock = 0x726566726573680A // "refresh"

// ReleaseQueueLock serialises the suites that share
// panel_release_requests.
//
// A fifth lock, for the third queue with a one-in-flight index. The
// reason is identical to the two above and worth repeating rather than
// cross-referencing: the index permits exactly one pending-or-running
// row in the whole table, so any suite's request makes another's Ask
// fail with ErrAlreadyInFlight, and a Claim from either can take a row
// it did not write. Naming each suite's rows would not help - what they
// collide on is global by construction.
//
// # Ordering
//
// Nothing takes this together with the others today. If something ever
// does, take them in the order they are declared here.
const ReleaseQueueLock = 0x72656C65617365FF // "release"

// BackupQueueLock serialises the suites that share
// panel_backup_requests.
//
// The fourth queue with a one-in-flight index, and the same collision:
// any suite's pending request makes another's Ask fail with
// ErrAlreadyInFlight.
//
// It also guards the catalogue. panel_backups has no in-flight index -
// there can be many backups - but two suites listing it while a third
// inserts would each see rows they did not write, and the tests here
// count them.
//
// # Ordering
//
// Nothing takes this together with the others today. If something ever
// does, take them in the order they are declared here.
const BackupQueueLock = 0x796564656B00FF01 // "yedek"

// AccountsLock serialises the suites that write panel_users.
//
// # What is global here
//
// Not a row and not a queue: an *emptiness*. Two of this product's
// behaviours are decided by whether the deployment has any account at
// all, and both are load-bearing.
//
//   - The first-run page exists only while nobody owns the deployment.
//   - A developer access link auto-approves only in that same window,
//     because before an account exists there is nobody to ask.
//
// The second is decided inside the INSERT, by a
// `NOT EXISTS (SELECT 1 FROM panel_users)` subquery, precisely so that
// an account created in the same millisecond cannot land between a
// check and a write. That closes the product's race. It does nothing
// for a *suite* that empties the table and then asserts against the
// emptiness: another package's INSERT lands in between, and the failure
// surfaces in whichever suite happened to be reading.
//
// # Why this constant moved here
//
// It did not start here. internal/panel and internal/panel/web each
// carried a copy, with a test on each side asserting the two numbers
// matched, and a comment saying a shared package for one test helper
// would be worse than the duplication.
//
// That reasoning was sound when it was written and had quietly expired:
// this package now holds six such keys, so the shared place exists. And
// what the duplication could not survive was not drift between the two
// copies - it was a *third* writer. internal/backup's restore suite
// began inserting accounts on the shared database while a backup ran,
// took no lock, and neither copy of the constant had anything to say
// about it.
//
// Measured, on 2026-09-07:
//
//	--- FAIL: TestStore_RealDB_BootstrapLinkDiesWhenAnAccountAppears
//	    expected an auto-approved request, got {... AutoApproved:false}
//
// A test that had passed for weeks, failing on main, naming neither the
// package that wrote the row nor the reason.
//
// # Ordering
//
// Nothing takes this together with the others today. If something ever
// does, take them in the order they are declared here.
const AccountsLock = 0x6372756369626c65 // "crucible"

// SchemaApplyLock serialises anything that applies a schema file.
//
// Unlike the four above, this one is not a test fixture. The applier
// takes it in production for the reason written out in
// internal/dblock, and this is the same key so that a suite applying a
// file by hand cannot land in the middle of an applier doing the same -
// which is not a tidiness problem: it produced
// "tuple concurrently updated" and "deadlock detected" in a plain
// `go test -tags integration ./...`, on the second run, in a package
// that had nothing to do with either.
//
// Re-exported rather than redeclared so there is one number. A second
// copy of a lock key is a lock that does not lock, and it would look
// correct in both places.
//
// # Ordering
//
// Take this one last. internal/asnlookup's upgrade-path test holds
// FetchLogLock and then this; anything needing both must use that order,
// since two suites taking one pair in opposite orders deadlock and a
// deadlocked suite looks like a hung machine rather than like a bug.
const SchemaApplyLock = dblock.SchemaApply

// SchemaRaceLock keeps everyone out while one test races appliers
// against each other on purpose.
//
// A sixth lock, and the reason it cannot be the one above: that key is
// the mechanism under test. internal/applier's concurrency test measures
// what three appliers do when they meet inside the schema, so it must
// not hold SchemaApplyLock - the appliers take it themselves, and a test
// holding it would make all three give way to the test rather than to
// each other.
//
// That leaves it defenceless against a suite that *is* holding
// SchemaApplyLock, and the failure was measured rather than imagined:
// internal/asnlookup's upgrade-path test holds that key for its whole
// duration, `go test ./...` runs packages in parallel, and the racers
// then time out against it instead of against one another. The test
// reported "8 applied, 16 gave way" - which is precisely the signature
// of the lock not being taken at all, so a false red and a true red
// were the same red.
//
// So the two meanings get two keys. This one means "no other suite may
// be applying a schema right now"; SchemaApplyLock keeps its production
// meaning and nothing else. Anything that applies a schema file by hand
// takes both.
//
// # Ordering
//
// **Before** SchemaApplyLock, and that order is the whole of what makes
// the pair work. Taken the other way round - which is how it was first
// written, and how it reached CI - internal/asnlookup grabs
// SchemaApplyLock and then blocks on this one, so it is *holding* the
// key the racers need while it waits for the test that has it. Every
// one of the racers then times out: measured as "0 applied, 24 waited
// for the schema lock", on a run whose only fault was the order these
// two comments described.
//
// So: this one outermost. internal/asnlookup takes FetchLogLock, then
// this, then SchemaApplyLock.
const SchemaRaceLock = 0x736368656D617263 // "schemarc"

// IPModeSettingLock serialises the suites that write the deployment-wide
// privacy.ip_storage row.
//
// A seventh lock, and the one setting row that needed one. Two suites
// write it: internal/beacon measures that the disclosure's date comes
// from that row rather than a neighbouring key, and internal/storage
// measures that the mode a panel stores changes what the collector
// writes. Both need to see their own value in a global row, both run
// against one database, and `go test` runs packages in parallel - so
// without this each is capable of reading the other's write and
// reporting a product defect, in whichever package lost the race.
//
// Scoped to the row rather than to settings in general: every other key
// these suites touch is written by exactly one of them, and a lock that
// covered all of settings would serialise suites that never collide.
//
// # Ordering
//
// Nothing takes this together with the others. If something ever does,
// take it after AccountsLock and before SchemaRaceLock - it guards a
// row, so it belongs inside the locks that guard whole schemas.
const IPModeSettingLock = 0x69706d6f64650001 // "ipmode" + 1

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
//
// # Not re-entrant, and it says so
//
// Each call takes its own connection, so a test that already holds key -
// or whose parent does - waits on itself: the lock is released when the
// test ends, and the test cannot end. `go test` reports that ten minutes
// later as a panic in whichever test was running, which looks like a
// hung machine. So a nested call fails at once and names the holder.
//
// It became a real risk when the helpers that write the schema row
// started taking SchemaVersionLock themselves: a test that also took it
// at the top, as TestTheHealthPageReportsTheSchemaVersion used to, would
// hang on its first case.
func Lock(t *testing.T, pool *pgxpool.Pool, key int64) {
	t.Helper()
	ctx := context.Background()

	if holder, nested := heldBy(t.Name(), key); nested {
		t.Fatalf("%s asked for suite lock %#x, and %s already holds it in this test. "+
			"testdb.Lock is not re-entrant: the second call would wait for a release "+
			"that happens only when this test ends. Take it once, where the test starts.",
			t.Name(), key, holder)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring a connection for the suite lock: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		conn.Release()
		t.Fatalf("taking the suite lock: %v", err)
	}
	holding(t.Name(), key)
	t.Cleanup(func() {
		released(t.Name(), key)
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

// held records which test in this process holds which suite lock.
//
// Only for the nesting check above. Two unrelated tests in one process -
// parallel ones - still wait on each other in Postgres, which is the
// point of the lock; only a test and its own ancestors cannot.
var held = struct {
	sync.Mutex
	by map[int64]string
}{by: map[int64]string{}}

// heldBy reports whether key is held by name itself or by an ancestor of
// it (subtests are named "parent/child").
func heldBy(name string, key int64) (string, bool) {
	held.Lock()
	defer held.Unlock()
	holder, ok := held.by[key]
	if !ok {
		return "", false
	}
	return holder, holder == name || strings.HasPrefix(name, holder+"/")
}

func holding(name string, key int64) {
	held.Lock()
	defer held.Unlock()
	held.by[key] = name
}

// released forgets name's hold, and only name's: by the time a cleanup
// runs, another test may already have taken the lock and recorded
// itself.
func released(name string, key int64) {
	held.Lock()
	defer held.Unlock()
	if held.by[key] == name {
		delete(held.by, key)
	}
}
