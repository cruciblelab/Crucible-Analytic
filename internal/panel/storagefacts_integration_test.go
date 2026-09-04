//go:build integration

// The size the health page shows, checked against a table being filled.
//
// # The defect this exists because of
//
// The storage section reported pg_total_relation_size for every table.
// For an ordinary table that is the answer. For a hypertable it is the
// size of the *parent*, and a hypertable's parent holds no rows - the
// data lives in chunks, in another schema.
//
// So the two tables the page exists to report on, the two the retention
// policy is about, the two that grow, were reported at four hundredths
// of a per cent of their real size. Measured on real data:
//
//	traffic_snapshots   parent 40 KB     hypertable 16 MB
//	beacon_events       parent 48 KB     hypertable 18 MB
//
// Nothing failed. The number was there, it was small, and small numbers
// on a storage page do not look like errors - they look like a quiet
// deployment. It shipped from B4 until F1b.
//
// # Why this test is written this way
//
// An assertion that the size is "about 16 MB" would be a copy of
// today's data. An assertion that it is greater than zero passes against
// the broken query, which returned 40960.
//
// So it writes rows and checks the number *moves*. Under the old query
// it does not move at all: the parent is 40 KB before and 40 KB after,
// however much goes in.

package panel

import (
	"context"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// factFor picks one table out of StorageFacts.
func factFor(t *testing.T, facts []StorageFact, table string) StorageFact {
	t.Helper()
	for _, f := range facts {
		if f.Table == table {
			return f
		}
	}
	t.Fatalf("StorageFacts said nothing about %s; it reported %d tables", table, len(facts))
	return StorageFact{}
}

// TestAHypertableIsMeasuredByItsChunksAndNotItsParent.
func TestAHypertableIsMeasuredByItsChunksAndNotItsParent(t *testing.T) {
	store := newTestStore(t, "storagefacts")
	ctx := context.Background()

	before, err := store.StorageFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := factFor(t, before, "traffic_snapshots")
	if !start.Hypertable {
		t.Skip("traffic_snapshots is not a hypertable in this database, so there is " +
			"nothing here to get wrong")
	}

	// Twenty thousand rows spread over six hours, in one statement.
	//
	// Both numbers are measured rather than picked. Four thousand rows
	// within four seconds of each other moved the reported size by
	// *zero* bytes: they landed in one chunk and fitted in pages that
	// earlier deletions had already freed. A test written that way fails
	// against correct code, on a database that has seen any churn, which
	// is every database this suite runs on.
	//
	// Spread across chunks and past the free space, the same measurement
	// moved 2.9 MB. So the assertion below is about a number that
	// actually has to move.
	writer := testdb.Pool(t, testdb.Collector)
	if _, err := writer.Exec(ctx, `
		INSERT INTO traffic_snapshots
		    (time, site_id, ip, ja4, prev_window_count, curr_window_count,
		     request_rate, bot_score, is_known_bot_ja4,
		     country, asn, asn_org, is_known_bot_asn)
		SELECT now() - (g || ' seconds')::interval,
		       $1,
		       ('198.51.100.' || (g % 256))::inet,
		       't13d1516h2_8daaf6152771_b186095e22b6',
		       0, 0, 0, 0, false, '', 0, '', false
		FROM generate_series(1, 20000) AS g`, measurementSite); err != nil {
		t.Fatalf("writing the measurement rows: %v", err)
	}
	// Cleared as schema_admin: the collector may insert and nothing else,
	// which is the isolation working. A cleanup that ran as the writer
	// would fail, and a failed cleanup leaves twenty thousand rows in a
	// shared database for every later test to trip over.
	t.Cleanup(func() {
		admin := testdb.Pool(t, testdb.SchemaAdmin)
		if _, err := admin.Exec(context.Background(),
			`DELETE FROM traffic_snapshots WHERE site_id = $1`, measurementSite); err != nil {
			t.Errorf("clearing the measurement rows: %v", err)
		}
	})

	after, err := store.StorageFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	end := factFor(t, after, "traffic_snapshots")

	if end.Bytes <= start.Bytes {
		t.Errorf("twenty thousand rows went into traffic_snapshots and the reported size "+
			"went from %d to %d bytes.\n"+
			"A hypertable's parent never grows - the rows go into chunks - so a size "+
			"that does not move is a size being read off the wrong relation. That is "+
			"the number an operator uses to decide whether the disk is filling",
			start.Bytes, end.Bytes)
	}
	// And by the right order of magnitude. Measured at 2.9 MB for this
	// many rows; a megabyte is a floor well under it and well over
	// anything that could be noise.
	if grew := end.Bytes - start.Bytes; grew < 1<<20 {
		t.Errorf("the reported size grew by only %d bytes for twenty thousand rows", grew)
	}
}

// measurementSite is the site id the rows above carry, so the cleanup
// can find exactly them and nothing else in a shared database.
const measurementSite = "depolama-olcum"

// TestAnOrdinaryTableIsStillMeasured is the other half.
//
// The fix reaches for hypertable_detailed_size, which returns nothing
// for a table that is not a hypertable. A version that used it
// unconditionally would report every ordinary table as zero bytes, and
// zero reads as "empty" rather than as "not measured".
func TestAnOrdinaryTableIsStillMeasured(t *testing.T) {
	store := newTestStore(t, "storagefacts-ordinary")

	facts, err := store.StorageFacts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f := factFor(t, facts, "ip_asn_ranges")
	if f.Hypertable {
		t.Skip("ip_asn_ranges is a hypertable here, which this test is not about")
	}
	if f.Bytes <= 0 {
		t.Errorf("ip_asn_ranges is reported as %d bytes. An existing table has a size, "+
			"and zero on this page reads as empty rather than as unmeasured", f.Bytes)
	}
}
