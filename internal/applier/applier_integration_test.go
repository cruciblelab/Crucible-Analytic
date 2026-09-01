//go:build integration

// The applier against a real database, as the role that owns it.
//
// This is the one component in the deployment that runs DDL, so the
// things worth asserting are the ones that would let it run the wrong
// DDL, or run it when it should not, or say it ran when it did not.
package applier

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
	"github.com/cruciblelab/crucible-analytic/internal/upgrade"
	"github.com/jackc/pgx/v5/pgxpool"
)

func applierAndPanel(t *testing.T) (*Applier, *pgxpool.Pool) {
	t.Helper()
	adminPool := testdb.Pool(t, testdb.SchemaAdmin)
	panelPool := testdb.Pool(t, testdb.Panel)
	testdb.Lock(t, adminPool, testdb.UpgradeQueueLock)
	// And the version row, which applying writes and which another
	// package's suite sets to four different states of its own. Second,
	// always - see testdb.SchemaVersionLock on why the order is fixed.
	testdb.Lock(t, adminPool, testdb.SchemaVersionLock)

	clean := func() {
		if _, err := testdb.Admin(t).Exec(context.Background(),
			`DELETE FROM panel_upgrade_requests`); err != nil {
			t.Fatalf("clearing the queue: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)

	return &Applier{Pool: adminPool, Name: "test-applier"}, panelPool
}

func askFor(t *testing.T, panelPool *pgxpool.Pool, fingerprint string) *upgrade.Request {
	t.Helper()
	r, err := upgrade.Ask(context.Background(), panelPool,
		upgrade.Actor{Kind: "user", Label: "test@example.invalid"},
		"op-test", 1, schemaver.Version, fingerprint)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	return r
}

// TestNothingWaitingIsNotAFailure.
//
// What almost every run reports. A timer that logged an error each time
// it found nothing would produce a log nobody reads, in the component
// whose log matters on the one day something goes wrong.
func TestNothingWaitingIsNotAFailure(t *testing.T) {
	a, _ := applierAndPanel(t)

	_, err := a.RunOnce(context.Background())
	if err != upgrade.ErrNothingToDo {
		t.Errorf("RunOnce on an empty queue returned %v, want ErrNothingToDo", err)
	}
}

// applyOnce runs the applier and retries while it reports ErrBusy.
//
// The production timer does exactly this, every 30 seconds. A test that
// gave up on the first busy table would be asserting something the
// deployment does not do - and it would be asserting it against a
// database under load no deployment sees, because `go test ./...` runs
// this suite beside internal/panel and internal/panel/web, both of which
// write these tables continuously for half a minute.
//
// Bounded rather than endless: a lock that is never free is a real
// finding, and a test that waits for ever reports it as a hung machine.
func applyOnce(t *testing.T, a *Applier) *upgrade.Request {
	t.Helper()
	ctx := context.Background()

	deadline := time.Now().Add(30 * time.Second)
	for attempt := 1; ; attempt++ {
		req, err := a.RunOnce(ctx)
		if !errors.Is(err, ErrBusy) {
			if err != nil {
				t.Fatalf("the upgrade failed on attempt %d: %v", attempt, err)
			}
			return req
		}
		if time.Now().After(deadline) {
			t.Fatalf("the tables stayed busy for 30s across %d attempts (%v).\n"+
				"The applier gives way on purpose, but something holding these "+
				"locks continuously is worth looking at rather than waiting out",
				attempt, err)
		}
		// Nothing to re-ask for: Requeue put the row back to pending, so
		// the next Claim takes the same request.
		t.Logf("attempt %d: busy, retrying", attempt)
		time.Sleep(100 * time.Millisecond)
	}
}

// TestARequestIsAppliedAndRecorded.
//
// The positive path, and the one a test suite full of refusals would
// leave unproven. Proving something refuses does not prove it works.
func TestARequestIsAppliedAndRecorded(t *testing.T) {
	a, panelPool := applierAndPanel(t)
	ctx := context.Background()

	req := askFor(t, panelPool, schemaver.Fingerprint)

	done := applyOnce(t, a)
	if done.ID != req.ID {
		t.Errorf("applied request %d, asked for %d", done.ID, req.ID)
	}

	// The row says what happened, read back through the panel's own role
	// - which is how the page will read it.
	latest, err := upgrade.Latest(ctx, panelPool)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != upgrade.StateSucceeded {
		t.Errorf("state = %q, want succeeded (error_chain: %q)", latest.State, latest.ErrorChain)
	}
	if latest.FinishedAt == nil {
		t.Error("the request was never closed; an upgrade with no end is the interesting case")
	}
	if latest.AppliedVersion == nil || *latest.AppliedVersion != schemaver.Version {
		t.Errorf("applied_version = %v, want %d", latest.AppliedVersion, schemaver.Version)
	}
	if latest.ClaimedBy != "test-applier" {
		t.Errorf("claimed_by = %q; a two-host deployment could not tell which one ran it", latest.ClaimedBy)
	}

	// And the database itself agrees, which is the fact the row is a
	// claim about.
	state, err := schemaver.Read(ctx, panelPool)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Matches() {
		t.Errorf("the request says succeeded and the database reports version %d / %s; "+
			"the row is a claim about the schema and it is not true",
			state.Version, state.Fingerprint)
	}
}

// TestApplyingTwiceIsSafe.
//
// Every schema file in this repository is written to be re-runnable - IF
// NOT EXISTS on every CREATE, DROP POLICY before every CREATE POLICY -
// and that property is what makes a failed upgrade retryable. It is a
// property of the SQL rather than of the applier, so it is asserted
// against the SQL by running the whole thing again.
//
// Without it the honest advice after a failure would be "restore a
// backup", which is not something a customer with no shell can do.
func TestApplyingTwiceIsSafe(t *testing.T) {
	a, panelPool := applierAndPanel(t)

	askFor(t, panelPool, schemaver.Fingerprint)
	applyOnce(t, a)

	// The claim: the same schema, applied again, over a database that
	// already has it. A schema that cannot be applied twice cannot be
	// retried after a partial failure, and retrying is the only recovery
	// a customer with no shell has.
	askFor(t, panelPool, schemaver.Fingerprint)
	applyOnce(t, a)
}

// TestAnApplierRefusesASchemaItDoesNotCarry.
//
// The check the whole arrangement rests on. A deployment can end up with
// a new panel and an old upgrader - packages upgraded one at a time, a
// unit not restarted - and the old one must refuse rather than migrate
// the database to a shape nobody asked for.
//
// Refusing is recoverable in one command. Applying the wrong schema is
// not.
func TestAnApplierRefusesASchemaItDoesNotCarry(t *testing.T) {
	a, panelPool := applierAndPanel(t)
	ctx := context.Background()

	askFor(t, panelPool, "0000000000000000000000000000000000000000000000000000000000000000")

	_, err := a.RunOnce(ctx)
	if err == nil {
		t.Fatal("the applier ran a migration for a fingerprint it does not carry")
	}
	if !strings.Contains(err.Error(), "does not carry") {
		t.Errorf("it refused, but not for the reason being tested: %v", err)
	}

	// And it is recorded as failed with the reason, not left running:
	// the operator's fix is to upgrade this component, and the row is
	// the only place they can find that out.
	latest, err := upgrade.Latest(ctx, panelPool)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != upgrade.StateFailed {
		t.Errorf("state = %q, want failed — a refused request left running blocks "+
			"the queue behind the one-in-flight index", latest.State)
	}
	if !strings.Contains(latest.ErrorChain, "this binary carries") {
		t.Errorf("the row does not say which schema this binary has: %q", latest.ErrorChain)
	}
	if latest.AppliedVersion != nil {
		t.Errorf("applied_version = %v on a request that was refused before any DDL ran",
			*latest.AppliedVersion)
	}
}
