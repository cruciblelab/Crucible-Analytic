package web

import (
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
)

// Two registries hold the same set of address lists, in two packages,
// for two different jobs:
//
//	web.technicalLists       what the panel will draw, and whether the
//	                         list shows a client-side address
//	analytics.addressLists   what the API client knows how to fetch
//
// Neither is redundant - they carry different fields - but they must
// cover the same kinds, and nothing was checking that.
//
// analytics.KnownAddressList was written for exactly this ("so a panel
// can refuse an unknown segment before it reaches a URL") and had no
// caller anywhere, which is how a guard against drift becomes a thing
// that drifts.
//
// The failure it guards is quiet in both directions. A kind the panel
// draws but the client cannot fetch is a link to an error. A kind the
// client can fetch but the panel does not list is a 404 on a page that
// exists - and neither shows up until somebody clicks.

func TestTheTwoAddressListRegistriesCoverTheSameKinds(t *testing.T) {
	if len(technicalLists) == 0 {
		t.Fatal("the panel lists no address lists; this test would pass by comparing nothing")
	}

	for kind := range technicalLists {
		if !analytics.KnownAddressList(kind) {
			t.Errorf("the panel draws %q but the API client cannot fetch it: "+
				"the page renders and the request behind it fails", kind)
		}
	}

	// And the other direction, which needs the client's own list. It is
	// unexported, so the kinds are named here - the one place a hand
	// list is unavoidable, and it is checked against both maps rather
	// than standing in for either.
	for _, kind := range []analytics.AddressListKind{
		analytics.ListSilent,
		analytics.ListJSBots,
	} {
		if !analytics.KnownAddressList(kind) {
			t.Errorf("%q is named here but the API client does not know it; "+
				"this list has gone stale", kind)
		}
		if _, ok := technicalLists[kind]; !ok {
			t.Errorf("the API client can fetch %q and the panel does not list it: "+
				"a segment that 404s on a page that exists", kind)
		}
	}
}
