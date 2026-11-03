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

// Every process that refuses by country or ASN reports how many it
// refused (B3f, catalog #12).
//
// A geo refusal is a 403 or a closed connection before anything is
// recorded: the count in the heartbeat is the only trace a rule in force
// leaves, and the panel shows it beside the lists. The count itself is
// kept by the blocklist (limiter.GeoBlocklist.Refuses), so no server can
// refuse without counting; what nothing else checks is whether the
// process that built the blocklist hands the count to its heartbeat.
// Delete that one map entry and every functional test still passes,
// while the panel says "the collector does not report this".
//
// The rule, from the tree: a file that builds a blocklist with
// limiter.NewGeoBlocklist has a map entry keyed
// heartbeat.CounterGeoBlocked whose value calls Refused() on that same
// blocklist.
func TestEveryGeoBlocklistReportsWhatItRefused(t *testing.T) {
	root := repoRootFromInvariants(t)
	fset := token.NewFileSet()
	var builders int
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
		// The variables a blocklist is built into.
		lists := map[string]token.Pos{}
		ast.Inspect(file, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			call, ok := as.Rhs[0].(*ast.CallExpr)
			if !ok {
				return true
			}
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "NewGeoBlocklist" {
				if id, ok := as.Lhs[0].(*ast.Ident); ok {
					lists[id.Name] = as.Pos()
				}
			}
			return true
		})
		if len(lists) == 0 {
			return nil
		}
		builders++
		// The counter entries, and whose Refused() they call.
		reported := map[string]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			kv, ok := n.(*ast.KeyValueExpr)
			if !ok {
				return true
			}
			key, ok := kv.Key.(*ast.SelectorExpr)
			if !ok || key.Sel.Name != "CounterGeoBlocked" {
				return true
			}
			ast.Inspect(kv.Value, func(m ast.Node) bool {
				call, ok := m.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Refused" {
					if id, ok := sel.X.(*ast.Ident); ok {
						reported[id.Name] = true
					}
				}
				return true
			})
			return true
		})
		for name, pos := range lists {
			if !reported[name] {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s:%d builds a geo blocklist (%s) and no heartbeat entry reports %s.Refused() "+
					"under heartbeat.CounterGeoBlocked; what its rules turn away would leave no trace",
					rel, fset.Position(pos).Line, name, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// One today: the collector's main. A scan that finds none checks nothing.
	if builders == 0 {
		t.Fatal("no file builds a geo blocklist; this scan is not reaching the collector")
	}
}
