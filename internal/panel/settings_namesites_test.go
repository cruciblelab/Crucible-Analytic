package panel

import (
	"reflect"
	"testing"
)

// TestEveryListOfSitesIsNarrowedForATenant ties Definition.NamesSites to
// what makes a value a list of sites (W1).
//
// SettingsView narrows a NamesSites value to the reader's own sites; a
// second setting holding site ids that did not carry the flag would go
// out whole to every customer, the leak the live rig found in the
// diagnostic file. What makes a value a list of sites is the check that
// validates it as one - checkSiteList - so the flag is asked of every
// definition that check is attached to, in both directions: a flag on a
// value that is not a list of site ids would narrow something that
// names nobody.
func TestEveryListOfSitesIsNarrowedForATenant(t *testing.T) {
	siteList := reflect.ValueOf(checkSiteList).Pointer()
	found := 0
	for _, def := range AllDefinitions() {
		isList := def.Check != nil && reflect.ValueOf(def.Check).Pointer() == siteList
		if isList {
			found++
		}
		switch {
		case isList && !def.NamesSites:
			t.Errorf("%s is validated as a list of site ids and does not set NamesSites: "+
				"its value would reach the owner of one site whole, naming every other customer", def.Key)
		case def.NamesSites && !isList:
			t.Errorf("%s sets NamesSites without being validated by checkSiteList; "+
				"narrowing a value that is not a list of site ids hides something from its reader for no one", def.Key)
		case def.NamesSites && (def.Kind != KindStringList || def.Scope != ScopeGlobal):
			t.Errorf("%s sets NamesSites but is %s/%s; the narrowing reads a deployment-wide list", def.Key, def.Kind, def.Scope)
		}
	}
	if found == 0 {
		t.Fatal("no setting is validated by checkSiteList; this check no longer finds the site list and asks nothing")
	}
}
