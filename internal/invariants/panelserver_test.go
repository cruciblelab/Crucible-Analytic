package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel/web"
)

// A server field nobody fills is a feature that does nothing - the
// preflight.Config rule (preflightconfig_test.go), one level up.
//
// # The defect (H12)
//
// The live rig found that the panel's sign-in limits counted every
// visitor behind a reverse proxy as one address. The fix gives
// web.Server a ClientIP field built from panel.toml's trusted_proxies -
// and a fix whose field cmd/panel forgot to set would pass every test in
// internal/panel/web, which builds its own servers, while changing
// nothing on any deployment.
//
// Asking that question of every field found two that had been in that
// state since they were written: BeaconURL (panel.toml's beacon_url,
// documented in the example, parsed, and never passed on - so the
// welcome wizard's snippet step told every customer to ask whoever
// installed the panel) and ConfiguredTimezone (the timezone step was
// meant to show the file's value beside the stored one).
//
// # The rule
//
// Every exported field of web.Server is given a value in cmd/panel's
// &web.Server{...} literal, or by an assignment to srv.<Field> in the
// same file, or named below with the reason it needs none. Fields are
// read from the type, so a field added tomorrow is covered the day it is
// added.
func TestEveryPanelServerFieldIsFilled(t *testing.T) {
	root := repoRoot(t)
	filledElsewhere := map[string]string{
		"HealthCheckBudget": "zero means defaultHealthCheckBudget, so the bound lives in " +
			"one place; only tests shorten it",
	}

	var fields []string
	typ := reflect.TypeOf(web.Server{})
	for i := 0; i < typ.NumField(); i++ {
		if f := typ.Field(i); f.IsExported() {
			fields = append(fields, f.Name)
		}
	}
	if len(fields) == 0 {
		t.Fatal("web.Server has no exported fields; the type moved or this check stopped reading it")
	}

	set := panelServerKeys(t, filepath.Join(root, "cmd", "panel", "main.go"))
	if len(set) == 0 {
		t.Fatal("no &web.Server{...} literal found in cmd/panel/main.go; the panel is " +
			"built somewhere else now, and this check must follow it")
	}

	for _, name := range fields {
		_, exempt := filledElsewhere[name]
		switch {
		case set[name] && exempt:
			t.Errorf("web.Server.%s is both set by cmd/panel and listed as needing no value "+
				"(%q); the listing is the one a reader would trust", name, filledElsewhere[name])
		case !set[name] && !exempt:
			t.Errorf("web.Server.%s is never given a value by cmd/panel.\n"+
				"Every page and check reading it sees its zero value on every deployment, "+
				"while internal/panel/web's own tests - which build their servers themselves - "+
				"pass. Set it in cmd/panel/main.go, or add it to filledElsewhere here with "+
				"the reason zero is right.", name)
		}
	}
	byName := map[string]bool{}
	for _, name := range fields {
		byName[name] = true
	}
	for _, name := range sortedNames(filledElsewhere) {
		if !byName[name] {
			t.Errorf("filledElsewhere names %q and web.Server has no such exported field", name)
		}
	}
}

// panelServerKeys reads the fields cmd/panel sets on its web.Server: the
// keys of the &web.Server{...} literal and every srv.<Field> = ...
// assignment. Parsed, not grepped: a name in a comment sets nothing.
func panelServerKeys(t *testing.T, path string) map[string]bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	keys := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			sel, ok := n.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Server" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "web" {
				return true
			}
			for _, elt := range n.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok {
						keys[id.Name] = true
					}
				}
			}
		case *ast.AssignStmt:
			for _, lhs := range n.Lhs {
				sel, ok := lhs.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "srv" {
					keys[sel.Sel.Name] = true
				}
			}
		}
		return true
	})
	return keys
}
