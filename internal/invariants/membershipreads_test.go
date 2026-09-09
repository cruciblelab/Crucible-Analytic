package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every read of panel_site_members either filters expired memberships or
// is on this list with a reason.
//
// # The failure this exists for
//
// A membership can now end on its own, and nothing sweeps the row away -
// the expiry is enforced by the queries that read the table. That design
// is one line of SQL away from being wrong in the worst possible
// direction: a reader added tomorrow that forgets the filter grants
// access to somebody the members page has already listed under "this
// access has ended". Nobody re-checks a person they believe is out, so
// that access stands until somebody happens to look.
//
// Nothing fails when it happens. The query is valid, the page renders,
// the tests pass. The only thing that notices is a test that reads the
// source and counts.
//
// # Why a list and not a scan
//
// PLAN.md §3.4: checked against a list, not against memory. Three
// readers deliberately do not filter, and each has a reason that is not
// obvious from looking at it - so they are named here with the reason
// beside them. A fourth appearing without being named is the failure;
// so is one of these three disappearing, because then the reason
// recorded here is describing code that no longer exists.
//
// *Süzgeci unutan bir okuma, unuttuğunu kendi söylemez.*

// membershipReadExceptions are the reads that must not filter, and why.
var membershipReadExceptions = map[string]string{
	"allSites": "site discovery for the operator, not an authorization decision: " +
		"a site whose only membership has run out still exists, and hiding it would " +
		"leave nobody able to see that it needs a new owner",
	"lockMembership": "the writers' locked read: a writer that cannot see a row that " +
		"has run out cannot give that person access again, and the owner census it " +
		"takes is unaffected because an ownership can never carry an end date",
	"Members": "the members page's own list, which returns both kinds and labels each " +
		"with endedMembership - the negation of the filter, so the label and the door " +
		"cannot mean different things",
}

// membershipTable is the table these rules are about.
const membershipTable = "panel_site_members"

// filterMarker is what a filtered read must contain. The call, not the
// SQL: writing the condition out by hand is exactly the drift this
// guards against, so only going through the function counts.
const filterMarker = "liveMembership("

// fromTable finds "<word> FROM panel_site_members", capturing the word
// before FROM so a DELETE can be told from a read.
//
// The distinction matters and the first version of this test did not
// make it: it flagged RemoveMember, which only deletes. A write does not
// need the filter - it needs to see the row precisely because it is
// removing it - and the read it depends on is lockMembership's, which is
// on the list below. INSERT names the table without a FROM at all.
var fromTable = regexp.MustCompile(`(?is)(\w+)\s+FROM\s+` + membershipTable + `\b`)

// readsTable reports whether src contains a read rather than only a
// delete.
func readsTable(src string) bool {
	for _, m := range fromTable.FindAllStringSubmatch(src, -1) {
		if !strings.EqualFold(m[1], "DELETE") {
			return true
		}
	}
	return false
}

func TestEveryMembershipReadFiltersOrIsExcused(t *testing.T) {
	dir := filepath.Join(repoRootFromInvariants(t), "internal", "panel")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	found := map[string]bool{} // function name -> reads the table unfiltered
	filtered := map[string]bool{}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// The whole function's source, because the SQL and the
			// concatenated filter are separate syntax nodes and what
			// matters is whether they end up in the same query.
			src := nodeSource(t, fset, dir, fn)
			if !readsTable(src) {
				continue
			}
			if strings.Contains(src, filterMarker) {
				filtered[fn.Name.Name] = true
				continue
			}
			found[fn.Name.Name] = true
		}
	}

	if len(found)+len(filtered) == 0 {
		t.Fatal("no reads of " + membershipTable + " were found at all; this test is " +
			"looking in the wrong place and would pass whatever the code did")
	}

	for name := range found {
		if _, excused := membershipReadExceptions[name]; !excused {
			t.Errorf("%s reads %s without liveMembership and is not on the list.\n"+
				"Either add the filter, or add it to membershipReadExceptions with the "+
				"reason it must not have one. An unfiltered read hands access to somebody "+
				"the members page has already listed as ended.", name, membershipTable)
		}
	}

	// And the other direction: a reason recorded for code that is gone,
	// or for a read that has since been filtered, is a reason nobody
	// will notice is stale.
	var stale []string
	for name := range membershipReadExceptions {
		if !found[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		if filtered[name] {
			t.Errorf("%s is excused from filtering but now filters. Remove it from "+
				"membershipReadExceptions; a note explaining an exception that no longer "+
				"exists is a note that misleads the next reader.", name)
			continue
		}
		t.Errorf("%s is excused from filtering but no longer reads %s. Remove it from "+
			"membershipReadExceptions.", name, membershipTable)
	}
}

// nodeSource returns the exact source text of a declaration.
func nodeSource(t *testing.T, fset *token.FileSet, dir string, n ast.Node) string {
	t.Helper()
	pos := fset.Position(n.Pos())
	end := fset.Position(n.End())
	b, err := os.ReadFile(filepath.Join(dir, filepath.Base(pos.Filename)))
	if err != nil {
		t.Fatal(err)
	}
	if end.Offset > len(b) {
		t.Fatalf("%s: offset past end of file", pos.Filename)
	}
	return string(b[pos.Offset:end.Offset])
}
