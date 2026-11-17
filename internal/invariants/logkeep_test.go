package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every service that keeps a log tree keeps it while it runs (W4a).
//
// logging.Setup makes one maintenance pass, at startup. Until W4a that
// was the only pass any process made, and a comment beside it said a
// panel operation covered the long-running case; nothing did. So the
// rule, from the tree: a cmd/ binary that calls logging.Setup also calls
// logging.KeepLive or logging.KeepFile - and one that reads the settings
// table (settings.New) calls KeepLive, because the panel offers the
// three ages as settings and a service that can read them and does not
// is the "saved, and nothing happens" this phase removed.
//
// Derived from the binaries rather than listed: a sixth service gets the
// rule the day it calls Setup.
func TestEveryServiceKeepsItsLogTreeWhileItRuns(t *testing.T) {
	root := repoRootFromInvariants(t)
	dirs, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		t.Fatal(err)
	}
	sawSetup := 0
	for _, dir := range dirs {
		if !dir.IsDir() {
			continue
		}
		calls := callsIn(t, filepath.Join(root, "cmd", dir.Name()))
		if !calls["logging.Setup"] {
			continue
		}
		sawSetup++
		live := calls["logging.KeepLive"]
		file := calls["logging.KeepFile"] || calls["logging.KeepFileOnce"]
		switch {
		case calls["settings.New"] && !live:
			t.Errorf("cmd/%s reads the settings table and does not call logging.KeepLive: "+
				"the panel's three log ages never reach its files", dir.Name())
		case !live && !file:
			t.Errorf("cmd/%s calls logging.Setup and none of logging.KeepLive, KeepFile "+
				"or KeepFileOnce: Setup keeps nothing, so its log tree is never kept",
				dir.Name())
		}
	}
	if sawSetup < 5 {
		t.Fatalf("found %d binaries calling logging.Setup; the five services all do, so the "+
			"scan is not seeing them", sawSetup)
	}
}

// callsIn is the set of pkg.Func calls in a directory's non-test files.
func callsIn(t *testing.T, dir string) map[string]bool {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok {
					out[x.Name+"."+sel.Sel.Name] = true
				}
			}
			return true
		})
	}
	return out
}
