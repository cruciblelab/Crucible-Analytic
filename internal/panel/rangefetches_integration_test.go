//go:build integration

package panel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/rangerefresh"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// The panel's read of the fetch log.
//
// Seeded through the admin pool rather than through the panel's own,
// deliberately: panel_user has no INSERT here and that is the whole
// design. internal/asnlookup's suite proves the writer's side and proves
// the panel is refused; this proves the reader returns what is there.

func fetchLogStore(t *testing.T) *Store {
	t.Helper()
	store := newTestStore(t, "rangefetch")
	// internal/asnlookup's upgrade-path test revokes and re-grants
	// privileges on this table to replay what the button does, and
	// `go test ./...` runs the two packages at once. A read landing
	// inside that revoke fails with "permission denied", which reads as
	// a broken grant rather than as two suites overlapping.
	testdb.Lock(t, testdb.Admin(t), testdb.FetchLogLock)
	clear := func() {
		if _, err := testdb.Admin(t).Exec(context.Background(),
			`DELETE FROM ip_range_fetches`); err != nil {
			t.Logf("cleanup: clearing the fetch log: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)
	return store
}

// seedFetch inserts one row, ago before now.
func seedFetch(t *testing.T, source, kind, family, outcome string, ago time.Duration, rows, bytes int64) {
	t.Helper()
	when := time.Now().Add(-ago)
	if _, err := testdb.Admin(t).Exec(context.Background(), `
		INSERT INTO ip_range_fetches
		  (started_at, finished_at, source_id, kind, family, origin,
		   outcome, rows_parsed, bytes_read, error_chain)
		VALUES ($1::timestamptz, $1::timestamptz + interval '2 seconds',
		        $2, $3, $4, 'download', $5::text, $6, $7,
		        CASE WHEN $5::text = 'failed' THEN 'HTTP 404' ELSE '' END)`,
		when, source, kind, family, outcome, rows, bytes); err != nil {
		t.Fatalf("seeding %s/%s: %v", source, family, err)
	}
}

func TestRecentRangeFetches_NewestFirst(t *testing.T) {
	store := fetchLogStore(t)

	seedFetch(t, "user-country", "country", "ipv4", "succeeded", 3*time.Hour, 300000, 8_796_182)
	seedFetch(t, "user-country", "country", "ipv6", "succeeded", 2*time.Hour, 200000, 15_954_424)
	seedFetch(t, "origin-asn", "asn", "ipv4", "failed", 1*time.Hour, 0, 0)

	got, err := store.RecentRangeFetches(context.Background(), 10)
	if err != nil {
		t.Fatalf("RecentRangeFetches: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3", len(got))
	}
	if got[0].SourceID != "origin-asn" || !got[0].Failed() {
		t.Errorf("newest row is %+v; want the failed origin-asn attempt first, "+
			"because a list that buries the newest attempt buries the one somebody "+
			"came to look at", got[0])
	}
	if got[2].Family != "ipv4" || got[2].SourceID != "user-country" {
		t.Errorf("oldest row is %+v, want user-country/ipv4", got[2])
	}
	if got[1].BytesRead != 15_954_424 || got[1].RowsParsed != 200000 {
		t.Errorf("the numbers did not survive the round trip: %+v", got[1])
	}
	if got[0].ErrorChain == "" {
		t.Error("a failed row came back with no error chain, so the page can say " +
			"that it failed and not why")
	}
	if took := got[0].Took(); took != 2*time.Second {
		t.Errorf("Took() = %v, want 2s from the seeded start and finish", took)
	}
}

// TestRecentRangeFetches_LimitIsBounded.
//
// A caller that asks for everything gets the cap rather than the table.
func TestRecentRangeFetches_LimitIsBounded(t *testing.T) {
	store := fetchLogStore(t)
	for i := range 4 {
		seedFetch(t, "user-country", "country", "ipv4", "succeeded",
			time.Duration(i)*time.Hour, 1, 1)
	}

	got, err := store.RecentRangeFetches(context.Background(), 2)
	if err != nil {
		t.Fatalf("RecentRangeFetches: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("asked for 2, got %d", len(got))
	}

	// Zero and absurd both land on the cap rather than on "everything"
	// or on nothing.
	for _, limit := range []int{0, -1, maxRangeFetches * 10} {
		got, err := store.RecentRangeFetches(context.Background(), limit)
		if err != nil {
			t.Fatalf("RecentRangeFetches(%d): %v", limit, err)
		}
		if len(got) != 4 {
			t.Errorf("limit %d returned %d rows, want all 4 that exist "+
				"(bounded by the cap, not by the caller)", limit, len(got))
		}
	}
}

// TestLastRangeFetchPerFile_ShowsTheStaleOneRatherThanBuryingIt.
//
// The reason this query exists. A plain "last N" is dominated by
// whichever dataset refreshed most recently, so the file that has been
// failing for a month scrolls off the bottom - which is the row somebody
// came to look for.
func TestLastRangeFetchPerFile_ShowsTheStaleOneRatherThanBuryingIt(t *testing.T) {
	store := fetchLogStore(t)

	// One file broke a month ago and has been broken since; the other
	// three have refreshed many times, most recently minutes ago.
	seedFetch(t, "origin-asn", "asn", "ipv6", "failed", 30*24*time.Hour, 0, 0)
	for i := range 20 {
		ago := time.Duration(i) * time.Hour
		seedFetch(t, "user-country", "country", "ipv4", "succeeded", ago, 300000, 8_796_182)
		seedFetch(t, "user-country", "country", "ipv6", "succeeded", ago, 200000, 15_954_424)
		seedFetch(t, "origin-asn", "asn", "ipv4", "succeeded", ago, 400000, 26_901_565)
	}

	// The plain list cannot see it, which is the finding rather than an
	// aside: 60 rows of healthy refreshes come first.
	recent, err := store.RecentRangeFetches(context.Background(), 20)
	if err != nil {
		t.Fatalf("RecentRangeFetches: %v", err)
	}
	for _, f := range recent {
		if f.Failed() {
			t.Fatal("the broken file appeared in the last 20 rows, so this test is " +
				"no longer set up to demonstrate anything")
		}
	}

	last, err := store.LastRangeFetchPerFile(context.Background())
	if err != nil {
		t.Fatalf("LastRangeFetchPerFile: %v", err)
	}
	if len(last) != 4 {
		t.Fatalf("got %d rows, want one per file (two country, two asn): %+v", len(last), last)
	}

	var failed []RangeFetch
	for _, f := range last {
		if f.Failed() {
			failed = append(failed, f)
		}
	}
	if len(failed) != 1 {
		t.Fatalf("got %d failed files, want exactly the one that broke: %+v", len(failed), failed)
	}
	if failed[0].SourceID != "origin-asn" || failed[0].Family != "ipv6" {
		t.Errorf("the stale file is reported as %s/%s, want origin-asn/ipv6",
			failed[0].SourceID, failed[0].Family)
	}
	if time.Since(failed[0].StartedAt) < 29*24*time.Hour {
		t.Errorf("the reported attempt is %v old; the newest attempt for that file "+
			"is the month-old failure, and a fresher one would mean the query "+
			"picked a different row", time.Since(failed[0].StartedAt))
	}
}

// TestTheReaderIsQuietOnAnEmptyLog.
//
// The ordinary state of a deployment with asn_lookup off: the table
// exists and nothing has ever fetched. An empty slice and no error,
// because a page that reported this as a fault would put a red section
// on a correctly configured installation.
func TestTheReaderIsQuietOnAnEmptyLog(t *testing.T) {
	store := fetchLogStore(t)

	recent, err := store.RecentRangeFetches(context.Background(), 10)
	if err != nil {
		t.Fatalf("RecentRangeFetches on an empty log: %v", err)
	}
	if len(recent) != 0 {
		t.Errorf("got %d rows from an empty log", len(recent))
	}
	last, err := store.LastRangeFetchPerFile(context.Background())
	if err != nil {
		t.Fatalf("LastRangeFetchPerFile on an empty log: %v", err)
	}
	if last == nil {
		t.Error("returned a nil slice rather than an empty one; a template ranging " +
			"over it should not have to know the difference")
	}
}

// --- M3: the button's rules ---

// refreshStore is fetchLogStore plus the queue's lock and cleanup.
func refreshStore(t *testing.T) *Store {
	t.Helper()
	store := fetchLogStore(t)
	testdb.Lock(t, testdb.Admin(t), testdb.RefreshQueueLock)
	clear := func() {
		if _, err := testdb.Admin(t).Exec(context.Background(),
			`DELETE FROM ip_range_refresh_requests`); err != nil {
			t.Logf("cleanup: clearing the refresh queue: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)
	return store
}

// mayManageSettings is a customer who can change settings, which is the
// whole entitlement this button asks for.
func mayManageSettings() Access {
	return Access{
		Principal: Principal{Kind: PrincipalUser, UserID: 1, Label: "sahip@example.invalid"},
		Role:      RoleOwner,
		Member:    true,
	}
}

// mayOnlyLook is a viewer.
func mayOnlyLook() Access {
	return Access{
		Principal: Principal{Kind: PrincipalUser, UserID: 2, Label: "izleyici@example.invalid"},
		Role:      RoleViewer,
		Member:    true,
	}
}

// TestTheRefreshButtonNeedsNoDeveloperPassword.
//
// M3's "works for the customer with the default" criterion, and the
// place the difference from L3 is asserted rather than only explained.
//
// The rule this project follows is that anything which can make work for
// the developer sits behind the developer password, because a customer
// can grant themselves any capability. Pressing this makes work for
// nobody: it re-downloads two public datasets onto the customer's own
// server. So entitlement is the whole gate, and a test that passes only
// when a password is supplied would be the wrong test.
func TestTheRefreshButtonNeedsNoDeveloperPassword(t *testing.T) {
	store := refreshStore(t)

	req, err := store.RequestRangeRefresh(context.Background(), mayManageSettings(), "op-m3")
	if err != nil {
		t.Fatalf("a customer who may change settings could not ask for a refresh: %v.\n"+
			"The default has to work for somebody who does not know the developer "+
			"password, because most customers never will", err)
	}
	if req.State != "pending" {
		t.Errorf("the request is %q, want pending", req.State)
	}
	if req.Actor.Label != "sahip@example.invalid" {
		t.Errorf("the row does not name who asked: %+v", req.Actor)
	}
}

// TestAViewerCannotAskForARefresh.
//
// Entitlement is the gate, so it has to actually be one.
func TestAViewerCannotAskForARefresh(t *testing.T) {
	store := refreshStore(t)

	_, err := store.RequestRangeRefresh(context.Background(), mayOnlyLook(), "op-viewer")
	if !errors.Is(err, ErrSettingNotWritable) {
		t.Fatalf("a viewer's request returned %v, want ErrSettingNotWritable", err)
	}

	// And nothing was written: a refusal that still queues a row would
	// hold the in-flight slot against the person who is allowed.
	latest, err := rangerefresh.Latest(context.Background(), store.Pool())
	if err != nil {
		t.Fatal(err)
	}
	if latest != nil {
		t.Errorf("a refused request left a row behind: %+v", latest)
	}
}

// TestPressingTwiceIsRefusedWithItsOwnAnswer.
//
// M3's second criterion at the store, where the page reads it. Refused
// rather than silently ignored, and with an error the page can turn into
// "one is already going" rather than "that failed".
func TestPressingTwiceIsRefusedWithItsOwnAnswer(t *testing.T) {
	store := refreshStore(t)
	ctx := context.Background()

	if _, err := store.RequestRangeRefresh(ctx, mayManageSettings(), "op-one"); err != nil {
		t.Fatalf("the first press failed: %v", err)
	}
	_, err := store.RequestRangeRefresh(ctx, mayManageSettings(), "op-two")
	if !IsRangeRefreshBusy(err) {
		t.Fatalf("the second press returned %v, want the busy refusal", err)
	}
}

// TestAJammedQueueUnjamsItself.
//
// The failure mode this whole expiry exists for, played out: asn_lookup
// is off, so nothing claims the request, and without the expiry the
// in-flight index would make the first press the last one this
// deployment ever accepts.
func TestAJammedQueueUnjamsItself(t *testing.T) {
	store := refreshStore(t)
	ctx := context.Background()
	admin := testdb.Admin(t)

	first, err := store.RequestRangeRefresh(ctx, mayManageSettings(), "op-jam")
	if err != nil {
		t.Fatalf("the first press failed: %v", err)
	}

	// Nobody claimed it, and time passed.
	if _, err := admin.Exec(ctx, `
		UPDATE ip_range_refresh_requests SET requested_at = now() - interval '1 hour'
		WHERE id = $1`, first.ID); err != nil {
		t.Fatalf("ageing the request: %v", err)
	}

	second, err := store.RequestRangeRefresh(ctx, mayManageSettings(), "op-again")
	if err != nil {
		t.Fatalf("the button stayed jammed after nothing claimed the first press: %v.\n"+
			"asn_lookup is off by default, so this is the ordinary state of most "+
			"deployments rather than an edge case", err)
	}
	if second.ID == first.ID {
		t.Error("the second press returned the first request rather than a new one")
	}
}

// TestTheStatusSaysNothingIsFetching.
//
// The sentence a customer needs when they press the button on a
// deployment with asn_lookup off. Without it the page shows a request
// that never moves, and the honest reading of that is "the panel is
// broken".
func TestTheStatusSaysNothingIsFetching(t *testing.T) {
	store := refreshStore(t)
	ctx := context.Background()

	status, err := store.RangeRefreshStatus(ctx, mayManageSettings())
	if err != nil {
		t.Fatalf("RangeRefreshStatus: %v", err)
	}
	if !status.NothingIsFetching {
		t.Error("with an empty fetch log the status does not report that nothing " +
			"is fetching, so the page cannot explain an unanswered request")
	}
	if !status.Allowed {
		t.Error("a customer who may change settings is reported as not allowed")
	}
	if status.Unanswered() {
		t.Error("with no request at all the status reports one as unanswered")
	}

	req, err := store.RequestRangeRefresh(ctx, mayManageSettings(), "op-status")
	if err != nil {
		t.Fatal(err)
	}
	// Fresh: not yet unanswered, because a fetcher may be about to take it.
	status, err = store.RangeRefreshStatus(ctx, mayManageSettings())
	if err != nil {
		t.Fatal(err)
	}
	if status.Unanswered() {
		t.Error("a request written a moment ago is already called unanswered; a " +
			"fetcher polls every thirty seconds and has not had its turn")
	}

	if _, err := testdb.Admin(t).Exec(ctx, `
		UPDATE ip_range_refresh_requests SET requested_at = now() - interval '1 hour'
		WHERE id = $1`, req.ID); err != nil {
		t.Fatal(err)
	}
	status, err = store.RangeRefreshStatus(ctx, mayManageSettings())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Unanswered() {
		t.Error("an hour-old pending request is not reported as unanswered, so the " +
			"page shows a spinner where a sentence belongs")
	}
}

// TestTheQueueIsSweptByHousekeeping.
//
// The plan's standing warning, applied to this table too: a sweep with
// no caller is the failure that stays green.
//
// Swept by the panel here rather than by the fetcher - the opposite of
// ip_range_fetches - and the reason is in Report: these rows accumulate
// on deployments where nothing is fetching, which is exactly where the
// fetcher is not running to sweep them.
func TestTheQueueIsSweptByHousekeeping(t *testing.T) {
	store := refreshStore(t)
	ctx := context.Background()
	admin := testdb.Admin(t)

	seed := func(state string, age time.Duration) {
		if _, err := admin.Exec(ctx, `
			INSERT INTO ip_range_refresh_requests
			  (requested_at, actor_kind, actor_label, state, finished_at)
			VALUES (now() - $1::interval, 'user', 'eski@example.invalid', $2, now())`,
			seconds(age), state); err != nil {
			t.Fatalf("seeding a %s row aged %v: %v", state, age, err)
		}
	}
	seed("succeeded", rangeRefreshRetention+24*time.Hour)
	seed("failed", rangeRefreshRetention+24*time.Hour)
	seed("succeeded", time.Hour)

	rep, err := store.Housekeeping(ctx)
	if err != nil {
		t.Fatalf("Housekeeping: %v", err)
	}
	if rep.RangeRefreshRequests != 2 {
		t.Errorf("housekeeping removed %d refresh requests, want the two past %v",
			rep.RangeRefreshRequests, rangeRefreshRetention)
	}

	var left int
	if err := admin.QueryRow(ctx,
		`SELECT count(*) FROM ip_range_refresh_requests`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Errorf("%d rows left, want the one inside retention", left)
	}
}

// TestTheSweepLeavesAnInFlightRequestAlone.
//
// A pending row is waiting for a fetcher and a running one is being
// worked on. Deleting either frees the in-flight slot underneath
// whoever holds it, so a second refresh could start on top of the first.
func TestTheSweepLeavesAnInFlightRequestAlone(t *testing.T) {
	store := refreshStore(t)
	ctx := context.Background()

	if _, err := testdb.Admin(t).Exec(ctx, `
		INSERT INTO ip_range_refresh_requests
		  (requested_at, actor_kind, actor_label, state)
		VALUES (now() - interval '400 days', 'user', 'cok@eski.invalid', 'running')`); err != nil {
		t.Fatal(err)
	}

	removed, err := store.PurgeOldRangeRefreshRequests(ctx)
	if err != nil {
		t.Fatalf("PurgeOldRangeRefreshRequests: %v", err)
	}
	if removed != 0 {
		t.Error("the sweep removed a running request. The fetcher holding it may " +
			"still be downloading, and the freed slot lets a second refresh start " +
			"on top of the first")
	}
}
