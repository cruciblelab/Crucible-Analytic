package panel

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A sweep nothing calls is the failure that stays green.
//
// PurgeOldLoginAttempts and PurgeOldDevAccess were both written,
// documented and correct, and neither had a caller anywhere in this
// repository - tests included. The tables they bound had been growing
// without limit since the day they were created, and every test passed
// the whole time. The first symptom would have been a full disk on
// somebody's server.
//
// Nothing about either function was wrong. What was missing was the
// question "who calls this", and that is a question a test can ask.

// TestEveryPurgeIsCalledByHousekeeping.
//
// Both halves are read from the source rather than listed here, so
// neither can be quietly satisfied: adding a Purge method makes the test
// fail until Housekeeping calls it, and deleting a call from
// Housekeeping makes it fail until the method goes too.
//
// A hand-written list would have been the same mistake one level up - it
// is dangerous not when it is wrong but when it is short, because wrong
// goes red and short stays green.
func TestEveryPurgeIsCalledByHousekeeping(t *testing.T) {
	defined := purgeMethodsDefined(t)
	if len(defined) == 0 {
		t.Fatal("no Purge methods found; this test would pass by comparing nothing")
	}
	called := methodsCalledByHousekeeping(t)

	for _, name := range defined {
		if !called[name] {
			t.Errorf("(*Store).%s is defined and Housekeeping never calls it.\n"+
				"A sweep with no caller does not bound its table: the rows accumulate, "+
				"every test stays green, and the first symptom is a full disk. "+
				"Add it to Housekeeping, or delete it.", name)
		}
	}
}

// purgeMethodsDefined finds every PurgeOld* method on *Store.
func purgeMethodsDefined(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}

	var out []string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || !strings.HasPrefix(fn.Name.Name, "PurgeOld") {
					continue
				}
				out = append(out, fn.Name.Name)
			}
		}
	}
	return out
}

// methodsCalledByHousekeeping reads the calls Housekeeping makes.
func methodsCalledByHousekeeping(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join(".", "housekeeping.go"), nil, 0)
	if err != nil {
		t.Fatalf("parsing housekeeping.go: %v", err)
	}

	called := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Housekeeping" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				called[sel.Sel.Name] = true
			}
			return true
		})
	}
	return called
}

// TestHousekeepingIsReachableFromMain.
//
// The level above: Housekeeping itself could be the next thing written
// and never wired. It is called from cmd/panel, which this package
// cannot import, so the check reads the file.
//
// Reading a sibling directory from a test is not something to do
// casually. It is right here because the defect being guarded is
// precisely a gap *between* two packages, and a test that stayed inside
// this one could not see it - which is how the gap lasted this long.
//
// # Why this walks the calls instead of searching for a string
//
// The first version asked whether ".Housekeeping(" appeared anywhere in
// main.go. Mutating `go runHousekeeping(...)` out of main left it green:
// the substring was still there, inside runHousekeeping's own body. The
// test was looking at something that was true rather than at the thing
// that could be false - so it would have passed on a panel whose sweep
// loop was defined, complete, and never started.
//
// Both links are checked, because either one alone is a sweep that never
// runs: main must start the loop, and the loop must call the pass.
func TestHousekeepingIsReachableFromMain(t *testing.T) {
	path := filepath.Join("..", "..", "cmd", "panel", "main.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parsing cmd/panel/main.go: %v", err)
	}

	// funcName -> the names it calls, whether as f() or x.f().
	calls := map[string]map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		made := map[string]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				made[fun.Name] = true
			case *ast.SelectorExpr:
				made[fun.Sel.Name] = true
			}
			return true
		})
		calls[fn.Name.Name] = made
	}

	// A `go f(...)` statement is a call for this purpose - it is how the
	// loop is actually started - and ast.Inspect finds the CallExpr
	// inside it either way.
	if !calls["main"]["runHousekeeping"] {
		t.Error("cmd/panel's main never starts runHousekeeping.\n" +
			"Every sweep in housekeeping.go then bounds nothing, which is the " +
			"state the panel was in before this file existed.")
	}
	if !calls["runHousekeeping"]["Housekeeping"] {
		t.Error("runHousekeeping never calls Store.Housekeeping, so the loop runs and sweeps nothing")
	}
}
