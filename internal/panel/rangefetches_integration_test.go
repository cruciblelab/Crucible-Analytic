//go:build integration

package panel

import (
	"context"
	"testing"
	"time"

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
