package asnlookup

import (
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/ipsources"
)

// TestTheDefaultsAreStillTodaysFiles.
//
// M1's first done criterion, stated as a test: a deployment that touches
// nothing behaves exactly as it did before the library existed.
//
// The four URLs below are the literal constants that were in
// asnlookup.go, copied here once, deliberately. Everywhere else in this
// package they are derived from the library - which is right, and which
// is also why this is the one place a literal earns its keep: a derived
// value compared against another derived value would agree with itself
// no matter what the library said.
//
// What it catches is not a typo. It is somebody reordering the table,
// renaming an id, or "tidying" the default to a newer dataset - each of
// which would change what every existing installation downloads at its
// next refresh, silently, with no setting having been touched by anyone.
func TestTheDefaultsAreStillTodaysFiles(t *testing.T) {
	const before = "https://github.com/sapics/ip-location-db/releases/download/latest/"

	for _, want := range []struct{ got, expect, what string }{
		{countryIPv4URL, before + "user-country-ipv4.csv", "country IPv4 URL"},
		{countryIPv6URL, before + "user-country-ipv6.csv", "country IPv6 URL"},
		{asnIPv4URL, before + "origin-asn-ipv4.csv", "ASN IPv4 URL"},
		{asnIPv6URL, before + "origin-asn-ipv6.csv", "ASN IPv6 URL"},
		{countryIPv4Filename, "user-country-ipv4.csv", "country IPv4 filename"},
		{countryIPv6Filename, "user-country-ipv6.csv", "country IPv6 filename"},
		{asnIPv4Filename, "origin-asn-ipv4.csv", "ASN IPv4 filename"},
		{asnIPv6Filename, "origin-asn-ipv6.csv", "ASN IPv6 filename"},
	} {
		if want.got != want.expect {
			t.Errorf("the default %s is now %q, and before M1 it was %q.\n"+
				"Every installation that has chosen nothing would start fetching "+
				"something else at its next refresh, with nobody having changed a "+
				"setting", want.what, want.got, want.expect)
		}
	}
}

// TestSetSourcesTakesEffectAtTheNextRefresh.
//
// M1's second done criterion: change the choice, and the next refresh
// fetches the new dataset.
//
// Asserted on the URLs the refresh would use rather than by downloading
// anything, because the question here is which dataset was chosen, not
// whether GitHub is up. The download path itself is covered by the
// package's existing local-file and HTTP tests.
func TestSetSourcesTakesEffectAtTheNextRefresh(t *testing.T) {
	var r Resolver

	// Nothing set: today's files, which is the compatibility promise.
	if got := r.countrySource().IPv4URL; got != countryIPv4URL {
		t.Errorf("an unconfigured resolver would fetch %q, not the default %q",
			got, countryIPv4URL)
	}

	r.SetSources("server-country", "iptoasn-asn", nil)
	if got := r.countrySource().ID; got != "server-country" {
		t.Errorf("after choosing server-country the resolver still uses %q", got)
	}
	if got := r.asnSource().ID; got != "iptoasn-asn" {
		t.Errorf("after choosing iptoasn-asn the resolver still uses %q", got)
	}

	// And back to the default by clearing it, which is what the panel's
	// reset control does.
	r.SetSources("", "", nil)
	if got := r.countrySource().ID; got != ipsources.DefaultCountry {
		t.Errorf("clearing the choice left %q rather than the default", got)
	}
}

// TestAnUnknownChoiceFallsBackRatherThanFetchingNothing.
//
// The state after a binary is rolled back: the settings row names a
// dataset this build does not carry. Refusing would mean no country data
// at all, which is worse than a stale choice - so it falls back, and the
// warning is what tells the operator.
func TestAnUnknownChoiceFallsBackRatherThanFetchingNothing(t *testing.T) {
	var r Resolver
	r.SetSources("bir-sonraki-surumden", "", nil)
	if got := r.countrySource().ID; got != ipsources.DefaultCountry {
		t.Errorf("an unknown dataset id produced %q; it should fall back to %q so the "+
			"deployment keeps working", got, ipsources.DefaultCountry)
	}
}

// TestTheFallbackChainSkipsTheWrongKind.
//
// A country fallback list naming an ASN dataset is a real mistake, and
// the resolver must not try to parse four-column ASN rows with the
// three-column country parser: it would not error, it would produce
// entries whose country code is an AS number.
func TestTheFallbackChainSkipsTheWrongKind(t *testing.T) {
	var r Resolver
	r.SetSources("user-country", "", []string{"origin-asn", "iptoasn-country"})

	chain := r.chain(r.countrySource(), ipsources.KindCountry)
	for _, s := range chain {
		if s.Kind != ipsources.KindCountry {
			t.Errorf("the country chain includes %q, which is an ASN dataset; the "+
				"country parser would read its AS numbers as country codes", s.ID)
		}
	}
	if len(chain) != 2 {
		t.Errorf("the chain is %d long, want the chosen source plus the one usable "+
			"fallback", len(chain))
	}
}
