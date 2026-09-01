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

// TestEverySweepIsReachableFromRun.
//
// Both halves read from the source, so neither can be quietly satisfied:
// adding a PurgeOld* method fails this until Run reaches it, and
// deleting the call from Run fails it until the method goes too.
//
// Reachability rather than a direct call, because the call is one hop
// away on purpose - Run calls sweep, sweep calls PurgeOldFetches - and a
// test that demanded a direct call would be a test that dictates the
// shape of the code rather than its behaviour.
func TestEverySweepIsReachableFromRun(t *testing.T) {
	calls, methods := packageCallGraph(t)

	var sweeps []string
	for name := range methods {
		if strings.HasPrefix(name, "PurgeOld") {
			sweeps = append(sweeps, name)
		}
	}
	sort.Strings(sweeps)
	if len(sweeps) == 0 {
		t.Fatal("no PurgeOld* method found on *Resolver; this test would pass by " +
			"checking nothing, which is how it would look on the day somebody " +
			"renamed the sweep")
	}

	reached := reachableFrom("Run", calls)
	if len(reached) <= 1 {
		t.Fatal("Run appears to call nothing in this package, which cannot be true - " +
			"the scan found no calls and every check below would pass by comparing " +
			"against an empty set")
	}

	for _, sweep := range sweeps {
		if !reached[sweep] {
			t.Errorf("(*Resolver).%s is defined and Run never reaches it.\n"+
				"A sweep with no caller does not bound its table: the rows "+
				"accumulate, every test stays green, and the first symptom is a "+
				"full disk. PLAN.md's M2 says this in advance.", sweep)
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
