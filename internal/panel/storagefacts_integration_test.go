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
//
// # Why this compares two queries rather than writing rows
//
// The first version inserted twenty thousand rows and asserted the
// reported size grew. It caught the defect, and it was flaky: run inside
// the whole suite it grew by 120 KB rather than the 2.9 MB measured on a
// quiet database, because a database that has seen churn has free space
// in its existing chunks and most of an insert lands without allocating
// anything.
//
// Lowering the floor would only move the flake. The honest form of the
// assertion is the defect itself: the broken query returned exactly
// pg_total_relation_size, so the test asks for both and requires them to
// differ. That needs no writes, no vacuum, and no assumption about what
// else is running.
func TestAHypertableIsMeasuredByItsChunksAndNotItsParent(t *testing.T) {
	store := newTestStore(t, "storagefacts")
	ctx := context.Background()

	facts, err := store.StorageFacts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := factFor(t, facts, "traffic_snapshots")
	if !got.Hypertable {
		t.Skip("traffic_snapshots is not a hypertable in this database, so there is " +
			"nothing here to get wrong")
	}

	reader := testdb.Pool(t, testdb.SchemaAdmin)
	var parentOnly, rows int64
	if err := reader.QueryRow(ctx,
		`SELECT pg_total_relation_size('traffic_snapshots'),
		        (SELECT count(*) FROM traffic_snapshots)`).Scan(&parentOnly, &rows); err != nil {
		t.Fatal(err)
	}
	if rows == 0 {
		t.Skip("traffic_snapshots is empty, so the parent and the chunks are both nothing")
	}

	if got.Bytes == parentOnly {
		t.Errorf("traffic_snapshots holds %d rows and is reported as %d bytes, which is "+
			"exactly pg_total_relation_size of the parent.\n"+
			"A hypertable's rows are in chunks, in another schema. The parent never "+
			"grows, so this number never moves however full the disk gets - and it is "+
			"the number an operator reads to decide whether it is filling",
			rows, got.Bytes)
	}
	if got.Bytes < parentOnly {
		t.Errorf("the reported size (%d) is smaller than the parent alone (%d)",
			got.Bytes, parentOnly)
	}
}

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
