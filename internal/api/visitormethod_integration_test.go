//go:build integration

// The summary says which of the two answers it gave - on the shared
// development database, which has no timescaledb_toolkit.
//
// So this is the branch most deployments are on the day they upgrade:
// the sketch tables do not exist, every visitor count is exact, and the
// response has to say so rather than leave the field empty. An empty
// visitor_counts is what a panel reads as "counted" - see
// internal/panel/analytics - so the difference between saying it and
// happening to be read that way is the difference between a contract and
// a coincidence.

package api

import (
	"context"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/scoring"
)

func TestStore_RealTimescaleDB_ASummaryAlwaysSaysHowItCounted(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-visitor-method"

	store := newTestStore(t, "t13d_method", []seedRow{
		{site: site, ip: "203.0.113.90", at: base, rate: 1, score: 5},
		{site: site, ip: "203.0.113.91", at: base.Add(time.Minute), rate: 1, score: 80},
	})

	from, to := base.Add(-time.Minute), base.Add(time.Hour)
	got, err := store.Summary(context.Background(), site, from, to, scoring.BotCutoff)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}

	if got.VisitorCounts != VisitorCountExact {
		t.Errorf("visitor_counts = %q, want %q. Two rows is far inside the exact "+
			"budget and this database has no sketches at all; there is no way for "+
			"this answer to be an estimate.", got.VisitorCounts, VisitorCountExact)
	}
	if got.VisitorCountError != 0 {
		t.Errorf("visitor_count_error = %v on an exact count. A margin printed beside "+
			"a counted figure tells a reader to distrust the one number on the page "+
			"that needs no distrust.", got.VisitorCountError)
	}
	if got.UniqueIPs != 2 || got.BotIPs != 1 || got.HumanIPs != 1 {
		t.Errorf("unique %d, bot %d, human %d; want 2, 1, 1",
			got.UniqueIPs, got.BotIPs, got.HumanIPs)
	}

	// And a budget of one row cannot turn it into an estimate, because
	// there is nothing to estimate from. This is the half that matters:
	// a read path that reached for a missing table would fail the
	// request rather than count, and a deployment without the extension
	// would see an error page where it used to see numbers.
	store.exactRows = 1
	got, err = store.Summary(context.Background(), site, from, to, scoring.BotCutoff)
	if err != nil {
		t.Fatalf("Summary over the budget on a database with no sketches: %v", err)
	}
	if got.VisitorCounts != VisitorCountExact {
		t.Errorf("with the budget exceeded and no sketch tables, visitor_counts = %q; "+
			"the only answer available here is the exact one", got.VisitorCounts)
	}
	if got.UniqueIPs != 2 {
		t.Errorf("unique = %d, want 2 - the numbers have to survive the fallback, "+
			"not merely be labelled correctly", got.UniqueIPs)
	}
}
