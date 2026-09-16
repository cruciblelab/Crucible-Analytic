//go:build integration

// O4a: a paginated breakdown's total is the total of what it pages.
//
// Against a real database, because the claim is about two SQL
// definitions of "how many" agreeing - and they agreed in Go only by
// accident before this.

package api

import (
	"context"
	"testing"
	"time"
)

// An address whose dimension changed inside the window counts once.
//
// # What was wrong
//
// Every paginated breakdown ran two queries: count(DISTINCT <column>)
// over the raw rows for the total, then a GROUP BY over one row per
// address for the page. Those are different questions whenever an
// address carried two values for the dimension in one window - and the
// page printed both answers side by side.
//
// Measured on the 12M-row set before the fix: the ASN breakdown said
// 855 and could page through 95. The whole API suite passed, because
// every fixture in it gave each address a single ASN and a single
// fingerprint, so the two definitions could not disagree. That is the
// shape of a test suite that cannot see a defect: not a missing
// assertion, a missing *input*.
//
// # Why an address's ASN changes at all
//
// Because the range dataset is refreshed (D3). The same address
// resolves to one ASN in June and another in September, and both rows
// stay in the window. The product is behaving correctly; it is the
// arithmetic on top that had two minds.
func TestStore_RealTimescaleDB_ABreakdownTotalCountsWhatItCanPage(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-pagetotal-asn"

	// One address, two ASNs, two countries - the dataset refreshed
	// between the two snapshots. max() picks one of each, so the
	// breakdown has exactly one row per dimension.
	store := newTestStore(t, "t13d_pagetotal", []seedRow{
		{site: site, ip: "203.0.113.60", at: base, rate: 1, score: 5,
			asn: 64496, asnOrg: "Eski", country: "TR"},
		{site: site, ip: "203.0.113.60", at: base.Add(time.Minute), rate: 1, score: 5,
			asn: 64497, asnOrg: "Yeni", country: "DE"},
	})

	from, to := base.Add(-time.Minute), base.Add(time.Hour)

	for _, tc := range []struct {
		name  string
		fetch func() (int, int, error) // rows, total
	}{
		{"asns", func() (int, int, error) {
			stats, total, err := store.ASNs(context.Background(), site, from, to, 50, 0, 50)
			return len(stats), total, err
		}},
		{"countries", func() (int, int, error) {
			stats, total, err := store.Countries(context.Background(), site, from, to, 50, 0, 50)
			return len(stats), total, err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, total, err := tc.fetch()
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if rows != 1 {
				t.Fatalf("%d rows, want 1: one address contributes one row", rows)
			}
			if total != rows {
				t.Errorf("the total is %d and %d row(s) can be paged through.\n"+
					"Before O4a this said 2: the total came from a second query "+
					"counting distinct values in the raw rows, while the page "+
					"counts one value per address. A total that is not the total "+
					"of the thing being paged sends somebody to an empty page with "+
					"no way to tell which number lied.", total, rows)
			}
		})
	}
}

// The same, for the fingerprint breakdown, which picks a representative
// rather than a maximum.
//
// Its own test because the mechanism differs: JA4s reduces an address's
// fingerprints to the flagged one, or the most recent (see
// representativeJA4, chosen deliberately in R2). So an address seen with
// two fingerprints is one row here too - and the old total counted both.
func TestStore_RealTimescaleDB_TheFingerprintTotalCountsWhatItCanPage(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-pagetotal-ja4"

	store := newTestStoreWithJA4(t, "", []seedRow{
		{site: site, ip: "203.0.113.61", at: base, rate: 1, score: 5, ja4: "t13d_once"},
		{site: site, ip: "203.0.113.61", at: base.Add(time.Minute), rate: 1, score: 5,
			ja4: "t13d_sonra"},
	})

	stats, total, err := store.JA4s(context.Background(), site,
		base.Add(-time.Minute), base.Add(time.Hour), 50, 0, 50)
	if err != nil {
		t.Fatalf("JA4s: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("%d rows, want 1 - one address, one representative fingerprint: %+v",
			len(stats), stats)
	}
	if total != 1 {
		t.Errorf("the total is %d and 1 row can be paged through; the old second "+
			"query counted both fingerprints", total)
	}
}

// A total survives paging, and an empty breakdown has a total of zero.
//
// The first is what the number is for: page 1 of 3 has to say 3, or
// there is no pager. It is also the half a window function gets wrong if
// it is placed after the LIMIT rather than before - so it is asserted
// with a limit smaller than the result.
//
// The second is the boundary the implementation has no row to carry:
// with no rows there is no window function output at all, so the total
// has to come back 0 rather than uninitialised or missing.
func TestStore_RealTimescaleDB_ABreakdownTotalSurvivesPaging(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-pagetotal-paging"

	store := newTestStore(t, "t13d_pagetotal_page", []seedRow{
		{site: site, ip: "203.0.113.70", at: base, rate: 1, score: 5, country: "TR"},
		{site: site, ip: "203.0.113.71", at: base, rate: 1, score: 5, country: "DE"},
		{site: site, ip: "203.0.113.72", at: base, rate: 1, score: 5, country: "FR"},
	})

	from, to := base.Add(-time.Minute), base.Add(time.Hour)

	// ---- one page of three ----
	stats, total, err := store.Countries(context.Background(), site, from, to, 1, 0, 50)
	if err != nil {
		t.Fatalf("Countries: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("a limit of 1 returned %d rows", len(stats))
	}
	if total != 3 {
		t.Errorf("total = %d on page 1 of 3, want 3.\n"+
			"A window function counts the rows the query would have returned "+
			"before LIMIT; a total that shrank to the page size would make every "+
			"breakdown look like it had exactly one page.", total)
	}

	// ---- the last page still carries it ----
	stats, total, err = store.Countries(context.Background(), site, from, to, 1, 2, 50)
	if err != nil {
		t.Fatalf("Countries page 3: %v", err)
	}
	if len(stats) != 1 || total != 3 {
		t.Errorf("page 3 returned %d rows with total %d, want 1 and 3", len(stats), total)
	}

	// ---- past the end: no rows, and the total is zero ----
	//
	// Not 3. There is no row to carry the number, and inventing one
	// would mean reporting a total for a page that does not exist.
	stats, total, err = store.Countries(context.Background(), site, from, to, 1, 99, 50)
	if err != nil {
		t.Fatalf("Countries past the end: %v", err)
	}
	if len(stats) != 0 || total != 0 {
		t.Errorf("past the end: %d rows, total %d; want 0 and 0", len(stats), total)
	}

	// ---- an empty window ----
	stats, total, err = store.Countries(context.Background(), site,
		base.Add(-48*time.Hour), base.Add(-47*time.Hour), 50, 0, 50)
	if err != nil {
		t.Fatalf("Countries over an empty window: %v", err)
	}
	if len(stats) != 0 || total != 0 {
		t.Errorf("an empty window: %d rows, total %d; want 0 and 0", len(stats), total)
	}
}
