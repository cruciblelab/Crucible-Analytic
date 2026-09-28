//go:build integration

package panel

import (
	"context"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// logRow writes one line the way a shape of row can exist in panel_logs,
// through the schema's owner so its time and columns can be chosen.
//
// Times are in the future on purpose: other suites write to this table
// while this one runs, and a future row sorts first - so "the newest n"
// is this test's rows and not whatever else arrived in the same second.
func logRow(t *testing.T, admin *pgxpool.Pool, message, level, site, attrs, at string) {
	t.Helper()
	if _, err := admin.Exec(context.Background(), `
		INSERT INTO panel_logs (at, service, level, message, site_id, attrs)
		VALUES (now() + $5::interval, 'panel_user', $2, $1, $3, $4::jsonb)`,
		message, level, site, attrs, at); err != nil {
		t.Fatalf("writing %q: %v", message, err)
	}
}

// TestRecentWarningsReadsWhatTheFileMayCarry: the level, the window,
// whose lines, the order and the bound, each with a row on both sides.
func TestRecentWarningsReadsWhatTheFileMayCarry(t *testing.T) {
	ns := "panel-gunluk"
	s := newTestStore(t, ns)
	ctx := context.Background()
	admin := testdb.Admin(t)
	clear := func() {
		if _, err := admin.Exec(ctx, `DELETE FROM panel_logs WHERE message LIKE $1`, ns+"%"); err != nil {
			t.Errorf("clearing: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)

	mine, theirs := "site-a-"+ns, "site-b-"+ns
	logRow(t, admin, ns+" warn, no site", "WARN", "", `{}`, "1 hour 10 minutes")
	logRow(t, admin, ns+" error, mine", "ERROR", mine, `{}`, "1 hour 9 minutes")
	logRow(t, admin, ns+" error, theirs", "ERROR", theirs, `{}`, "1 hour 8 minutes")
	// Written before the sink's site key matched what callers wrote: the
	// site is in the attributes and the column is empty.
	logRow(t, admin, ns+" legacy, mine", "ERROR", "", `{"site":"`+mine+`"}`, "1 hour 7 minutes")
	logRow(t, admin, ns+" legacy, theirs", "ERROR", "", `{"site":"`+theirs+`"}`, "1 hour 6 minutes")
	logRow(t, admin, ns+" info", "INFO", "", `{}`, "1 hour 5 minutes")
	logRow(t, admin, ns+" debug", "DEBUG", "", `{}`, "1 hour 4 minutes")
	logRow(t, admin, ns+" eight days old", "ERROR", "", `{}`, "-8 days")

	read := func(f LogFilter) map[string]bool {
		t.Helper()
		lines, _, err := s.RecentWarnings(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, l := range lines {
			got[l.Message] = true
		}
		return got
	}
	const week = 7 * 24 * time.Hour
	all := read(LogFilter{Since: week, Limit: 100000})
	for msg, want := range map[string]bool{
		ns + " warn, no site": true, ns + " error, mine": true, ns + " error, theirs": true,
		ns + " legacy, mine": true, ns + " legacy, theirs": true,
		ns + " info": false, ns + " debug": false, ns + " eight days old": false,
	} {
		if all[msg] != want {
			t.Errorf("the operator's view: %q present = %v, want %v", msg, all[msg], want)
		}
	}

	owned := read(LogFilter{Since: week, Limit: 100000, Sites: []string{mine}})
	for msg, want := range map[string]bool{
		ns + " warn, no site": true, ns + " error, mine": true, ns + " legacy, mine": true,
		ns + " error, theirs": false, ns + " legacy, theirs": false,
	} {
		if owned[msg] != want {
			t.Errorf("an owner's view: %q present = %v, want %v", msg, owned[msg], want)
		}
	}
	none := read(LogFilter{Since: week, Limit: 100000, Sites: []string{}})
	if !none[ns+" warn, no site"] || none[ns+" error, mine"] || none[ns+" legacy, mine"] {
		t.Errorf("owning no site should read lines about no site only: %v", none)
	}

	// Newest first, bounded, and saying so. The newest rows that qualify
	// are this test's own, being in the future; the newest of them is the
	// one furthest ahead.
	lines, truncated, err := s.RecentWarnings(ctx, LogFilter{Since: week, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || !truncated {
		t.Fatalf("limit 2 gave %d lines, truncated=%v", len(lines), truncated)
	}
	if lines[0].Message != ns+" warn, no site" || lines[1].Message != ns+" error, mine" {
		t.Errorf("not newest first: %q, %q", lines[0].Message, lines[1].Message)
	}

	// A line carries its site from the column, and a legacy line from its
	// attributes - so the file can say whose it is either way.
	wide, _, err := s.RecentWarnings(ctx, LogFilter{Since: week, Limit: 100000})
	if err != nil {
		t.Fatal(err)
	}
	sites := map[string]string{}
	for _, l := range wide {
		sites[l.Message] = l.Site
	}
	if sites[ns+" error, theirs"] != theirs || sites[ns+" legacy, theirs"] != theirs ||
		sites[ns+" warn, no site"] != "" {
		t.Errorf("sites read: column %q, legacy %q, none %q",
			sites[ns+" error, theirs"], sites[ns+" legacy, theirs"], sites[ns+" warn, no site"])
	}
}
