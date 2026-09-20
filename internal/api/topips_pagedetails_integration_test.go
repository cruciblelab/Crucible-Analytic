//go:build integration

// O4c: the highest-scoring addresses are chosen first and described
// second, so the window is not sorted to produce columns for addresses
// nobody asked about.
//
// Against a real TimescaleDB because the claim is about what a second
// read of the same window returns - which is a property of the SQL, not
// of any Go code here.

package api

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// Every address on a page carries its own description, on every page,
// and a row from outside the window does not describe it.
//
// # The two ways a second pass goes wrong
//
// Narrowed too far, it leaves rows with an empty fingerprint and no
// country - which reads as "nothing is known about this address"
// rather than as a defect, so it would ship. This asserts a full page
// as well as one row at a time: a pass that fetched the first row's
// address and stopped answers every page-of-one correctly.
//
// Unbounded in time, it describes an address by rows the page is not
// counting. The first address below has a second fingerprint and
// another country two hours earlier, outside the window, and the page
// still has to say one fingerprint.
func TestStore_RealTimescaleDB_EveryTopIPCarriesItsOwnDetails(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-o4c-sayfa"

	// The first address is seeded twice inside the window, with the
	// second row quieter and later. Without that, every aggregate in
	// the first pass is an identity - max of one value is min of one
	// value, a count of one row is one, and four mutations of them
	// survived a fixture that gave each address a single row.
	store := newTestStoreWithJA4(t, "", []seedRow{
		{site: site, ip: "203.0.113.80", at: base, rate: 3, score: 90,
			ja4: "t13d_o4c_bir", country: "TR", asn: 64496, asnOrg: "Bir"},
		{site: site, ip: "203.0.113.80", at: base.Add(2 * time.Minute), rate: 1,
			score: 40, ja4: "t13d_o4c_bir", country: "TR", asn: 64496, asnOrg: "Bir"},
		{site: site, ip: "203.0.113.81", at: base, rate: 2, score: 80,
			ja4: "t13d_o4c_iki", country: "DE", asn: 64497, asnOrg: "Iki"},
		{site: site, ip: "203.0.113.82", at: base, rate: 1, score: 70,
			ja4: "t13d_o4c_uc", country: "FR", asn: 64498, asnOrg: "Uc"},
		// Outside the window: same address, another answer.
		{site: site, ip: "203.0.113.80", at: base.Add(-2 * time.Hour), rate: 9,
			score: 99, ja4: "t13d_o4c_eski", country: "XX", asn: 64999,
			asnOrg: "Pencere disi"},
	})

	from, to := base.Add(-time.Minute), base.Add(time.Hour)
	want := map[string]struct {
		ja4     string
		country string
		asn     int
	}{
		"203.0.113.80": {"t13d_o4c_bir", "TR", 64496},
		"203.0.113.81": {"t13d_o4c_iki", "DE", 64497},
		"203.0.113.82": {"t13d_o4c_uc", "FR", 64498},
	}

	check := func(what string, stats []IPStat) {
		t.Helper()
		for _, got := range stats {
			w, ok := want[got.IP]
			if !ok {
				t.Errorf("%s: unexpected address %s", what, got.IP)
				continue
			}
			if got.JA4 != w.ja4 || got.Country != w.country || got.ASN != w.asn {
				t.Errorf("%s, address %s: fingerprint %q, country %q, ASN %d; "+
					"want %q, %q, %d.\nThe second pass has to cover every address "+
					"on the page it was asked for.", what, got.IP, got.JA4,
					got.Country, got.ASN, w.ja4, w.country, w.asn)
			}
			if got.JA4Count != 1 {
				t.Errorf("%s, address %s: %d distinct fingerprints, want 1 - a "+
					"second pass without the window would find the row two hours "+
					"earlier", what, got.IP, got.JA4Count)
			}
		}
	}

	// Walked one row at a time, and in order: with a limit that cuts,
	// the page has to start at the *highest* score. A first pass that
	// ordered the other way would hand the last three addresses to a
	// final sort that puts them back in the right order among
	// themselves - so the order of the whole list is not what shows it,
	// the choice of who is on page 1 is.
	inOrder := []string{"203.0.113.80", "203.0.113.81", "203.0.113.82"}
	for page := range 3 {
		stats, total, err := store.TopIPs(context.Background(), site, from, to, 1, page)
		if err != nil {
			t.Fatalf("TopIPs page %d: %v", page+1, err)
		}
		if len(stats) != 1 || total != 3 {
			t.Fatalf("page %d: %d row(s) with total %d, want 1 and 3",
				page+1, len(stats), total)
		}
		if stats[0].IP != inOrder[page] {
			t.Errorf("page %d of one holds %s, want %s - most suspicious first",
				page+1, stats[0].IP, inOrder[page])
		}
		check("one row at a time", stats)
	}

	all, total, err := store.TopIPs(context.Background(), site, from, to, 3, 0)
	if err != nil {
		t.Fatalf("TopIPs whole page: %v", err)
	}
	if len(all) != 3 || total != 3 {
		t.Fatalf("%d rows with total %d, want 3 and 3", len(all), total)
	}
	check("a three-row page", all)

	// The order is the phase's other half: the page is chosen in the
	// first pass and joined in the second, and a join does not preserve
	// order by itself.
	if all[0].IP != "203.0.113.80" || all[2].IP != "203.0.113.82" {
		t.Errorf("order = %s, %s, %s; want the highest score first",
			all[0].IP, all[1].IP, all[2].IP)
	}

	// The aggregates of the first pass, on the one address that has
	// more than one row in the window. Each of these is a mutation that
	// survived until this row existed: with a single row per address,
	// max is min and a count is one.
	top := all[0]
	if top.PeakScore != 90 {
		t.Errorf("PeakScore = %d, want 90 (the louder of its two rows)", top.PeakScore)
	}
	if top.PeakRequestRate != 3 {
		t.Errorf("PeakRequestRate = %v, want 3 - the peak, not the later or the "+
			"quieter value", top.PeakRequestRate)
	}
	if top.Snapshots != 2 {
		t.Errorf("Snapshots = %d, want 2: the address was seen twice inside the "+
			"window (a third time outside it, which must not count)", top.Snapshots)
	}
	if !top.LastSeen.Equal(base.Add(2 * time.Minute)) {
		t.Errorf("LastSeen = %s, want %s - the later of the two rows",
			top.LastSeen, base.Add(2*time.Minute))
	}
}

// A page comes back in the order it was chosen in, however many rows
// it holds.
//
// # Why this needs its own fixture and forty addresses
//
// Because the order is decided in the first pass and the rows then go
// through a join, and a join preserves nothing by itself. With three
// rows the mutation that deletes the final ORDER BY survives: the plan
// for a tiny page emits the outer side in order anyway, so the clause
// looks unnecessary until a plan changes and it silently is not.
//
// So the fixture is built to disagree with any order but the asked-for
// one: the addresses ascend while the scores descend, which means an
// answer that came back in address order - the order a hash join would
// hand back - is upside down.
func TestStore_RealTimescaleDB_ATopIPPageKeepsItsOrder(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-o4c-sira"

	const (
		addresses = 40
		perRow    = 5
	)
	rows := make([]seedRow, 0, addresses*perRow)
	for i := range addresses {
		for r := range perRow {
			rows = append(rows, seedRow{
				site: site,
				// 203.0.113.100 .. .139 ascending, scores 90 down to 51.
				ip:    "203.0.113." + strconv.Itoa(100+i),
				at:    base.Add(time.Duration(r) * time.Second),
				rate:  1,
				score: int16(90 - i),
				ja4:   "t13d_o4c_sira_" + strconv.Itoa(i),
			})
		}
	}
	store := newTestStoreWithJA4(t, "", rows)

	stats, total, err := store.TopIPs(context.Background(), site,
		base.Add(-time.Minute), base.Add(time.Hour), addresses, 0)
	if err != nil {
		t.Fatalf("TopIPs: %v", err)
	}
	if len(stats) != addresses || total != addresses {
		t.Fatalf("%d rows with total %d, want %d and %d",
			len(stats), total, addresses, addresses)
	}
	for i, got := range stats {
		if want := 90 - i; got.PeakScore != want {
			t.Fatalf("row %d has score %d, want %d - the page came back in some "+
				"other order than the one it was chosen in (row 0 is %s)",
				i, got.PeakScore, want, stats[0].IP)
		}
	}
}

// The second pass belongs to one site.
//
// The crossover twin of this test was written because a mutation
// survived: no fixture in this package had ever put one address on two
// sites, so a detail pass that ignored the site read nothing extra.
// This is the same query shape over the same table, so it gets the
// same input - the other site's values sort above this one's, which is
// what makes the assertion able to fail.
func TestStore_RealTimescaleDB_TopIPDetailsStayOnTheirOwnSite(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	const (
		mine   = "api-o4c-site-a"
		theirs = "api-o4c-site-b"
		shared = "203.0.113.90"
	)

	store := newTestStoreWithJA4(t, "", []seedRow{
		{site: mine, ip: shared, at: base, rate: 1, score: 60,
			ja4: "t13d_o4c_benim", country: "DE", asn: 64496, asnOrg: "Benim"},
		{site: theirs, ip: shared, at: base, rate: 1, score: 60,
			ja4: "t13d_o4c_baska", country: "TR", asn: 64497, asnOrg: "Baska"},
	})

	stats, total, err := store.TopIPs(context.Background(), mine,
		base.Add(-time.Minute), base.Add(time.Hour), 50, 0)
	if err != nil {
		t.Fatalf("TopIPs: %v", err)
	}
	if len(stats) != 1 || total != 1 {
		t.Fatalf("%d row(s) with total %d, want 1 and 1", len(stats), total)
	}
	got := stats[0]
	if got.JA4 != "t13d_o4c_benim" || got.JA4Count != 1 {
		t.Errorf("fingerprint = %q (%d distinct), want t13d_o4c_benim out of 1",
			got.JA4, got.JA4Count)
	}
	if got.Country != "DE" || got.ASN != 64496 {
		t.Errorf("country/ASN = %q/%d, want DE/64496; TR/64497 is the other "+
			"customer's answer", got.Country, got.ASN)
	}
}
