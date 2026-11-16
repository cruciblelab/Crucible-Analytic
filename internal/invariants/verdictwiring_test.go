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
// VerdictKey and the flusher's ScoreNow. Leave either field out and every
// functional test still passes - the proxy's tests build their own
// Server, the beacon's tests sign their own headers - while every
// deployment forwards events with no verdict and the dashboard counts the
// curl that the collector already scored 74. P5a was the same shape: a
// live setting whose only wiring was in main, so no test could see that
// it was missing.
//
// The rule, from the tree: outside tests, every composite literal of
// fullproxy.Server sets both VerdictKey and Score.
func TestEveryFullProxySignsItsVerdict(t *testing.T) {
	root := repoRootFromInvariants(t)
	fset := token.NewFileSet()
	var built int
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
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "fullproxy" {
				return true
			}
			built++
			set := map[string]bool{}
			for _, e := range lit.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok {
						set[k.Name] = true
					}
				}
			}
			for _, field := range []string{"VerdictKey", "Score"} {
				if !set[field] {
					t.Errorf("%s builds a fullproxy.Server without %s: every event it forwards "+
						"reaches the beacon with no verdict, and the dashboard counts what the "+
						"collector already judged automated (W2)", fset.Position(lit.Pos()), field)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if built == 0 {
		t.Fatal("found no fullproxy.Server literal outside tests; this check has stopped checking anything")
	}
}
