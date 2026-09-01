package panel

import (
	"fmt"
	"strings"

	"github.com/cruciblelab/crucible-analytic/internal/ipsources"
)

// The three dataset settings, built from internal/ipsources at startup.
//
// # Why generated rather than written out
//
// Every other entry in the registry is a literal, and these are not,
// which is a difference worth justifying. The alternative was three
// hand-written enums listing the same ids the library already lists -
// and this project has spent a day finding what a second list costs: a
// role the CI workflow forgot, a binary the version check did not know
// about, a password nothing reported. Each was a list that stopped being
// complete, and each cost more to find than it would have cost to
// derive.
//
// So a source is added in one place. The mutation that proves it is in
// TestASourceAddedToTheLibraryReachesThePanel: add an entry, and the
// panel offers it without another line changing.
//
// # Why the panel may import this
//
// internal/ipsources is a leaf: a table, no behaviour, no dependencies.
// The rule this registry follows - see the overload-policy constants
// above, mirrored by hand on purpose - is that it must not drag in
// traffic-path code. A package that resolves addresses would have been
// mirrored; a package that is a list is imported.
func init() {
	for _, def := range sourceSettings() {
		if _, clash := registry[def.Key]; clash {
			// The same guard the generated limit family uses, for the
			// same reason: a generated definition silently replacing a
			// hand-written one would leave the panel showing the
			// generic wording and nobody looking for why.
			panic(fmt.Sprintf("panel: %s is defined twice", def.Key))
		}
		registry[def.Key] = def
	}
}

// sourceSettings builds the three definitions.
func sourceSettings() []Definition {
	return []Definition{
		{
			Key: KeySourceCountry, Scope: ScopeGlobal, Kind: KindEnum,
			Category: CatToplama,
			// Empty is the default and means the library's own default,
			// which is what every installation made before this setting
			// existed is already fetching. A definition whose default
			// named a dataset would have changed what those
			// installations download the moment they upgraded.
			Default:   "",
			Enum:      append([]string{""}, ipsources.IDs(ipsources.KindCountry)...),
			Label:     "Ülke veri kümesi",
			Help:      sourceHelp(ipsources.KindCountry),
			Developer: true,
			Live:      true,
		},
		{
			Key: KeySourceASN, Scope: ScopeGlobal, Kind: KindEnum,
			Category:  CatToplama,
			Default:   "",
			Enum:      append([]string{""}, ipsources.IDs(ipsources.KindASN)...),
			Label:     "ASN veri kümesi",
			Help:      sourceHelp(ipsources.KindASN),
			Developer: true,
			Live:      true,
		},
		{
			Key: KeySourceFallback, Scope: ScopeGlobal, Kind: KindStringList,
			Category: CatToplama,
			Default:  []string{},
			Label:    "Yedek veri kümeleri",
			Help: "Seçilen küme indirilemezse sırayla bunlar denenir. Boş " +
				"bırakılırsa yedek yok ve indirme başarısız olduğunda bir önceki " +
				"tablo yerinde kalır — yani ülke/ASN bilgisi eskir ama kaybolmaz. " +
				"Yanlış türde bir ad (ASN listesine ülke kümesi) atlanır ve " +
				"günlüğe yazılır. Geçerli adlar: " +
				strings.Join(ipsources.SortedIDs(), ", "),
			Developer: true,
			Live:      true,
			Check:     checkFallbackOrder,
		},
	}
}

// sourceHelp writes the help text from the library, so each option's
// reason to exist is the one the library records.
//
// Built rather than written, because a hand-written paragraph listing
// three datasets is a paragraph that describes two of them after the
// fourth is added - and the reader has no way to tell which sentence
// went stale.
func sourceHelp(kind ipsources.SourceKind) string {
	var b strings.Builder
	b.WriteString("Boş bırakılırsa varsayılan kullanılır. Seçenekler:\n")
	for _, s := range ipsources.All() {
		if s.Kind != kind {
			continue
		}
		fmt.Fprintf(&b, "\n• %s (%s) — %s Lisans: %s", s.Label, s.ID, s.Why, s.Licence)
	}
	return b.String()
}

// checkFallbackOrder refuses a list naming something the build does not
// carry.
//
// Refused at write time rather than skipped at refresh time, and the two
// are different promises: the resolver skips an unknown id because a
// half-usable list beats none at three in the morning, but a person
// typing one into the panel wants to be told now. The same value,
// answered differently depending on whether somebody is there to hear
// it.
func checkFallbackOrder(v any) error {
	list, ok := v.([]string)
	if !ok {
		return fmt.Errorf("liste bekleniyordu")
	}
	known := map[string]bool{}
	for _, id := range ipsources.SortedIDs() {
		known[id] = true
	}
	for _, id := range list {
		if !known[id] {
			return fmt.Errorf("%q bu yapının taşımadığı bir veri kümesi. Geçerli olanlar: %s",
				id, strings.Join(ipsources.SortedIDs(), ", "))
		}
	}
	return nil
}
