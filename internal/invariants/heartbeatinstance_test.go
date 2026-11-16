package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// The collector names its heartbeat row after its site (W3).
//
// The instance is wired in main, and nothing else would notice it gone:
// every heartbeat test builds its own Reporter, and a collector without
// one writes the role's own row - correct for a host with one collector,
// and on a host with one per site the stopped collector's "a moment ago"
// that this phase exists to remove. The rule, from the tree: every
// heartbeat.Options literal in cmd/collector sets Instance.
func TestTheCollectorsHeartbeatNamesItsSite(t *testing.T) {
	root := repoRootFromInvariants(t)
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join(root, "cmd", "collector", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Options" {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "heartbeat" {
				return true
			}
			found++
			for _, e := range lit.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Instance" {
						return true
					}
				}
			}
			t.Errorf("%s: the collector's heartbeat.Options sets no Instance - every collector "+
				"on a host writes one row, and a stopped one reads as alive", fset.Position(lit.Pos()))
			return true
		})
	}
	if found == 0 {
		t.Error("found no heartbeat.Options in cmd/collector; this check has stopped checking anything")
	}
}
