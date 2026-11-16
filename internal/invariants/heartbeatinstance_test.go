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
// heartbeat.Options literal in cmd/collector sets Instance - and so does
// every restartreq.Watcher, or the panel's request to restart one site's
// collector alone reaches no process (W3b).
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
			if !ok {
				return true
			}
			x, isIdent := sel.X.(*ast.Ident)
			if !isIdent || !(x.Name == "heartbeat" && sel.Sel.Name == "Options" ||
				x.Name == "restartreq" && sel.Sel.Name == "Watcher") {
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
			t.Errorf("%s: the collector's %s.%s sets no Instance - on a host with a collector "+
				"per site, its row and its restart request are not its own",
				fset.Position(lit.Pos()), x.Name, sel.Sel.Name)
			return true
		})
	}
	if found != 2 {
		t.Errorf("found %d heartbeat.Options and restartreq.Watcher literals in cmd/collector, want 2; "+
			"this check is no longer checking what it was written for", found)
	}
}
