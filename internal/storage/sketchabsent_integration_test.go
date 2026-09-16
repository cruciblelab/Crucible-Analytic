//go:build integration

// O3's other half: a deployment whose database has no sketch at all.
//
// # Why this is a measurement and not a fallback nobody exercises
//
// The visitor sketches need timescaledb_toolkit, which an existing
// deployment will not have and which some operators will never install.
// That is a supported state, not a degraded one: the read path counts
// distinct addresses exactly, the way the product did before this phase,
// and the response says so.
//
// A state described in a comment and never run is a state that breaks.
// So the rule is asserted against whatever database the suite is pointed
// at, in both directions: the sketch tables exist exactly when the
// extension does, and the refresh reports itself unavailable exactly
// when they do not. Nothing is skipped and nothing is simulated - on the
// shared development database the extension is absent, so this is the
// absent branch; on a cluster that has it, the same test measures the
// present one.

package storage

import (
	"context"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

func TestSketch_RealTimescaleDB_TheSketchExistsExactlyWhereTheExtensionDoes(t *testing.T) {
	ctx := context.Background()
	pool := testdb.Pool(t, testdb.Collector)

	var installed, tableThere, stateThere bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_extension
		                WHERE extname = 'timescaledb_toolkit'),
		       to_regclass('visitor_sketch') IS NOT NULL,
		       to_regclass('visitor_sketch_state') IS NOT NULL`,
	).Scan(&installed, &tableThere, &stateThere); err != nil {
		t.Fatalf("asking what this database has: %v", err)
	}

	// The schema's conditional CREATE TABLE, measured rather than read.
	//
	// Both directions matter and they fail differently. A table without
	// the extension cannot happen - the type would not exist - but a
	// *missing* table on a database that has the extension is the whole
	// feature silently absent, and the only symptom would be a
	// dashboard that stayed slow.
	if tableThere != installed || stateThere != installed {
		t.Fatalf("timescaledb_toolkit installed = %v, but visitor_sketch = %v and "+
			"visitor_sketch_state = %v.\n"+
			"internal/storage/schema.sql creates both exactly where the extension is "+
			"present. A database with the extension and no tables is O3 switched off "+
			"with nothing saying so; a database with tables and no extension should "+
			"not be reachable at all.", installed, tableThere, stateThere)
	}

	sketch := NewSketch(pool)
	report, err := sketch.Refresh(ctx, "storage-sketch-absent")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if report.Unavailable == installed {
		t.Errorf("the refresh reported Unavailable = %v on a database whose extension "+
			"is installed = %v. The collector logs this once per process and the read "+
			"path uses the same fact to decide whether to estimate; the two disagreeing "+
			"means one of them is guessing.", report.Unavailable, installed)
	}
	if report.Unavailable && report.Materialized() {
		t.Error("a report that is both unavailable and materialized. Materialized() is " +
			"what the collector's log line branches on.")
	}

	// And pruning is a no-op rather than an error, so a retention cycle
	// on a deployment without the extension does not spend its warning
	// budget on a table that was never meant to exist. Reached only when
	// the tables are absent; where they exist, the sketch suite in
	// internal/api measures the prune against real rows.
	if !installed {
		n, err := sketch.Prune(ctx, "storage-sketch-absent", 30)
		if err != nil {
			t.Errorf("Prune on a database with no sketch returned %v; the retention "+
				"cycle would log a warning every hour about a table this deployment "+
				"is not supposed to have", err)
		}
		if n != 0 {
			t.Errorf("Prune removed %d rows from a table that does not exist", n)
		}
	}
}
