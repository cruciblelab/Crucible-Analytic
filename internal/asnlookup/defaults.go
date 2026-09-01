package asnlookup

import "github.com/cruciblelab/crucible-analytic/internal/ipsources"

// The default datasets' URLs and filenames, derived from the library
// rather than written out again.
//
// They exist under these names because the defaults are a compatibility
// promise - every installation made before M1 is fetching exactly these
// four files - and deriving them is what makes the promise checkable
// instead of merely stated: TestTheDefaultsAreStillTodaysFiles compares
// them against the literal URLs that were in asnlookup.go before the
// library existed.
//
// A second copy would have made that test compare a constant with
// itself.
var (
	countryIPv4URL      = mustSource(ipsources.DefaultCountry).IPv4URL
	countryIPv6URL      = mustSource(ipsources.DefaultCountry).IPv6URL
	countryIPv4Filename = mustSource(ipsources.DefaultCountry).IPv4File
	countryIPv6Filename = mustSource(ipsources.DefaultCountry).IPv6File

	asnIPv4URL      = mustSource(ipsources.DefaultASN).IPv4URL
	asnIPv6URL      = mustSource(ipsources.DefaultASN).IPv6URL
	asnIPv4Filename = mustSource(ipsources.DefaultASN).IPv4File
	asnIPv6Filename = mustSource(ipsources.DefaultASN).IPv6File
)

// mustSource panics for an id the library does not have.
//
// At package initialisation, so it is a build-time-ish failure rather
// than a runtime one: the only way to reach it is a default that names
// nothing, which is a mistake in ipsources and should stop the binary
// starting rather than quietly leave a deployment with no country data.
func mustSource(id string) ipsources.Source {
	s, ok := ipsources.ByID(id)
	if !ok {
		panic("asnlookup: default source " + id + " is not in the library")
	}
	return s
}
