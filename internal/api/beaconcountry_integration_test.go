//go:build integration

// O4e: the country breakdown answers in one pass, and probes the
// collector only for the events that have no country of their own.
//
// Against a real TimescaleDB because both claims are about what one SQL
// statement returns - a window count taken before LIMIT, and a LATERAL
// whose input set changed.

package api

import (
	"context"
	"testing"
	"time"
)

// countryFixture seeds three addresses whose beacon rows differ in
// whether they already carry a country, plus a collector row for each
// that says something else.
//
// # Why the third address exists
//
// The probe set is now the addresses with a row that has no country,
// rather than every address in the window. Narrowing it to addresses
// with *no country at all* would be the same thing on the first two
// addresses and wrong on the third, which has one row of each kind -
// and an address that was resolved in June and not in July is the
// ordinary case, not a contrived one: the beacon's geo lookup is a
// setting an operator turns on.
//
// So the third address is what makes "the condition is on rows" a
// measurable claim rather than a sentence in a comment.
func countryFixture(t *testing.T, site string, base time.Time) *Store {
	t.Helper()

	// The other customer, on the same address. Written because a
	// mutation survived without it: dropping `t.site_id = $1` from the
	// probe broke nothing, since no fixture here had ever put one
	// address on two sites. What was missing was an input, not an
	// assertion - and the defect it hides is the one this product has
	// least right to (one table, two customers).
	//
	// Its row is later than ours, so a probe that lost the site filter
	// would prefer it: the probe takes the most recent row.
	neighbour := site + "-komsu"

	store := seedBeacon(t, []beaconSeed{
		// A: the beacon resolved it. The collector disagrees, and must
		// not win - this is the COALESCE order the narrowing rests on.
		{site: site, visitor: "v-a", at: base, path: "/a",
			ip: "203.0.113.40", country: "TR"},
		// B: the beacon has nothing. The collector is the only source.
		{site: site, visitor: "v-b", at: base.Add(time.Minute), path: "/b",
			ip: "203.0.113.41", country: ""},
		// C: one row of each, same address.
		{site: site, visitor: "v-c", at: base.Add(2 * time.Minute), path: "/c",
			ip: "203.0.113.42", country: "NL"},
		{site: site, visitor: "v-c", at: base.Add(3 * time.Minute), path: "/c",
			ip: "203.0.113.42", country: ""},
		// Listed so the helper clears this site too, before and after.
		{site: neighbour, visitor: "v-komsu", at: base, path: "/",
			ip: "203.0.113.41", country: "XX"},
	})

	// The collector's side, written after seedBeacon because that helper
	// clears the sites - including traffic_snapshots - before it seeds.
	insertSnapshots(t, []seedRow{
		{site: site, ip: "203.0.113.40", at: base, rate: 1, score: 1, country: "DE"},
		{site: site, ip: "203.0.113.41", at: base, rate: 1, score: 1, country: "FR"},
		{site: site, ip: "203.0.113.42", at: base, rate: 1, score: 1, country: "ES"},
		// Same address, other customer, later row, different answer.
		{site: neighbour, ip: "203.0.113.41", at: base.Add(10 * time.Minute),
			rate: 1, score: 1, country: "XX"},
	}, func(r seedRow) string { return "t13d_o4e_ulke" })

	return store
}

// Every event lands under the country its own row resolves to.
func TestStore_RealTimescaleDB_ACountryComesFromTheCollectorOnlyForEventsWithoutOne(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-o4e-ulke"

	store := countryFixture(t, site, base)
	from, to := base.Add(-time.Minute), base.Add(time.Hour)

	countries, total, err := store.BeaconCountries(context.Background(), site,
		testBeaconParams(from, to))
	if err != nil {
		t.Fatalf("BeaconCountries: %v", err)
	}

	got := map[string]int{}
	for _, c := range countries {
		got[c.Key] = c.Pageviews
	}
	want := map[string]int{
		"TR": 1, // the beacon's own answer, not the collector's DE
		"FR": 1, // recovered, because the beacon had none
		"NL": 1, // the third address's resolved row
		"ES": 1, // the third address's unresolved row, recovered
	}
	for key, n := range want {
		if got[key] != n {
			t.Errorf("country %q has %d pageview(s), want %d.\nWhole answer: %+v\n"+
				"ES missing means the probe set was narrowed to addresses with no "+
				"country at all rather than to rows without one; DE present means "+
				"the collector overrode a country the beacon already had.",
				key, got[key], n, got)
		}
	}
	if _, ok := got["DE"]; ok {
		t.Errorf("DE is in the answer: the collector's country won over the "+
			"beacon's own.\nWhole answer: %+v", got)
	}
	if _, ok := got[""]; ok {
		t.Errorf("an empty country group came back: an address the probe should "+
			"have resolved was left unresolved.\nWhole answer: %+v", got)
	}
	if total != 4 {
		t.Errorf("total = %d, want 4 distinct countries", total)
	}
}

// The total counts every country, not the ones that fit on the page.
//
// Its own test rather than a line in the one above, because the total
// now comes from a window function inside the paged query: it is the
// count of groups *before* LIMIT, and a page that does not cut cannot
// tell that apart from a count of the rows returned.
func TestStore_RealTimescaleDB_ACountryPageCountsEveryCountry(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-o4e-ulke-sayfa"

	store := countryFixture(t, site, base)
	from, to := base.Add(-time.Minute), base.Add(time.Hour)

	p := testBeaconParams(from, to)
	p.limit = 1
	countries, total, err := store.BeaconCountries(context.Background(), site, p)
	if err != nil {
		t.Fatalf("BeaconCountries: %v", err)
	}
	if len(countries) != 1 {
		t.Fatalf("a page of one returned %d rows: %+v", len(countries), countries)
	}
	if total != 4 {
		t.Errorf("total = %d for a page of 1, want 4 - somebody paging to the end "+
			"of this list has to find four countries, not one", total)
	}

	// And the rest of the list is reachable, so the total is not merely
	// a bigger number than the page.
	p.offset = 3
	last, lastTotal, err := store.BeaconCountries(context.Background(), site, p)
	if err != nil {
		t.Fatalf("BeaconCountries page 4: %v", err)
	}
	if len(last) != 1 || lastTotal != 4 {
		t.Errorf("the fourth page holds %d row(s) with total %d, want 1 and 4",
			len(last), lastTotal)
	}
}
