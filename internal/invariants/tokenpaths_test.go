package invariants

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The panel routes whose path is a credential - registered with a
// {token...} wildcard - and the list internal/panel/web hides from its
// logs (tokenPathPrefixes) have to be the same set, both ways.
//
// Measured before the list existed: all three link tokens were in the
// access log in plain text, and one reached panel_logs through the
// deadline's WARN line. A route added tomorrow with a token in its path
// and without an entry in the list would do the same, and nothing would
// say so - the database stores only the token's hash, which is exactly
// why a plain-text copy anywhere else goes unnoticed.
func TestEveryLinkRouteIsHiddenFromTheLogs(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "internal", "panel", "web")
	registered := map[string]bool{}
	hidden := map[string]bool{}

	for _, f := range goFilesIn(t, dir) {
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				// mux.HandleFunc(Prefix+"{token...}", ...) and mux.Handle.
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle") || len(node.Args) == 0 {
					return true
				}
				bin, ok := node.Args[0].(*ast.BinaryExpr)
				if !ok || bin.Op != token.ADD {
					return true
				}
				lit, ok := bin.Y.(*ast.BasicLit)
				if !ok {
					return true
				}
				pattern, err := strconv.Unquote(lit.Value)
				if err != nil || !strings.Contains(pattern, "{token") {
					return true
				}
				if id, ok := bin.X.(*ast.Ident); ok {
					registered[id.Name] = true
				} else {
					t.Errorf("a {token} route's prefix is %T, not a named constant this "+
						"check can match against the list", bin.X)
				}
			case *ast.ValueSpec:
				for i, name := range node.Names {
					if name.Name != "tokenPathPrefixes" || i >= len(node.Values) {
						continue
					}
					lit, ok := node.Values[i].(*ast.CompositeLit)
					if !ok {
						t.Fatalf("tokenPathPrefixes is not a literal list this check can read")
					}
					for _, el := range lit.Elts {
						if id, ok := el.(*ast.Ident); ok {
							hidden[id.Name] = true
						}
					}
				}
			}
			return true
		})
	}

	// Vacuity, with a number: three link routes today.
	if len(registered) < 3 {
		t.Fatalf("found %d {token} routes in internal/panel/web, expected at least 3 - "+
			"the registrations moved or changed shape", len(registered))
	}
	for name := range registered {
		if !hidden[name] {
			t.Errorf("%s is registered with a {token} wildcard and is not in tokenPathPrefixes.\n"+
				"Its token goes into the access log in plain text - the database keeps only "+
				"its hash, and a log keeps it for as long as the log is kept.", name)
		}
	}
	for name := range hidden {
		if !registered[name] {
			t.Errorf("tokenPathPrefixes names %s, which no route registers with a {token} "+
				"wildcard - a stale entry that would hide an ordinary path", name)
		}
	}
	if t.Failed() {
		var r, h []string
		for k := range registered {
			r = append(r, k)
		}
		for k := range hidden {
			h = append(h, k)
		}
		sort.Strings(r)
		sort.Strings(h)
		t.Logf("registered: %v\nhidden:     %v", r, h)
	}
}
