//go:build integration

// O4b: the crossover queries form their join key once per address
// instead of once per row, and fetch a page's detail columns for that
// page only. Neither may change an answer.
//
// Against a real TimescaleDB because every claim here is about what
// PostgreSQL does with two groupings and a filter - none of it is
// visible from Go, and the one-level form these replace was correct.

package api

import (
	"context"
	"testing"
	"time"
)

// twoNetworkToken is one token written with two masked networks.
//
// The product cannot produce this: privacy.TokenIP hashes the whole
// address and privacy.MaskIP derives the network from that same
// address, so a token determines its network. A fixture can, and it has
// to - because the second grouping level is *only* observable when one
// key arrives as more than one (ip_hash, ip) pair. With one pair per
// key, max-of-maxima and sum-of-counts are identities and every
// mutation of them survives.
//
// So this is the row that makes the merge measurable, and it is
// deliberately a row no writer would write.
var twoNetworkToken = []byte("iki-agli-jetonx")

func twoNetworkRows(site string, base time.Time) []seedRow {
	return []seedRow{
		// Same token, first network: the lower score, the earlier time,
		// the unflagged fingerprint.
		{site: site, ip: "203.0.113.10", ipHash: twoNetworkToken, at: base,
			rate: 1, score: 30, ja4: "t13d_2ag_bir", country: "TR",
			asn: 64496, asnOrg: "Bir"},
		// Same token, second network: the higher score, the later time,
		// the flagged fingerprint - so every merge below has a wrong
		// answer available to it.
		{site: site, ip: "203.0.113.11", ipHash: twoNetworkToken,
			at: base.Add(time.Minute), rate: 2, score: 70, ja4: "t13d_2ag_iki",
			botJA4: true, country: "ZZ", asn: 64497, asnOrg: "Iki"},
	}
}

// One key is one address however many networks its rows carry, and the
// aggregates come back merged rather than halved.
//
// # What each assertion is holding
//
//   - One row: the second grouping level exists. Without it the two
//     pairs are two addresses, and a reader pages through a list where
//     one visitor appears twice.
//   - PeakScore, LastSeen, Snapshots: the merge is max, max and *sum*.
//     A count of counts that stayed a count would say 1.
//   - Country/ASN/JA4Count: the detail pass looked at both networks. It
//     filters on ip, from the array the grouping collected, so a
//     version of it that probed one network - the page's own ip, say -
//     reports the first network's country and one fingerprint.
func TestStore_RealTimescaleDB_ATokenSeenAsTwoNetworksIsOneSilentAddress(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-o4b-silent"

	store := newTestStoreWithJA4(t, "", twoNetworkRows(site, base))
	from, to := base.Add(-time.Minute), base.Add(time.Hour)

	silent, total, err := store.SilentIPs(context.Background(), site, from, to, 50, 0)
	if err != nil {
		t.Fatalf("SilentIPs: %v", err)
	}
	if len(silent) != 1 || total != 1 {
		t.Fatalf("%d row(s) with total %d, want 1 and 1 - one token is one "+
			"address, and the regrouping by the join key is what merges the "+
			"two networks it was written with: %+v", len(silent), total, silent)
	}

	got := silent[0]
	if got.PeakScore != 70 {
		t.Errorf("PeakScore = %d, want 70 (max of 30 and 70): the second level "+
			"has to take the max of the first level's maxima", got.PeakScore)
	}
	if got.PeakRequestRate != 2 {
		t.Errorf("PeakRequestRate = %v, want 2", got.PeakRequestRate)
	}
	if got.Snapshots != 2 {
		t.Errorf("Snapshots = %d, want 2: a count of counts is their sum, and "+
			"a max here would report one of the two", got.Snapshots)
	}
	if !got.LastSeen.Equal(base.Add(time.Minute)) {
		t.Errorf("LastSeen = %s, want %s (the later of the two)",
			got.LastSeen, base.Add(time.Minute))
	}
	if got.IP != "203.0.113.11" {
		t.Errorf("IP = %q, want the max of the two networks", got.IP)
	}
	if got.JA4Count != 2 || got.JA4 != "t13d_2ag_iki" || !got.IsKnownBotJA4 {
		t.Errorf("fingerprint = %q (%d distinct, flagged=%v), want the flagged "+
			"t13d_2ag_iki out of 2.\nThe detail pass reads the rows of every "+
			"network this key was seen with; one that read a single network "+
			"would report 1 distinct fingerprint and could miss the flag "+
			"entirely - which is the verdict the page prints.",
			got.JA4, got.JA4Count, got.IsKnownBotJA4)
	}
	if got.Country != "ZZ" || got.ASN != 64497 {
		t.Errorf("country/ASN = %q/%d, want ZZ/64497 (max over both networks)",
			got.Country, got.ASN)
	}

	// And the summary, which groups the same rows twice for its own
	// reasons and hands the merged score to the bands. One address in
	// one band, not two addresses in two - and the band is the one the
	// higher score belongs to, so a merge that took the lower would put
	// this address three bands down from where it is.
	sum, err := store.CrossoverSummary(context.Background(), site, from, to)
	if err != nil {
		t.Fatalf("CrossoverSummary: %v", err)
	}
	if sum.IPsSeen != 1 {
		t.Errorf("IPsSeen = %d, want 1: the summary regroups by the key too",
			sum.IPsSeen)
	}
	if sum.Bands[7].IPsSeen != 1 || sum.Bands[3].IPsSeen != 0 {
		t.Errorf("bands 70-79 / 30-39 hold %d / %d addresses, want 1 / 0 - the "+
			"band comes from the merged peak score (70), not from either of the "+
			"rows it was merged from", sum.Bands[7].IPsSeen, sum.Bands[3].IPsSeen)
	}
}

// The same address on the other list, which reaches it through the
// beacon and therefore through a different join.
func TestStore_RealTimescaleDB_ATokenSeenAsTwoNetworksIsOneJSBot(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-o4b-jsbot"

	store := seedBeacon(t, []beaconSeed{
		// The beacon heard the token at the *first* network only. The
		// detail pass must still read both, because the networks it
		// looks at come from the collector's own grouping rather than
		// from the address this page happens to print.
		{site: site, visitor: "v1", at: base, ip: "203.0.113.10", path: "/",
			ipHash: twoNetworkToken, browser: "Chrome", os: "Windows"},
	})
	seedSnapshotsFor(t, twoNetworkRows(site, base))

	from, to := base.Add(-time.Minute), base.Add(time.Hour)
	bots, total, err := store.JSBots(context.Background(), site, from, to, 50, 0, DefaultBotScoreMin)
	if err != nil {
		t.Fatalf("JSBots: %v", err)
	}
	if len(bots) != 1 || total != 1 {
		t.Fatalf("%d row(s) with total %d, want 1 and 1: %+v", len(bots), total, bots)
	}

	got := bots[0]
	if got.PeakScore != 70 {
		t.Errorf("PeakScore = %d, want 70 - and this one also decides whether "+
			"the address is on the list at all, since the filter is the score",
			got.PeakScore)
	}
	if got.JA4Count != 2 || got.JA4 != "t13d_2ag_iki" || !got.IsKnownBotJA4 {
		t.Errorf("fingerprint = %q (%d distinct, flagged=%v), want the flagged "+
			"t13d_2ag_iki out of 2", got.JA4, got.JA4Count, got.IsKnownBotJA4)
	}
	if got.Country != "ZZ" || got.ASN != 64497 {
		t.Errorf("country/ASN = %q/%d, want ZZ/64497", got.Country, got.ASN)
	}
}

// Every row on a page carries its own details, and a later page carries
// the later rows'.
//
// This is the half the split into two passes can get wrong in the
// direction that matters: a detail pass narrowed to fewer addresses
// than the page shows leaves rows with empty fingerprints and no
// country, which reads as "nothing known about this address" rather
// than as a defect. Three addresses, three fingerprints, three
// countries, and the page is walked one row at a time so that the
// second page has to fetch its own.
func TestStore_RealTimescaleDB_EveryRowOfACrossoverPageCarriesItsOwnDetails(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-o4b-sayfa"

	store := newTestStoreWithJA4(t, "", []seedRow{
		{site: site, ip: "203.0.113.20", at: base, rate: 3, score: 90,
			ja4: "t13d_sayfa_bir", country: "TR", asn: 64496, asnOrg: "Bir"},
		{site: site, ip: "203.0.113.21", at: base, rate: 2, score: 80,
			ja4: "t13d_sayfa_iki", country: "DE", asn: 64497, asnOrg: "Iki"},
		{site: site, ip: "203.0.113.22", at: base, rate: 1, score: 70,
			ja4: "t13d_sayfa_uc", country: "FR", asn: 64498, asnOrg: "Uc"},
		// The same address two hours before the window, resolved to
		// another country with another fingerprint. The detail pass
		// carries the window like the grouping pass does, and this row
		// is how that is measured: without it, a detail pass that asked
		// about an address rather than about an address *in a window*
		// would answer every assertion here correctly.
		{site: site, ip: "203.0.113.20", at: base.Add(-2 * time.Hour), rate: 9,
			score: 99, ja4: "t13d_sayfa_eski", country: "XX", asn: 64999,
			asnOrg: "Pencere disi"},
	})
	from, to := base.Add(-time.Minute), base.Add(time.Hour)

	want := map[string]struct {
		ja4     string
		country string
		asn     int
	}{
		"203.0.113.20": {"t13d_sayfa_bir", "TR", 64496},
		"203.0.113.21": {"t13d_sayfa_iki", "DE", 64497},
		"203.0.113.22": {"t13d_sayfa_uc", "FR", 64498},
	}

	for page := range 3 {
		rows, total, err := store.SilentIPs(context.Background(), site, from, to, 1, page)
		if err != nil {
			t.Fatalf("SilentIPs page %d: %v", page+1, err)
		}
		if len(rows) != 1 {
			t.Fatalf("page %d returned %d rows, want 1", page+1, len(rows))
		}
		if total != 3 {
			t.Errorf("page %d of 3 says total %d, want 3: the total counts the "+
				"addresses the query would have returned unpaginated",
				page+1, total)
		}
		got, w := rows[0], want[rows[0].IP]
		if got.JA4 != w.ja4 || got.Country != w.country || got.ASN != w.asn {
			t.Errorf("page %d, address %s: fingerprint %q, country %q, ASN %d; "+
				"want %q, %q, %d.\nThe detail pass has to cover every address on "+
				"the page it was asked for, not the first one and not the first "+
				"page.", page+1, got.IP, got.JA4, got.Country, got.ASN,
				w.ja4, w.country, w.asn)
		}
	}

	// And all three at once, which is the case a page-of-one cannot
	// see: a detail pass that fetched the first row's networks and
	// stopped would answer every one-row page correctly and leave rows
	// two and three of this one blank.
	all, total, err := store.SilentIPs(context.Background(), site, from, to, 3, 0)
	if err != nil {
		t.Fatalf("SilentIPs whole page: %v", err)
	}
	if len(all) != 3 || total != 3 {
		t.Fatalf("%d rows with total %d, want 3 and 3", len(all), total)
	}
	for _, got := range all {
		w := want[got.IP]
		if got.JA4 != w.ja4 || got.Country != w.country || got.ASN != w.asn {
			t.Errorf("on a three-row page, address %s: fingerprint %q, country "+
				"%q, ASN %d; want %q, %q, %d", got.IP, got.JA4, got.Country,
				got.ASN, w.ja4, w.country, w.asn)
		}
		// One fingerprint each - and for the first address that is the
		// window talking: it has a second one two hours earlier.
		if got.JA4Count != 1 {
			t.Errorf("address %s: %d distinct fingerprints in the window, want 1 "+
				"- a detail pass without the window would find the row before it",
				got.IP, got.JA4Count)
		}
	}

	// Past the end: no rows, and no total to report for a page that does
	// not exist - the boundary the window function has no row to carry.
	rows, total, err := store.SilentIPs(context.Background(), site, from, to, 1, 99)
	if err != nil {
		t.Fatalf("SilentIPs past the end: %v", err)
	}
	if len(rows) != 0 || total != 0 {
		t.Errorf("past the end: %d rows, total %d; want 0 and 0", len(rows), total)
	}
}

// The detail pass belongs to one site, and the address it is asked
// about is not enough to keep it there.
//
// # Why this test exists at all
//
// A mutation wrote it. Removing `site_id = $1` from the detail pass
// left every test in this package passing, because no fixture here had
// ever put the same address on two sites - the addresses were unique
// per test, so a query that ignored the site read nothing extra. That
// is the shape of a suite that cannot see a defect: not a missing
// assertion, a missing input.
//
// And the defect it could not see is the one the product is least
// allowed to have. Two customers behind one deployment share this
// table; an address that visits both - a crawler, a monitoring service,
// anything - would carry the other customer's fingerprint, country and
// ASN onto this page.
func TestStore_RealTimescaleDB_ACrossoverPagesDetailsStayOnItsOwnSite(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	const (
		mine   = "api-o4b-site-a"
		theirs = "api-o4b-site-b"
		shared = "203.0.113.40"
	)

	// The other site's values sort *above* this one's, so a detail pass
	// that read both would report theirs: max('DE','TR') is 'TR', and
	// max of the two ASNs is the other one.
	store := newTestStoreWithJA4(t, "", []seedRow{
		{site: mine, ip: shared, at: base, rate: 1, score: 60,
			ja4: "t13d_capraz_benim", country: "DE", asn: 64496, asnOrg: "Benim"},
		{site: theirs, ip: shared, at: base, rate: 1, score: 60,
			ja4: "t13d_capraz_baska", country: "TR", asn: 64497, asnOrg: "Baska"},
	})

	from, to := base.Add(-time.Minute), base.Add(time.Hour)
	silent, total, err := store.SilentIPs(context.Background(), mine, from, to, 50, 0)
	if err != nil {
		t.Fatalf("SilentIPs: %v", err)
	}
	if len(silent) != 1 || total != 1 {
		t.Fatalf("%d row(s) with total %d, want 1 and 1", len(silent), total)
	}
	got := silent[0]
	if got.JA4 != "t13d_capraz_benim" || got.JA4Count != 1 {
		t.Errorf("fingerprint = %q (%d distinct), want t13d_capraz_benim out of 1 "+
			"- the other site's row is for the same address and must not be read",
			got.JA4, got.JA4Count)
	}
	if got.Country != "DE" || got.ASN != 64496 {
		t.Errorf("country/ASN = %q/%d, want DE/64496; TR/64497 is the other "+
			"customer's answer", got.Country, got.ASN)
	}
}

// The JS-bot list's total is the total of what it pages, and it says so
// on every page.
//
// Its own test because it is a different query with a different filter:
// an address is on this list for either of two reasons, and the total
// used to come from a second execution of the whole CTE. Two executions
// of one definition agreed, which is why this is about the pass that
// was saved - but the number is what a pager reads, so it is asserted
// where it is now produced.
func TestStore_RealTimescaleDB_TheJSBotTotalCountsWhatItCanPage(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-o4b-jsbot-total"

	store := seedBeacon(t, []beaconSeed{
		{site: site, visitor: "v1", at: base, ip: "203.0.113.30", path: "/"},
		{site: site, visitor: "v2", at: base, ip: "203.0.113.31", path: "/"},
		// Not automation by either test: no bot user agent, and the
		// collector scored it below the cutoff. Without a row like this
		// the total would equal the number of beacon addresses whatever
		// the filter did.
		{site: site, visitor: "v3", at: base, ip: "203.0.113.32", path: "/"},
	})
	seedSnapshotsFor(t, []seedRow{
		{site: site, ip: "203.0.113.30", at: base, rate: 1, score: 90, ja4: "t13d_tot_bir"},
		{site: site, ip: "203.0.113.31", at: base, rate: 1, score: 80, ja4: "t13d_tot_iki"},
		{site: site, ip: "203.0.113.32", at: base, rate: 1, score: 5, ja4: "t13d_tot_uc"},
	})

	from, to := base.Add(-time.Minute), base.Add(time.Hour)
	for page := range 2 {
		bots, total, err := store.JSBots(context.Background(), site, from, to, 1, page,
			DefaultBotScoreMin)
		if err != nil {
			t.Fatalf("JSBots page %d: %v", page+1, err)
		}
		if len(bots) != 1 {
			t.Fatalf("page %d returned %d rows, want 1", page+1, len(bots))
		}
		if total != 2 {
			t.Errorf("page %d says total %d, want 2 - three addresses ran JS and "+
				"two of them look automated", page+1, total)
		}
	}
}
