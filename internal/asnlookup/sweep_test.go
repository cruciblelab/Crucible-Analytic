package asnlookup

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// A sweep nothing calls is the failure that stays green.
//
// internal/panel learned this the expensive way: two purge functions
// were written, documented and correct, and neither had a caller
// anywhere in the repository. The tables they bound had been growing
// without limit since the day they were created, every test passed the
// whole time, and the first symptom would have been a full disk.
//
// PLAN.md's M2 warned about it again in advance - "o dosyanın var olma
// sebebi tam olarak 'yazılıp çağrılmayan süpürme'" - and named
// internal/panel/housekeeping.go as the place to hang this phase's
// sweep. It could not go there: the same paragraph says the panel only
// reads this table, and a sweep needs DELETE. PurgeOldFetches explains
// the resolution at length.
//
// What the plan was actually protecting is not the file, it is the
// property, and the property is checked here instead.

// periodicDuties are the things Run exists to do, with what stops if it
// stops doing each.
//
// The half that can be wrong on purpose, so every entry carries why -
// the same rule internal/invariants' listeningPackages and the CLA
// exemptions follow. Adding a line here is the moment somebody reads
// what a duty owes the deployment.
//
// # Why a list at all, when PurgeOld* is derived
//
// Sweeps have a name shape, so they can be found. The other duties do
// not, and the alternative to naming them is naming nothing - which is
// where this test started and what it missed: emptying Run's
// request-poll case left every test in this repository green while the
// M3 button silently stopped working. Measured, by doing exactly that.
var periodicDuties = map[string]string{
	"refresh": "the range tables go stale, and every lookup keeps answering " +
		"from whatever was loaded at startup",
	"sweep": "ip_range_fetches grows without bound; the first symptom is a full disk",
	"answerRequests": "the panel's refresh button writes a row nobody reads, so it " +
		"queues a request, shows it waiting, and expires it a few minutes later",
}

// TestRunReachesEveryPeriodicDuty.
//
// Both halves read from the source, so neither can be quietly satisfied:
// adding a PurgeOld* method fails this until Run reaches it, deleting a
// call from Run fails it until the duty goes too, and a duty named here
// that no longer exists fails it as a stale entry.
//
// Reachability rather than a direct call, because some calls are one hop
// away on purpose - Run calls sweep, sweep calls PurgeOldFetches - and a
// test that demanded a direct call would dictate the shape of the code
// rather than its behaviour.
func TestRunReachesEveryPeriodicDuty(t *testing.T) {
	calls, methods := packageCallGraph(t)

	// The derived half: anything shaped like a sweep.
	duties := map[string]string{}
	for name := range methods {
		if strings.HasPrefix(name, "PurgeOld") {
			duties[name] = "a sweep with no caller does not bound its table: the rows " +
				"accumulate, every test stays green, and the first symptom is a full disk"
		}
	}
	if len(duties) == 0 {
		t.Fatal("no PurgeOld* method found on *Resolver; the derived half of this " +
			"test would check nothing, which is how it would look on the day " +
			"somebody renamed the sweep")
	}

	// And the named half.
	for name, why := range periodicDuties {
		if !methods[name] {
			t.Errorf("periodicDuties names (*Resolver).%s and no such method exists. "+
				"A stale entry is a duty this test believes is covered and is not", name)
			continue
		}
		duties[name] = why
	}

	reached := reachableFrom("Run", calls)
	if len(reached) <= 1 {
		t.Fatal("Run appears to call nothing in this package, which cannot be true - " +
			"the scan found no calls and every check below would pass by comparing " +
			"against an empty set")
	}

	var names []string
	for name := range duties {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if !reached[name] {
			t.Errorf("(*Resolver).%s is defined and Run never reaches it.\n"+
				"What stops when it does not run: %s.\n"+
				"Run is the only entry point a service has, so a duty it does not "+
				"reach is a duty no deployment performs - however many tests call "+
				"it directly.", name, duties[name])
		}
	}
}

// packageCallGraph returns, for every method on *Resolver, the names of
// the other *Resolver methods it calls, plus the set of methods defined.
func packageCallGraph(t *testing.T) (calls map[string][]string, methods map[string]bool) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		// Non-test files only. A sweep called from a test is a sweep no
		// deployment runs, which is the failure rather than the fix.
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}

	calls = map[string][]string{}
	methods = map[string]bool{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || !onResolver(fn.Recv) {
					continue
				}
				methods[fn.Name.Name] = true
				ast.Inspect(fn, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok {
						return true
					}
					// r.Something(...) where r is the receiver. The name is
					// not checked against the receiver's identifier: every
					// method in this package names it r, and a test that
					// insisted on that would break on a rename that changed
					// nothing.
					if ident, ok := sel.X.(*ast.Ident); ok && ident.Obj != nil {
						calls[fn.Name.Name] = append(calls[fn.Name.Name], sel.Sel.Name)
					}
					return true
				})
			}
		}
	}
	return calls, methods
}

// onResolver reports whether a receiver is *Resolver or Resolver.
func onResolver(recv *ast.FieldList) bool {
	if len(recv.List) == 0 {
		return false
	}
	expr := recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	ident, ok := expr.(*ast.Ident)
	return ok && ident.Name == "Resolver"
}

// reachableFrom walks the call graph, including the starting point.
func reachableFrom(start string, calls map[string][]string) map[string]bool {
	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		for _, next := range calls[name] {
			if seen[next] {
				continue
			}
			seen[next] = true
			queue = append(queue, next)
		}
	}
	return seen
}
