package panel

import (
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/ipsources"
)

// TestThePanelOffersEverySourceTheLibraryHas.
//
// M1's third done criterion: a source added to internal/ipsources shows
// up in the panel with no other change.
//
// The property is not "the two lists agree" - a hand-written list that
// somebody remembered to update also agrees, right up until the day
// nobody does. It is that there is no second list at all, and the way to
// see that is to add an entry to the library and watch this pass without
// a line of panel code moving. That is the mutation this test exists to
// be run under.
func TestThePanelOffersEverySourceTheLibraryHas(t *testing.T) {
	for _, c := range []struct {
		key  Key
		kind ipsources.SourceKind
		what string
	}{
		{KeySourceCountry, ipsources.KindCountry, "country"},
		{KeySourceASN, ipsources.KindASN, "ASN"},
	} {
		def, ok := registry[c.key]
		if !ok {
			t.Errorf("%s is not in the registry, so the %s dataset cannot be chosen "+
				"from the panel at all", c.key, c.what)
			continue
		}

		offered := map[string]bool{}
		for _, v := range def.Enum {
			offered[v] = true
		}

		ids := ipsources.IDs(c.kind)
		if len(ids) == 0 {
			t.Fatalf("the library has no %s datasets; this check would pass by "+
				"comparing nothing", c.what)
		}
		for _, id := range ids {
			if !offered[id] {
				t.Errorf("the library carries %s dataset %q and the panel does not "+
					"offer it.\nThe enum is generated from the library, so this means "+
					"somebody replaced the generation with a list - and a list is what "+
					"this phase exists to remove", c.what, id)
			}
		}

		// And the empty option, which is what "use the default" is.
		if !offered[""] {
			t.Errorf("%s offers no empty option, so a deployment cannot say 'use "+
				"whatever this build's default is' - and every upgrade that changed "+
				"the default would be invisible to them", c.key)
		}

		// The other direction: an option naming nothing.
		for _, v := range def.Enum {
			if v == "" {
				continue
			}
			if _, known := ipsources.ByID(v); !known {
				t.Errorf("%s offers %q, which is not in the library. Choosing it would "+
					"store an id the resolver falls back from, so the panel would be "+
					"offering a choice that does nothing", c.key, v)
			}
		}
	}
}

// TestTheHelpTextNamesEverySource.
//
// The dropdown shows ids; the help text is where a person finds out what
// they mean and why they would pick one. A generated enum with a
// hand-written paragraph is the worst of both: the option appears, and
// the sentence explaining it does not.
func TestTheHelpTextNamesEverySource(t *testing.T) {
	for _, c := range []struct {
		key  Key
		kind ipsources.SourceKind
	}{{KeySourceCountry, ipsources.KindCountry}, {KeySourceASN, ipsources.KindASN}} {
		def := registry[c.key]
		for _, id := range ipsources.IDs(c.kind) {
			src, _ := ipsources.ByID(id)
			if !strings.Contains(def.Help, id) {
				t.Errorf("%s's help never mentions %q, so the dropdown offers an id "+
					"with no explanation anywhere on the page", c.key, id)
			}
			if !strings.Contains(def.Help, src.Licence) {
				t.Errorf("%s's help does not state %q's licence. It is fetched onto "+
					"the customer's machine; they are the one who has to comply",
					c.key, id)
			}
		}
	}
}

// TestTheFallbackListRefusesAnUnknownDataset.
//
// The setting takes free-ish text - a list of ids - and this is the
// check that stops it storing one this build cannot use. Refused when a
// person types it, rather than skipped six hours later at a refresh
// nobody is watching.
func TestTheFallbackListRefusesAnUnknownDataset(t *testing.T) {
	def, ok := registry[KeySourceFallback]
	if !ok {
		t.Fatal("the fallback setting is not registered")
	}
	if def.Check == nil {
		t.Fatal("the fallback setting has no Check, so any list of words would be " +
			"stored and every one of them skipped at refresh")
	}

	if err := def.Check([]string{ipsources.DefaultCountry}); err != nil {
		t.Errorf("a real dataset was refused: %v", err)
	}
	if err := def.Check([]string{"boyle-bir-sey-yok"}); err == nil {
		t.Error("an id this build does not carry was accepted. The refresh would skip " +
			"it, so the operator would believe they had a fallback and find out only " +
			"when the first source failed")
	}
}
