package ipsources

import (
	"strings"
	"testing"
)

// TestEverySourceIsUsableByThisBuild.
//
// An entry is a promise that this binary can fetch and parse the thing.
// Four ways to break that promise without any test noticing, so all four
// are checked: no URL, no local filename, an empty id, and a duplicate
// id - the last being the one that would silently shadow another source
// in SourceByID.
func TestEverySourceIsUsableByThisBuild(t *testing.T) {
	all := All()
	if len(all) == 0 {
		t.Fatal("the library is empty; every check here would pass by looking at nothing")
	}

	seen := map[string]bool{}
	for _, s := range all {
		switch {
		case s.ID == "":
			t.Errorf("a source has no id; the settings row would store an empty string " +
				"and read back as the default")
			continue
		case seen[s.ID]:
			t.Errorf("%q appears twice; SourceByID returns the first and the second is "+
				"unreachable, so the panel would offer a choice that does nothing", s.ID)
			continue
		}
		seen[s.ID] = true

		if s.IPv4URL == "" || s.IPv6URL == "" {
			t.Errorf("%s has no URL for one address family; the refresh would report a "+
				"failure that names an empty address", s.ID)
		}
		if s.IPv4File == "" || s.IPv6File == "" {
			t.Errorf("%s has no local filename, so a deployment mirroring datasets "+
				"could not use it and would get 'open : no such file'", s.ID)
		}
		if !strings.HasPrefix(s.IPv4URL, "https://") || !strings.HasPrefix(s.IPv6URL, "https://") {
			t.Errorf("%s is fetched over something other than https", s.ID)
		}
		if s.Label == "" || s.Why == "" {
			t.Errorf("%s has no label or no reason to choose it. The panel would draw a "+
				"blank option, and an option a person cannot tell apart from the one "+
				"above it is not a choice", s.ID)
		}
		if s.Licence == "" {
			t.Errorf("%s has no licence recorded. It is fetched onto a customer's "+
				"machine under somebody's terms, and this table is where the answer "+
				"lives", s.ID)
		}
	}
}

// TestNoSourceCostsTheDeploymentAnything.
//
// The rule the library's own comment states, asserted rather than left
// as prose: every dataset shipped here is under terms that ask nothing
// of the deployment.
//
// It is easy to break with good intentions. The same publisher offers
// DB-IP Lite (CC BY 4.0) and GeoLite2, both better in coverage and both
// carrying an obligation - an attribution the customer must display,
// terms they must accept. Adding one would be a change to what this
// software asks of the people running it, made in a table that looks
// like configuration.
//
// Not a ban. A source with a cost may be added, and this test is where
// somebody has to say so out loud.
func TestNoSourceCostsTheDeploymentAnything(t *testing.T) {
	// Licences that impose nothing on a deployment that merely uses the
	// data, with why each is here.
	free := map[string]string{
		licencePDDL: "public domain dedication; free use, no attribution",
	}

	for _, s := range All() {
		if free[s.Licence] == "" {
			t.Errorf("%s is under %q, which is not in the list of licences that ask "+
				"nothing of a deployment.\n"+
				"If that is intended, add the licence to `free` with what it requires, "+
				"and say the same thing in THIRD-PARTY.md and in the source's Why - "+
				"the customer running this is the one who has to comply",
				s.ID, s.Licence)
		}
	}
}

// TestBothKindsHaveSomething.
//
// A library with no ASN source would leave the ASN enum empty, which
// renders as a dropdown with nothing in it rather than as an error.
func TestBothKindsHaveSomething(t *testing.T) {
	for _, k := range []struct {
		kind SourceKind
		name string
	}{{KindCountry, "country"}, {KindASN, "ASN"}} {
		if len(IDs(k.kind)) == 0 {
			t.Errorf("the library has no %s dataset, so its setting would offer an "+
				"empty list", k.name)
		}
	}
}

// TestTheDefaultsAreInTheLibrary.
//
// mustSource panics at package initialisation for a default that is not
// there, which is the right behaviour and a terrible way to find out.
// This is the same question asked where it can be answered with a
// sentence.
func TestTheDefaultsAreInTheLibrary(t *testing.T) {
	for _, d := range []struct{ id, kind string }{
		{DefaultCountry, "country"},
		{DefaultASN, "ASN"},
	} {
		src, ok := ByID(d.id)
		if !ok {
			t.Errorf("the default %s source %q is not in the library; the binary would "+
				"panic on start", d.kind, d.id)
			continue
		}
		if d.kind == "country" && src.Kind != KindCountry {
			t.Errorf("the default country source %q is an ASN dataset", d.id)
		}
		if d.kind == "ASN" && src.Kind != KindASN {
			t.Errorf("the default ASN source %q is a country dataset", d.id)
		}
	}
}
