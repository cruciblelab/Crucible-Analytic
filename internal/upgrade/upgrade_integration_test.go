//go:build integration

// The queue against a real database, as the two roles that use it.
//
// Every test here connects as panel_user or schema_admin rather than as
// a superuser, because the property being tested *is* the privilege
// split: the panel may ask and may not answer, the applier may answer
// and may not ask. A superuser satisfies both halves and would prove
// neither.
package upgrade

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

func queuePools(t *testing.T) (panelPool, adminPool *pgxpool.Pool) {
	t.Helper()
	panelPool = testdb.Pool(t, testdb.Panel)
	adminPool = testdb.Pool(t, testdb.SchemaAdmin)
	testdb.Lock(t, adminPool, testdb.UpgradeQueueLock)

	clean := func() {
		if _, err := testdb.Admin(t).Exec(context.Background(),
			`DELETE FROM panel_upgrade_requests`); err != nil {
			t.Fatalf("clearing the queue: %v — a cleanup that does not run leaves "+
				"the next test blocked by this one's in-flight row", err)
		}
	}
	clean()
	t.Cleanup(clean)
	return panelPool, adminPool
}

func ask(t *testing.T, pool *pgxpool.Pool) *Request {
	t.Helper()
	r, err := Ask(context.Background(), pool,
		Actor{Kind: "user", Label: "test@example.invalid"}, "op-test", 1, 2, "parmakizi")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	return r
}

// TestThePanelCanAskAndCannotAnswer.
//
// The split the whole design rests on. A compromised panel process may
// ask for an upgrade - that is a button any signed-in customer can press
// anyway - and must not be able to fabricate the result of one, because
// a request marked succeeded is a claim that the database is at a
// version it is not.
func TestThePanelCanAskAndCannotAnswer(t *testing.T) {
	panelPool, _ := queuePools(t)
	ctx := context.Background()

	req := ask(t, panelPool)
	if req.State != StatePending {
		t.Errorf("a new request is %q, want pending", req.State)
	}

	// And now the half that must be refused.
	_, err := panelPool.Exec(ctx,
		`UPDATE panel_upgrade_requests SET state = 'succeeded' WHERE id = $1`, req.ID)
	if err == nil {
		t.Fatal("the panel marked its own request succeeded. " +
			"A process that can both ask for an upgrade and declare it done can " +
			"claim the database is at a version it is not.")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("the update was refused, but not by the privilege split: %v", err)
	}
}

// TestTheApplierCanAnswerAndCannotAsk.
//
// The mirror of the above, and not symmetry for its own sake: an applier
// that could insert its own requests would be a component that upgrades
// the database because it feels like it, with no customer and no audit
// entry behind the decision.
func TestTheApplierCanAnswerAndCannotAsk(t *testing.T) {
	panelPool, adminPool := queuePools(t)
	ctx := context.Background()

	req := ask(t, panelPool)

	claimed, err := Claim(ctx, adminPool, "test-applier")
	if err != nil {
		t.Fatalf("the applier could not claim a pending request: %v", err)
	}
	if claimed.ID != req.ID || claimed.State != StateRunning {
		t.Errorf("claimed %d in state %q, want %d running", claimed.ID, claimed.State, req.ID)
	}

	// Finished first, so the in-flight index is not what refuses the
	// insert below.
	//
	// This test passed for years without that line, and it was passing
	// for the wrong reason: it asked while the claimed request was still
	// running, the unique index refused it, and the assertion only
	// checked that *something* had. Measured on a real database,
	// schema_admin could insert here whenever the queue was empty -
	// every table is owned by schema_admin, and an owner is exempt from
	// row-level security unless the table FORCEs it. The schema now
	// does; this is the test that would have said so.
	applied := 2
	if err := Finish(ctx, adminPool, req.ID, StateSucceeded, &applied, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	_, err = Ask(ctx, adminPool, Actor{Kind: "service", Label: "applier"}, "", 1, 2, "x")
	if err == nil {
		t.Fatal("the applier inserted its own upgrade request; " +
			"an upgrade with no customer behind it has nobody who asked for it")
	}
	if !refusedByPrivilege(err) {
		t.Errorf("the refusal was %v. That is not the privilege check doing it, and "+
			"a test that accepts any error here passes on a database where this "+
			"role can insert whenever the queue happens to be empty", err)
	}
}

// refusedByPrivilege reports whether an error is the database refusing
// on authority rather than on data.
//
// Two spellings, because two mechanisms enforce the same rule and which
// one answers depends on who owns the table. A plain GRANT refusal says
// "permission denied"; row-level security says "violates row-level
// security policy" - and this project needs the second, since every
// table is owned by schema_admin and an owner is exempt from RLS unless
// the table FORCEs it.
//
// What this must never match is a unique-violation. "The queue was busy"
// and "you are not allowed" are different answers, and a test that
// treats them alike is the test this one used to be.
func refusedByPrivilege(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "row-level security")
}

// TestASecondClaimFindsNothing.
//
// Two appliers, or one timer overlapping its previous run. The state is
// part of the UPDATE's WHERE rather than checked first and updated
// after, because between a SELECT and an UPDATE the other process can do
// both.
func TestASecondClaimFindsNothing(t *testing.T) {
	panelPool, adminPool := queuePools(t)
	ctx := context.Background()
	ask(t, panelPool)

	if _, err := Claim(ctx, adminPool, "first"); err != nil {
		t.Fatalf("the first claim failed: %v", err)
	}
	_, err := Claim(ctx, adminPool, "second")
	if err == nil {
		t.Fatal("two appliers both claimed the same request; they would migrate " +
			"the same database at the same time")
	}
	if err != ErrNothingToDo {
		t.Errorf("the second claim failed with %v, want ErrNothingToDo", err)
	}
}

// TestOnlyOneRequestMayBeInFlight.
//
// Two customers pressing the button within a second of each other is the
// ordinary way to get here, and it is not a fault - which is why it has
// its own error and the panel says "one is already going" rather than
// "that failed".
func TestOnlyOneRequestMayBeInFlight(t *testing.T) {
	panelPool, _ := queuePools(t)
	ctx := context.Background()

	first := ask(t, panelPool)

	_, err := Ask(ctx, panelPool, Actor{Kind: "user", Label: "ikinci@example.invalid"},
		"op-iki", 1, 2, "parmakizi")
	if err != ErrAlreadyInFlight {
		t.Fatalf("a second request was accepted while %d was pending (err = %v); "+
			"two migrations would run against one database", first.ID, err)
	}

	// And once the first is finished, the next one goes through - the
	// index counts pending and running, not history.
	if err := Finish(ctx, testdb.Pool(t, testdb.SchemaAdmin), first.ID, StateFailed, nil, "test"); err != nil {
		t.Fatalf("finishing the first: %v", err)
	}
	if _, err := Ask(ctx, panelPool, Actor{Kind: "user", Label: "ucuncu@example.invalid"},
		"op-uc", 1, 2, "parmakizi"); err != nil {
		t.Fatalf("no request was in flight and this was still refused: %v. "+
			"A failed upgrade that cannot be retried is a deployment stuck for ever "+
			"on one bad attempt.", err)
	}
}

// TestAStrandedClaimDoesNotBlockTheQueueForEver.
//
// A process killed mid-migration leaves its row in `running`, and the
// one-in-flight index then refuses every later request. Without this the
// symptom is a button permanently greyed out because of a crash weeks
// ago, and nothing on the page explains why.
func TestAStrandedClaimDoesNotBlockTheQueueForEver(t *testing.T) {
	panelPool, adminPool := queuePools(t)
	ctx := context.Background()

	stranded := ask(t, panelPool)
	if _, err := Claim(ctx, adminPool, "the-one-that-died"); err != nil {
		t.Fatal(err)
	}

	// Nothing is released while the claim is fresh: a migration that is
	// genuinely running must not be declared dead underneath itself.
	released, err := ReleaseStaleClaims(ctx, adminPool, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if released != 0 {
		t.Errorf("a claim %s old was released with a one-hour timeout; a running "+
			"migration would be marked failed while it was still going",
			time.Since(stranded.RequestedAt))
	}

	// And it is released once it is genuinely stale.
	released, err = ReleaseStaleClaims(ctx, adminPool, 0)
	if err != nil {
		t.Fatal(err)
	}
	if released != 1 {
		t.Fatalf("released %d stale claims, want 1", released)
	}

	latest, err := Latest(ctx, panelPool)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != StateFailed {
		t.Errorf("the stranded request is %q, want failed", latest.State)
	}
	if !strings.Contains(latest.ErrorChain, "not known") {
		t.Errorf("the row does not say the outcome is unknown: %q", latest.ErrorChain)
	}

	// The queue is open again.
	if _, err := Ask(ctx, panelPool, Actor{Kind: "user", Label: "sonra@example.invalid"},
		"", 1, 2, "parmakizi"); err != nil {
		t.Errorf("the queue is still blocked after the stale claim was released: %v", err)
	}
}

// TestLatestSaysNothingHasBeenAskedRatherThanFailing.
//
// The state every deployment starts in. A page that showed it as an
// error would tell a customer something is broken on the day they
// installed it.
func TestLatestSaysNothingHasBeenAskedRatherThanFailing(t *testing.T) {
	panelPool, _ := queuePools(t)

	latest, err := Latest(context.Background(), panelPool)
	if err != nil {
		t.Fatalf("Latest on an empty queue returned an error: %v", err)
	}
	if latest != nil {
		t.Errorf("Latest on an empty queue returned %+v, want nil", latest)
	}
}
