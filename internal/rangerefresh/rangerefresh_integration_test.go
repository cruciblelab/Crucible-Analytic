//go:build integration

package rangerefresh

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// The queue, against the real table and as the real roles.
//
// As the roles, not as the owner, because the whole design is a split:
// the panel asks and the fetcher answers, and a suite that ran both
// halves as one privileged role would be testing a database nobody
// deploys. internal/testdb's package comment lists the three real bugs a
// more privileged fixture hid.

// queue opens the two pools this package's design needs and clears the
// table around the test.
func queue(t *testing.T) (panelPool, fetcherPool *pgxpool.Pool) {
	t.Helper()
	admin := testdb.Admin(t)
	// One pending-or-running row is permitted in the whole table, so two
	// suites running at once collide by construction rather than by
	// sharing a name. See testdb.RefreshQueueLock.
	testdb.Lock(t, admin, testdb.RefreshQueueLock)

	clear := func() {
		if _, err := admin.Exec(context.Background(),
			`DELETE FROM ip_range_refresh_requests`); err != nil {
			t.Logf("cleanup: clearing the queue: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)

	return testdb.Pool(t, testdb.Panel), testdb.Pool(t, testdb.Collector)
}

func actor() Actor { return Actor{Kind: "user", Label: "test@example.invalid"} }

// TestAskClaimFinish is the whole path, in the order it happens.
func TestAskClaimFinish(t *testing.T) {
	panelPool, fetcherPool := queue(t)
	ctx := context.Background()

	req, err := Ask(ctx, panelPool, actor(), "op-test")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if req.State != StatePending {
		t.Errorf("a fresh request is %q, want pending", req.State)
	}
	if !req.InFlight() {
		t.Error("a pending request does not report itself in flight, so the page " +
			"would offer the button again while one is queued")
	}

	claimed, err := Claim(ctx, fetcherPool, "test-fetcher")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claimed.ID != req.ID {
		t.Errorf("claimed %d, asked %d", claimed.ID, req.ID)
	}
	if claimed.State != StateRunning || claimed.ClaimedBy != "test-fetcher" {
		t.Errorf("claimed row = %+v, want running and named", claimed)
	}

	// And nothing is left to claim, which is what stops a second fetcher
	// running the same refresh.
	if _, err := Claim(ctx, fetcherPool, "second-fetcher"); !errors.Is(err, ErrNothingToDo) {
		t.Errorf("a second Claim returned %v, want ErrNothingToDo. Two fetchers "+
			"taking one request is two refreshes of the same data at once", err)
	}

	if err := Finish(ctx, fetcherPool, req.ID, StateSucceeded, 10, 0, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	// Read back through the panel's role, which is how the page reads it.
	latest, err := Latest(ctx, panelPool)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if latest.State != StateSucceeded || latest.FilesOK != 10 || latest.FilesFailed != 0 {
		t.Errorf("finished row = %+v, want succeeded with 10 files", latest)
	}
	if latest.FinishedAt == nil {
		t.Error("the request was never closed; a refresh with no end is the " +
			"interesting case")
	}
	if latest.InFlight() {
		t.Error("a finished request still reports itself in flight, so the button " +
			"would stay hidden for ever")
	}
}

// TestPressingTwiceDoesNotStartTwoRefreshes is M3's done criterion,
// stated as a test.
//
// Enforced by the index rather than by a check-then-insert, because what
// is being prevented is two requests deciding at the same moment that
// nothing is running - which a check first and an insert second does not
// prevent at all.
func TestPressingTwiceDoesNotStartTwoRefreshes(t *testing.T) {
	panelPool, fetcherPool := queue(t)
	ctx := context.Background()

	if _, err := Ask(ctx, panelPool, actor(), "op-one"); err != nil {
		t.Fatalf("the first Ask failed: %v", err)
	}
	if _, err := Ask(ctx, panelPool, actor(), "op-two"); !errors.Is(err, ErrAlreadyInFlight) {
		t.Fatalf("the second Ask returned %v, want ErrAlreadyInFlight", err)
	}

	// And while one is *running* too, which is the longer half of the
	// window: a refresh takes seconds, and a customer watching nothing
	// happen presses again.
	if _, err := Claim(ctx, fetcherPool, "test-fetcher"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := Ask(ctx, panelPool, actor(), "op-three"); !errors.Is(err, ErrAlreadyInFlight) {
		t.Errorf("Ask during a running refresh returned %v, want ErrAlreadyInFlight", err)
	}
}

// TestAFinishedRequestDoesNotBlockTheNextOne.
//
// The other half of the index: it counts pending and running only, so a
// customer can press again after reading what happened. A button that
// worked once per deployment would be worse than no button.
func TestAFinishedRequestDoesNotBlockTheNextOne(t *testing.T) {
	panelPool, fetcherPool := queue(t)
	ctx := context.Background()

	for _, outcome := range []State{StateSucceeded, StateFailed} {
		req, err := Ask(ctx, panelPool, actor(), "op-"+string(outcome))
		if err != nil {
			t.Fatalf("Ask after a %s request: %v", outcome, err)
		}
		if _, err := Claim(ctx, fetcherPool, "test-fetcher"); err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if err := Finish(ctx, fetcherPool, req.ID, outcome, 1, 0, ""); err != nil {
			t.Fatalf("Finish(%s): %v", outcome, err)
		}
	}
}

// TestAnUnclaimedRequestExpires.
//
// The difference from internal/upgrade that matters, and the reason this
// package has ExpireStale at all: asn_lookup is off by default, so on
// most deployments nothing polls this table. Without expiry the first
// press would jam the in-flight index for ever.
func TestAnUnclaimedRequestExpires(t *testing.T) {
	panelPool, _ := queue(t)
	ctx := context.Background()
	admin := testdb.Admin(t)

	req, err := Ask(ctx, panelPool, actor(), "op-unclaimed")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	// Nothing has claimed it, and it is fresh: the panel must not remove
	// a request a fetcher is about to take.
	if removed, err := ExpireStale(ctx, panelPool, UnclaimedAfter); err != nil {
		t.Fatalf("ExpireStale: %v", err)
	} else if removed != 0 {
		t.Fatalf("a fresh request was expired after %v; the customer would watch "+
			"nothing happen for a reason no page could explain", UnclaimedAfter)
	}

	// Age it past the window.
	if _, err := admin.Exec(ctx, `
		UPDATE ip_range_refresh_requests SET requested_at = now() - $1::interval
		WHERE id = $2`, "1 hour", req.ID); err != nil {
		t.Fatalf("ageing the request: %v", err)
	}

	if removed, err := ExpireStale(ctx, panelPool, UnclaimedAfter); err != nil {
		t.Fatalf("ExpireStale: %v", err)
	} else if removed != 1 {
		t.Errorf("ExpireStale removed %d rows, want the one nobody claimed", removed)
	}

	// And the button works again, which is the whole point.
	if _, err := Ask(ctx, panelPool, actor(), "op-after"); err != nil {
		t.Errorf("after the expiry the button is still jammed: %v", err)
	}
}

// TestExpiryLeavesAClaimedRequestAlone.
//
// A running row belongs to a fetcher that may still be working - a
// refresh downloads about 124 MB - and removing it would free the
// in-flight slot while the work continues, so a second refresh could
// start on top of the first.
func TestExpiryLeavesAClaimedRequestAlone(t *testing.T) {
	panelPool, fetcherPool := queue(t)
	ctx := context.Background()
	admin := testdb.Admin(t)

	req, err := Ask(ctx, panelPool, actor(), "op-claimed")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := Claim(ctx, fetcherPool, "slow-fetcher"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := admin.Exec(ctx, `
		UPDATE ip_range_refresh_requests SET requested_at = now() - $1::interval
		WHERE id = $2`, "1 hour", req.ID); err != nil {
		t.Fatalf("ageing: %v", err)
	}

	if removed, err := ExpireStale(ctx, panelPool, UnclaimedAfter); err != nil {
		t.Fatalf("ExpireStale: %v", err)
	} else if removed != 0 {
		t.Error("a claimed request was expired. The fetcher holding it is still " +
			"downloading, and the freed slot lets a second refresh start on top " +
			"of the first")
	}
}

// TestAStrandedClaimIsReleased.
//
// A process killed mid-refresh leaves a row at running for ever, and the
// in-flight index then refuses every later request - a button
// permanently greyed out because of a crash weeks ago.
func TestAStrandedClaimIsReleased(t *testing.T) {
	panelPool, fetcherPool := queue(t)
	ctx := context.Background()
	admin := testdb.Admin(t)

	req, err := Ask(ctx, panelPool, actor(), "op-stranded")
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if _, err := Claim(ctx, fetcherPool, "doomed-fetcher"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := admin.Exec(ctx, `
		UPDATE ip_range_refresh_requests SET claimed_at = now() - $1::interval
		WHERE id = $2`, "1 hour", req.ID); err != nil {
		t.Fatalf("ageing the claim: %v", err)
	}

	released, err := ReleaseStaleClaims(ctx, fetcherPool, 10*time.Minute)
	if err != nil {
		t.Fatalf("ReleaseStaleClaims: %v", err)
	}
	if released != 1 {
		t.Errorf("released %d claims, want 1", released)
	}

	latest, err := Latest(ctx, panelPool)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != StateFailed {
		t.Errorf("the stranded request is %q, want failed - nobody knows how far "+
			"it got, and saying so is the honest record", latest.State)
	}
	if latest.ErrorChain == "" {
		t.Error("the released row says nothing about why it is failed")
	}
	if _, err := Ask(ctx, panelPool, actor(), "op-after-crash"); err != nil {
		t.Errorf("the button is still jammed after the release: %v", err)
	}
}

// TestNeitherSideHoldsBothRights.
//
// The design, measured against the live database rather than read off
// grants.sql. A panel that could write "succeeded" could report a
// refresh that never happened; a fetcher that could ask could start work
// nobody requested.
func TestNeitherSideHoldsBothRights(t *testing.T) {
	panelPool, fetcherPool := queue(t)
	ctx := context.Background()

	if _, err := Ask(ctx, fetcherPool, actor(), "op-from-the-fetcher"); err == nil {
		t.Error("the fetcher could write a request. Then a compromised collector " +
			"could manufacture the appearance of a customer asking for something")
	}

	req, err := Ask(ctx, panelPool, actor(), "op-rights")
	if err != nil {
		t.Fatalf("the panel could not ask: %v", err)
	}
	if err := Finish(ctx, panelPool, req.ID, StateSucceeded, 10, 0, ""); err == nil {
		t.Error("the panel could finish its own request. A reader that can write " +
			"its own evidence can report a refresh that never ran")
	}
	if _, err := Claim(ctx, panelPool, "panel-pretending"); err == nil {
		t.Error("the panel could claim a request, which is the same authority " +
			"with an extra step")
	}
}
