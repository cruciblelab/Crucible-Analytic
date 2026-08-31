//go:build integration

package upgrade

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// upgradeQueueLock serialises the two suites that share the upgrade
// queue.
//
// internal/upgrade and internal/applier both run against
// panel_upgrade_requests, and `go test ./...` runs packages in parallel.
// Scoping each suite's rows would not be enough here, because the thing
// they collide on is global by construction: idx_upgrade_one_in_flight
// permits exactly one pending-or-running row in the whole table, so a
// request inserted by either suite makes the other's Ask fail with
// ErrAlreadyInFlight - and a Claim from either can take the other's row.
//
// That is the index doing its job. The suites take turns instead.
//
// Measured rather than predicted: before this, running the two packages
// together failed with "no request is waiting" inside a test that had
// just inserted one, while each package passed alone. The same shape
// internal/panel and internal/panel/web already found, for the same
// reason, and this follows their solution.
//
// The number differs from the panel suites' lock on purpose: these two
// need to exclude each other, not the panel, and sharing one number
// would serialise four packages where two is enough.
const upgradeQueueLock = 0x75706772616465FF // "upgrade"

// lockUpgradeQueue holds the lock until the test ends.
//
// The connection is pinned with Acquire rather than borrowed per query.
// Advisory locks belong to a session, and a pool hands out whichever
// connection is free - so locking on one connection and unlocking on
// another silently leaks the lock and deadlocks the next run.
func lockUpgradeQueue(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquiring a connection for the suite lock: %v", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(upgradeQueueLock)); err != nil {
		conn.Release()
		t.Fatalf("taking the suite lock: %v", err)
	}
	t.Cleanup(func() {
		// Unlock before release, on the same connection. Releasing first
		// would return a connection that still holds the lock to the
		// pool, where it would be handed to somebody else still holding
		// it.
		if _, err := conn.Exec(context.Background(),
			`SELECT pg_advisory_unlock($1)`, int64(upgradeQueueLock)); err != nil {
			t.Logf("releasing the suite lock: %v", err)
		}
		conn.Release()
	})
}

// TestUpgradeLockConstantMatches guards the one thing the duplication
// can get wrong.
//
// Two copies of this helper exist, one per package, because there is no
// shared test package between them. Everything in them is inert except
// the constant: if the two numbers drift apart the locks stop excluding
// each other, both suites go green, and the race comes back invisibly.
func TestUpgradeLockConstantMatches(t *testing.T) {
	const agreed = 0x75706772616465FF
	if upgradeQueueLock != agreed {
		t.Fatalf("upgradeQueueLock is %#x here and the other suite uses %#x; "+
			"while they differ the two packages no longer exclude each other",
			upgradeQueueLock, agreed)
	}
}
