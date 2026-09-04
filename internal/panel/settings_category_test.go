package panel

import (
	"sort"
	"testing"
)

// D4c turned the settings page from one flat list into sections, and
// that swap has exactly one dangerous failure mode: a setting that
// belongs to no section is not misplaced, it is *gone*. Nothing errors,
// no page is blank, no log line appears - the row simply is not drawn,
// and the customer concludes the setting does not exist.
//
// Before the change, a new definition needed no group and was shown
// anyway. That is what stopped being true, so it is what gets a test.

// TestEverySettingIsInACategory.
//
// Derived from the registry rather than from a list kept beside it. A
// hand list of "settings that have a category" is a second thing to
// update, and the whole failure being guarded is somebody updating one
// place and not the other.
func TestEverySettingIsInACategory(t *testing.T) {
	known := map[Category]bool{}
	for _, c := range CategoryOrder {
		if known[c] {
			t.Errorf("CategoryOrder lists %q twice; the page would draw that section "+
				"twice and each setting in it once, which reads as a duplicate", c)
		}
		known[c] = true
	}
	if len(known) == 0 {
		t.Fatal("CategoryOrder is empty, so every check below passes by checking nothing")
	}

	if len(registry) == 0 {
		t.Fatal("the registry is empty; this test would pass on a panel with no settings")
	}

	for key, def := range registry {
		switch {
		case def.Category == "":
			t.Errorf("%s has no Category. It would be drawn under no heading, which is "+
				"to say not drawn at all - and a setting the page silently omits is one "+
				"the customer concludes does not exist", key)
		case !known[def.Category]:
			t.Errorf("%s is in category %q, which is not in CategoryOrder. The page "+
				"walks that slice, so this setting would never be reached", key, def.Category)
		}
	}
}

// TestNoCategoryIsEmpty.
//
// The other direction, and it is not symmetry for its own sake. An
// empty section draws a heading somebody can open onto nothing, which
// reads as a bug in the panel rather than as a category that lost its
// last setting - and the person who notices is a customer, not us.
//
// It also catches the likelier version: a category constant kept after
// its settings moved elsewhere, sitting in CategoryOrder waiting for a
// future setting to be filed under it by mistake.
func TestNoCategoryIsEmpty(t *testing.T) {
	count := map[Category]int{}
	for _, def := range registry {
		count[def.Category]++
	}
	for _, c := range CategoryOrder {
		if count[c] == 0 {
			t.Errorf("category %q has no settings. Either it lost its last one and "+
				"should go, or a setting that belongs in it was filed elsewhere", c)
		}
	}
}

// TestTheLegallyWeightySettingsAreTogether.
//
// Not a style rule. The settings that decide what personal data is
// stored, and for how long, are the ones somebody has to be able to find
// and account for when a customer or a regulator asks. Answering "where
// is that configured" by naming four different sections is not an
// answer.
//
// So the property is asserted rather than left to whoever adds the next
// retention setting: anything behind the developer password - which is
// how this project marks legal weight, see
// Definition.RequiresDeveloperPassword - is in one place.
//
// The exception is named, not pattern-matched, for the reason the CLA
// exemptions are: a rule loose enough to be convenient is a rule that
// silently absorbs the case it was meant to catch.
func TestTheLegallyWeightySettingsAreTogether(t *testing.T) {
	// Guarded settings that are deliberately not about personal data.
	elsewhere := map[Key]string{
		KeyUpgradeLocked: "not about data at all - it decides whether the customer may " +
			"apply a schema upgrade, and it is guarded because it creates work for the " +
			"developer rather than because it stores anything",
		KeyReleaseUpdateLocked: "the same, one step further out: it decides whether the " +
			"customer may replace the binaries. Guarded because the operation can take " +
			"the customer's website down and the developer is who gets called, not " +
			"because anything is stored",
	}

	var strays []string
	for key, def := range registry {
		if !def.RequiresDeveloperPassword {
			continue
		}
		if _, ok := elsewhere[key]; ok {
			continue
		}
		if def.Category != CatGizlilik {
			strays = append(strays, string(key))
		}
	}
	sort.Strings(strays)

	for _, key := range strays {
		t.Errorf("%s carries legal weight (it is behind the developer password) and is "+
			"not in the privacy section.\n"+
			"Somebody asked what this deployment stores and for how long has to find "+
			"the answer in one place. If this one genuinely is not about personal data, "+
			"add it to `elsewhere` with the reason", key)
	}

	// And the exemption list does not outlive its entries.
	for key := range elsewhere {
		def, ok := registry[key]
		if !ok {
			t.Errorf("%s is exempted from the privacy grouping and is not in the "+
				"registry; a stale exemption is how the next setting of that name "+
				"inherits a decision nobody made about it", key)
			continue
		}
		if !def.RequiresDeveloperPassword {
			t.Errorf("%s is exempted from the privacy grouping but is not guarded, so "+
				"the rule never applied to it and the entry says nothing", key)
		}
	}
}
