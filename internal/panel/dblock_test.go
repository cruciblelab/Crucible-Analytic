//go:build integration

package panel

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// panelDatabaseLock serialises the suites that share the panel schema.
//
// internal/panel and internal/panel/web both run against the same
// database, and `go test ./...` runs packages in parallel. Most of what
// they touch is namespaced well enough to coexist, but two things are
// not, because they are global by nature:
//
//   - "this deployment has no accounts", which is the entire subject of
//     the first-run page. This suite creates users; the web suite asserts
//     none exist. Checking the count and then asserting is a gap another
//     package can write into, and it did - intermittently, which is the
//     worst way for a test to fail.
//   - a dev-access link auto-approves only while nobody owns the
//     deployment, so the same account count decides the rest of the flow.
//
// A guard clause cannot close that gap: the condition has to hold for
// the length of the test, not just at the moment it is read. So the two
// suites take a Postgres advisory lock instead and take turns.
//
// The number is arbitrary but must match on both sides; it is written
// out in hex on purpose so a stray copy is recognisable.
const panelDatabaseLock = 0x6372756369626c65 // "crucible"

// This copy is duplicated rather than shared because the two packages
// have no test-only package between them, and a real package existing
// only to hold a test helper would be worse than sixty duplicated lines.
// The constant is what has to agree, and a test on each side asserts it.
//
// lockPanelDatabase holds the lock until the test ends.
//
// The connection is pinned with Acquire rather than borrowed per query.
// Advisory locks belong to a session, and a pool hands out whichever
// connection is free - so locking on one connection and unlocking on
// another silently leaks the lock and deadlocks the next run.
func lockPanelDatabase(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring a connection for the suite lock: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(panelDatabaseLock)); err != nil {
		conn.Release()
		t.Fatalf("taking the suite lock: %v", err)
	}
	t.Cleanup(func() {
		// Unlock before release, on the same connection. Releasing first
		// would return a connection that still holds the lock to the
		// pool, where it would be handed to somebody else still holding
		// it.
		if _, err := conn.Exec(context.Background(),
			`SELECT pg_advisory_unlock($1)`, int64(panelDatabaseLock)); err != nil {
			t.Logf("releasing the suite lock: %v", err)
		}
		conn.Release()
	})
}

// TestSuiteLockConstantMatches guards the one thing the duplication can
// get wrong.
//
// Two copies of this helper exist, one per package, because there is no
// shared test package between them. Everything in them is inert except
// the constant: if the two numbers drift apart the locks stop excluding
// each other, both suites go green, and the race comes back invisibly.
// So the value is asserted here against a literal rather than read from
// the other package - which cannot be imported from a _test file anyway.
func TestSuiteLockConstantMatches(t *testing.T) {
	const agreed = 0x6372756369626c65
	if panelDatabaseLock != agreed {
		t.Fatalf("panelDatabaseLock = %#x, want %#x; internal/panel's copy uses the agreed "+
			"value and the two locks only exclude each other while they match",
			int64(panelDatabaseLock), int64(agreed))
	}
}
