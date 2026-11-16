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

// Every full proxy that is built signs its verdict (W2).
//
// The verdict is wired in main: a fullproxy.Server literal given a
// VerdictKey and the flusher's IdentityScore. Leave either field out and every
// functional test still passes - the proxy's tests build their own
// Server, the beacon's tests sign their own headers - while every
// deployment forwards events with no verdict and the dashboard counts the
// curl that the collector already scored 74. P5a was the same shape: a
// live setting whose only wiring was in main, so no test could see that
// it was missing.
//
// The rule, from the tree: outside tests, every composite literal of
// fullproxy.Server sets both VerdictKey and IdentityScore.
//
// And the same for the beacon's half (W2b): every beacon.Server sets
// EventRate. Its zero value judges nothing, which is what every beacon
// test builds - so a main that left the field out would pass them all
// and count every flood as people.
func TestEveryFullProxySignsItsVerdict(t *testing.T) {
	rules := []struct {
		pkg    string
		fields []string
		why    string
	}{
		{"fullproxy", []string{"VerdictKey", "IdentityScore"}, "every event it forwards " +
			"reaches the beacon with no verdict, and the dashboard counts what the " +
			"collector already judged automated (W2)"},
		{"beacon", []string{"EventRate"}, "it judges no address's event rate, and a flood " +
			"is counted as people and written to the customer's disk (W2b)"},
	}
	root := repoRootFromInvariants(t)
	fset := token.NewFileSet()
	built := map[string]int{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "scratchpad", "dist", ".claude":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Server" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			for _, rule := range rules {
				if pkg.Name != rule.pkg {
					continue
				}
				built[rule.pkg]++
				set := map[string]bool{}
				for _, e := range lit.Elts {
					if kv, ok := e.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok {
							set[k.Name] = true
						}
					}
				}
				for _, field := range rule.fields {
					if !set[field] {
						t.Errorf("%s builds a %s.Server without %s: %s",
							fset.Position(lit.Pos()), rule.pkg, field, rule.why)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range rules {
		if built[rule.pkg] == 0 {
			t.Errorf("found no %s.Server literal outside tests; this check has stopped checking anything", rule.pkg)
		}
	}
}
