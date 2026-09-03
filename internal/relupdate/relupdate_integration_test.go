//go:build integration

// The release update queue against a real database, as the two roles
// that use it.
//
// Every test connects as panel_user or schema_admin rather than as a
// superuser, because the property being tested *is* the privilege split:
// the panel may ask and may not answer, the upgrader may answer and may
// not ask. A superuser satisfies both halves and would prove neither.
package relupdate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

func queuePools(t *testing.T) (panelPool, adminPool *pgxpool.Pool) {
	t.Helper()
	panelPool = testdb.Pool(t, testdb.Panel)
	adminPool = testdb.Pool(t, testdb.SchemaAdmin)
	testdb.Lock(t, adminPool, testdb.ReleaseQueueLock)

	clean := func() {
		if _, err := testdb.Admin(t).Exec(context.Background(),
			`DELETE FROM panel_release_requests`); err != nil {
			t.Fatalf("clearing the queue: %v — a cleanup that does not run leaves "+
				"the next test blocked by this one's in-flight row", err)
		}
	}
	clean()
	t.Cleanup(clean)
	return panelPool, adminPool
}

func actor() Actor { return Actor{Kind: "user", Label: "biri@example.invalid"} }

func ask(t *testing.T, pool *pgxpool.Pool, to string) *Request {
	t.Helper()
	r, err := Ask(context.Background(), pool, actor(), "op-bir", "v0.19.0", to)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	return r
}

// TestThePanelCanAskAndCannotAnswer is the whole design in one test.
//
// The panel writing "succeeded" would mean a compromised panel could
// claim a version is installed that is not - and the page a customer
// reads to find out what is running would be reading the attacker's
// answer. INSERT and UPDATE being held by different roles is what stops
// it, and this checks the database enforces that rather than the code
// remembering to.
func TestThePanelCanAskAndCannotAnswer(t *testing.T) {
	panelPool, _ := queuePools(t)
	ctx := context.Background()

	r := ask(t, panelPool, "v0.20.0")
	if r.State != StatePending {
		t.Errorf("a new request is %q, not pending", r.State)
	}
	if !r.InFlight() {
		t.Error("a pending request does not report itself in flight, so the page " +
			"would offer the button again while one is queued")
	}

	_, err := panelPool.Exec(ctx,
		`UPDATE panel_release_requests SET state = 'succeeded' WHERE id = $1`, r.ID)
	if err == nil {
		t.Fatal("panel_user updated the queue. A compromised panel could then report " +
			"a version as installed without anything having been installed, and the " +
			"health page would repeat it")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("the refusal was %v, which is not the privilege check doing it", err)
	}
}

// TestTheUpgraderCanAnswerAndCannotAsk is the other half.
//
// An upgrader that could insert could grant itself work, which is the
// same shape of hole one role over: the component that installs code
// would also be the component that decides code should be installed.
func TestTheUpgraderCanAnswerAndCannotAsk(t *testing.T) {
	panelPool, adminPool := queuePools(t)
	ctx := context.Background()

	r := ask(t, panelPool, "v0.20.0")

	claimed, err := Claim(ctx, adminPool, "test-upgrader")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claimed.ID != r.ID || claimed.State != StateRunning {
		t.Fatalf("claimed request %d in state %q; wanted %d running",
			claimed.ID, claimed.State, r.ID)
	}
	if err := Finish(ctx, adminPool, r.ID, StateSucceeded, nil, "v0.20.0", false); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	// The row above is finished, so the in-flight index is not what
	// refuses this. That distinction is the whole test: the first draft
	// inserted while a request was still running, the unique index
	// refused it, and the assertion could not tell the difference. It
	// passed against a database where schema_admin could insert freely.
	_, err = adminPool.Exec(ctx, `
		INSERT INTO panel_release_requests (actor_kind, actor_label, to_version)
		VALUES ('user', 'upgrader-inventing-work', 'v9.9.9')`)
	if err == nil {
		t.Fatal("schema_admin inserted a request. The component that installs code " +
			"would then also be the component that decides code should be installed")
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
// security policy" - and this project needs the second one, since every
// table is owned by schema_admin and an owner is exempt from RLS unless
// the table FORCEs it.
//
// What this must never match is a unique-violation. "The queue was
// busy" and "you are not allowed" are different answers, and a test that
// treats them alike is a test that passes on an open database.
func refusedByPrivilege(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "permission denied") ||
		strings.Contains(msg, "row-level security")
}

// TestOnlyOneRequestMayBeInFlight.
//
// Enforced by the index rather than by check-then-insert, because what
// is being prevented is two processes deciding at the same moment that
// nothing is running. Two of them here would be two downloads racing to
// replace the same files - one wins a rename and the other installs half
// a release, which is worse than two migrations.
func TestOnlyOneRequestMayBeInFlight(t *testing.T) {
	panelPool, adminPool := queuePools(t)
	ctx := context.Background()

	first := ask(t, panelPool, "v0.20.0")

	if _, err := Ask(ctx, panelPool, actor(), "op-iki", "v0.19.0", "v0.21.0"); !errors.Is(err, ErrAlreadyInFlight) {
		t.Fatalf("a second request was accepted while %d was pending (err = %v)", first.ID, err)
	}

	// And while one is *running*, which is the longer half of the window
	// here: an update downloads and unpacks, so a customer watching
	// nothing happen presses again.
	if _, err := Claim(ctx, adminPool, "test-upgrader"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := Ask(ctx, panelPool, actor(), "op-uc", "v0.19.0", "v0.21.0"); !errors.Is(err, ErrAlreadyInFlight) {
		t.Errorf("Ask during a running update returned %v, want ErrAlreadyInFlight", err)
	}

	// And once it is finished the next one goes through - the index
	// counts pending and running, not history. An update that could be
	// requested once per deployment would be worse than no button.
	if err := Finish(ctx, adminPool, first.ID, StateFailed, errors.New("test"), "v0.19.0", true); err != nil {
		t.Fatal(err)
	}
	if _, err := Ask(ctx, panelPool, actor(), "op-dort", "v0.19.0", "v0.21.0"); err != nil {
		t.Fatalf("nothing was in flight and this was refused: %v. A failed update that "+
			"cannot be retried is a deployment stuck for ever on one bad attempt", err)
	}
}

// TestAVersionThatCouldChangeTheAddressIsRefused.
//
// The version becomes part of a URL the upgrader fetches, so the shapes
// below are the ones that would make it fetch something else. Refused in
// Ask, before the row exists: a request that could never be served
// should not occupy the in-flight slot, and telling somebody why should
// not need an upgrader run.
func TestAVersionThatCouldChangeTheAddressIsRefused(t *testing.T) {
	panelPool, _ := queuePools(t)
	ctx := context.Background()

	bad := map[string]string{
		"climbs out of the path":   "v1.2.3/../../../etc/passwd",
		"a path of its own":        "v1.2.3/evil",
		"another host entirely":    "https://evil.example/v1.2.3",
		"a scheme":                 "file:///etc/shadow",
		"a query string":           "v1.2.3?x=1",
		"a newline":                "v1.2.3\nv9.9.9",
		"empty":                    "",
		"no leading v":             "1.2.3",
		"not a version at all":     "latest",
		"a wildcard":               "v1.2.*",
		"trailing slash":           "v1.2.3/",
		"a null byte":              "v1.2.3\x00",
		"leading whitespace":       " v1.2.3",
		"absurdly long":            "v1.2.3+" + strings.Repeat("a", 200),
		"a shell substitution":     "v1.2.3$(id)",
		"backslash instead of dot": `v1\2\3`,
	}
	for what, v := range bad {
		t.Run(what, func(t *testing.T) {
			if ValidVersion(v) {
				t.Fatalf("ValidVersion accepted %q, and it becomes part of an address", v)
			}
			if _, err := Ask(ctx, panelPool, actor(), "op", "v0.19.0", v); !errors.Is(err, ErrBadVersion) {
				t.Fatalf("Ask(%q) returned %v, want ErrBadVersion", v, err)
			}
		})
	}

	// And the ones that must work, because a check that refuses
	// everything is a check nobody notices is broken.
	for _, v := range []string{"v0.20.0", "v1.0.0", "v0.9.0+L3", "v10.20.30", "v1.2.3+A2.b"} {
		if !ValidVersion(v) {
			t.Errorf("ValidVersion refused %q, which VERSIONING.md defines", v)
		}
	}
}

// TestAClaimedRequestIsCheckedAgain.
//
// Ask ran in the panel's process. Claim runs in the upgrader's, and the
// row in between was written by a role the upgrader has no reason to
// trust. A claimant that reads its instructions out of a table has to
// validate them there - the alternative is a check that holds only while
// the only writer is the one that was tested.
func TestAClaimedRequestIsCheckedAgain(t *testing.T) {
	panelPool, adminPool := queuePools(t)
	ctx := context.Background()

	r := ask(t, panelPool, "v0.20.0")

	// Written past Ask, the way a compromised panel or a hand-edited row
	// would arrive. The superuser connection is the test standing in for
	// "somehow this value is in the table".
	if _, err := testdb.Admin(t).Exec(ctx,
		`UPDATE panel_release_requests SET to_version = $2 WHERE id = $1`,
		r.ID, "v1.2.3/../../evil"); err != nil {
		t.Fatal(err)
	}

	claimed, err := Claim(ctx, adminPool, "test-upgrader")
	if !errors.Is(err, ErrBadVersion) {
		t.Fatalf("Claim accepted a version that was not written through Ask: %v", err)
	}
	if claimed == nil {
		t.Fatal("Claim refused and returned no request, so the upgrader cannot mark " +
			"the bad row failed and it holds the in-flight slot for ever")
	}
}

// TestAStrandedClaimDoesNotBlockTheQueueForEver.
//
// A process killed mid-download leaves its row running, and the
// one-in-flight index then refuses every later request. Without the
// sweep the symptom is a button permanently dead because of a crash
// weeks ago, and nothing on the page explaining why.
func TestAStrandedClaimDoesNotBlockTheQueueForEver(t *testing.T) {
	panelPool, adminPool := queuePools(t)
	ctx := context.Background()

	r := ask(t, panelPool, "v0.20.0")
	if _, err := Claim(ctx, adminPool, "upgrader-that-died"); err != nil {
		t.Fatal(err)
	}

	// Backdated rather than waited out. A test that sleeps for the real
	// interval is a test somebody deletes.
	if _, err := testdb.Admin(t).Exec(ctx,
		`UPDATE panel_release_requests SET claimed_at = now() - interval '2 hours' WHERE id = $1`,
		r.ID); err != nil {
		t.Fatal(err)
	}

	n, err := ExpireStale(ctx, adminPool, StaleAfter)
	if err != nil {
		t.Fatalf("ExpireStale: %v", err)
	}
	if n != 1 {
		t.Fatalf("the sweep took %d rows; the stranded one is exactly one, and a "+
			"sweep that takes more is one that can interrupt a live update", n)
	}

	latest, err := Latest(ctx, panelPool)
	if err != nil {
		t.Fatal(err)
	}
	if latest == nil || latest.InFlight() {
		t.Error("the queue is still blocked after the sweep")
	}
}

// TestTheSweepLeavesALiveClaimAlone is the other half of the sweep, and
// the half with teeth.
//
// A sweep that ignored the age would fail an update while it was
// replacing binaries - the worst moment available, because the row then
// says "failed" while a half-installed release is on the disk and
// nothing is coming back to finish it.
//
// Its own test rather than a second assertion in the one above, because
// the one-in-flight index means a stranded row and a live one cannot
// exist at the same time. Two tests is what the constraint costs.
func TestTheSweepLeavesALiveClaimAlone(t *testing.T) {
	panelPool, adminPool := queuePools(t)
	ctx := context.Background()

	r := ask(t, panelPool, "v0.20.0")
	if _, err := Claim(ctx, adminPool, "an-upgrader-doing-its-job"); err != nil {
		t.Fatal(err)
	}

	n, err := ExpireStale(ctx, adminPool, StaleAfter)
	if err != nil {
		t.Fatalf("ExpireStale: %v", err)
	}
	if n != 0 {
		t.Fatalf("the sweep took %d rows and the only one is a claim made a moment "+
			"ago. An update failed mid-install leaves a half-replaced release with "+
			"nothing coming back for it", n)
	}

	latest, err := Latest(ctx, panelPool)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != StateRunning {
		t.Errorf("the live claim is %q after a sweep that should not have seen it",
			latest.State)
	}
	_ = r
}

// TestLatestSaysNothingHasBeenAskedRatherThanFailing.
//
// Every deployment starts here, and a page that reported an error would
// send a new customer looking for a fault on their first visit.
func TestLatestSaysNothingHasBeenAskedRatherThanFailing(t *testing.T) {
	panelPool, _ := queuePools(t)

	r, err := Latest(context.Background(), panelPool)
	if err != nil {
		t.Fatalf("Latest on an empty queue: %v", err)
	}
	if r != nil {
		t.Fatalf("Latest returned a request from an empty queue: %+v", r)
	}
	if r.InFlight() {
		t.Error("a nil request reports itself in flight")
	}
}

// TestTheOutcomeSurvivesTheBinaryItDescribes.
//
// After a rollback the question is "so what is running", and the binary
// that could answer it has been replaced. The row has to carry it.
func TestTheOutcomeSurvivesTheBinaryItDescribes(t *testing.T) {
	panelPool, adminPool := queuePools(t)
	ctx := context.Background()

	r := ask(t, panelPool, "v0.20.0")
	if _, err := Claim(ctx, adminPool, "test-upgrader"); err != nil {
		t.Fatal(err)
	}

	cause := errors.New("the new collector did not answer its health check")
	if err := Finish(ctx, adminPool, r.ID, StateFailed, cause, "v0.19.0", true); err != nil {
		t.Fatal(err)
	}

	got, err := Latest(ctx, panelPool)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case got.State != StateFailed:
		t.Errorf("state is %q", got.State)
	case got.InstalledVersion != "v0.19.0":
		t.Errorf("installed_version is %q; after a rollback it is what came back, "+
			"and it is the first thing somebody woken up by a down site reads",
			got.InstalledVersion)
	case !got.RolledBack:
		t.Error("rolled_back is false after a rollback. It is a column rather than a " +
			"sentence in error_chain precisely so a page does not have to read prose")
	case !strings.Contains(got.ErrorChain, "health check"):
		t.Errorf("the cause did not survive: %q", got.ErrorChain)
	}
	if got.FinishedAt == nil {
		t.Error("a finished request has no finished_at")
	}
	if got.InFlight() {
		t.Error("a finished request still reports itself in flight, so the button " +
			"would stay hidden for ever")
	}
}
