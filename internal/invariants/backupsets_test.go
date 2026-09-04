package invariants

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
	"github.com/cruciblelab/crucible-analytic/internal/schemafiles"
)

// Every table is either backed up or deliberately not.
//
// # The two defects this covers, one of them already made
//
// **A name that is not a table.** internal/backup's panel set named
// `panel_sites`, which does not exist - sites are identified by a string
// and their membership lives in panel_site_members. Nothing caught it
// until a round-trip test tried to count the rows, and a set naming a
// missing table is a backup that fails at the moment it is taken, on a
// customer's machine, with the disk already committed.
//
// **A table nobody placed.** The worse one, because it is silent. A
// table added in a future migration and not put in a set is simply not
// in the backup: the file is produced, the operation reports success,
// and one table's worth of the customer's data is missing from it. That
// is only discovered during a restore, which is the one moment there is
// no second chance.
//
// So both directions are checked against the schema files rather than
// against a list: the tables are read out of the CREATE TABLE statements
// this product actually ships.
//
// *Yokluğu, "sayı bulamadım" ile aynı şey sanan bir kontrol, kusurun tam
// da ürettiği şekli görmez.*

// createTable finds the tables the schema files create.
//
// Anchored to the start of a line so a table named inside a comment or a
// policy body is not mistaken for one being created.
var createTable = regexp.MustCompile(`(?mi)^CREATE TABLE IF NOT EXISTS\s+([a-z0-9_]+)`)

func schemaTables(t *testing.T) []string {
	t.Helper()
	seen := map[string]bool{}
	var out []string
	for _, f := range schemafiles.InOrder {
		for _, m := range createTable.FindAllStringSubmatch(f.SQL, -1) {
			name := m[1]
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// TestEveryTableIsInASetOrExplicitlyExcluded.
func TestEveryTableIsInASetOrExplicitlyExcluded(t *testing.T) {
	tables := schemaTables(t)
	if len(tables) == 0 {
		t.Fatal("no CREATE TABLE statements were found in the schema files, so this " +
			"check examined nothing. If the schemas changed shape, this has to change " +
			"with them")
	}

	inSet := map[string]string{}
	for _, s := range backup.Sets {
		for _, table := range s.Tables {
			inSet[table] = s.Name
		}
	}

	for _, table := range tables {
		if _, ok := inSet[table]; ok {
			continue
		}
		if _, ok := backup.Excluded[table]; ok {
			continue
		}
		t.Errorf("%s is a table this product creates and internal/backup neither backs "+
			"it up nor says why not.\n"+
			"A table nobody placed is one that quietly is not in the file: the backup "+
			"is taken, it reports success, and the rows are gone. Add it to a set, or "+
			"to Excluded with the reason.", table)
	}
}

// TestEverySetNamesATableThatExists is the other direction, and it is
// the one that was already broken.
func TestEverySetNamesATableThatExists(t *testing.T) {
	exists := map[string]bool{}
	for _, table := range schemaTables(t) {
		exists[table] = true
	}

	for _, s := range backup.Sets {
		if len(s.Tables) == 0 {
			t.Errorf("the set %q names no tables, so choosing it would produce an empty "+
				"backup that reports success", s.Name)
		}
		for _, table := range s.Tables {
			if !exists[table] {
				t.Errorf("the set %q names %s and no schema file creates it.\n"+
					"The backup fails when it is taken - on a customer's machine, with "+
					"the disk already committed to it", s.Name, table)
			}
		}
	}
	for table := range backup.Excluded {
		if !exists[table] {
			t.Errorf("Excluded names %s and no schema file creates it. A stale exclusion "+
				"is how the next table of that name inherits a decision nobody made "+
				"about it", table)
		}
	}
}

// TestNoTableIsBothIncludedAndExcluded.
//
// The two lists are read by different code paths, and a table in both
// would be backed up while a reader of Excluded believed it was not -
// which is the wrong way round for a file holding credentials.
func TestNoTableIsBothIncludedAndExcluded(t *testing.T) {
	for _, s := range backup.Sets {
		for _, table := range s.Tables {
			if why, ok := backup.Excluded[table]; ok {
				t.Errorf("%s is in the set %q and also in Excluded (%q). One of the two "+
					"is what somebody will read", table, s.Name, why)
			}
		}
	}
}

// TestTheSetsHaveWordsSomewhereReadable is a placeholder-free check that
// the set names are usable as message keys: they become
// "saglik.yedek.kume.<name>" when the page arrives, and a name with a
// dot or a space in it would silently split that key.
func TestTheSetNamesCanBeMessageKeys(t *testing.T) {
	for _, name := range backup.SetNames() {
		if name == "" || strings.ContainsAny(name, ". /") {
			t.Errorf("the set name %q cannot be part of a message key", name)
		}
	}
}
