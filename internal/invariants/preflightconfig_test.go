package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel/preflight"
)

// A check nobody gives an address to is a check that does not exist.
//
// # The defect
//
// preflight.Config has a ServiceURLs field, and checkService - the one
// check that asks whether the collector, the beacon and the API are
// actually answering - is driven entirely by it. The field was written,
// the check was written, the check was tested, and cmd/panel never
// passed anything. So the setup wizard ran fourteen checks about the
// schema, the roles, the disk and the log tree, and not one request to
// a service, and the reason was one absent line in a struct literal.
//
// Nothing could have noticed. The checks are built from the Config, so
// an unset field does not produce a failing check - it produces no
// check, and a list cannot show the absence of an entry nobody wrote.
//
// # The rule
//
// Every field of preflight.Config is either filled where the panel is
// built from its config file, or named below with the reason it is not.
// The list of fields is read from the type, so a field added tomorrow
// is covered the day it is added, and the literal is read from the
// source, so a line deleted tomorrow fails here rather than going
// quiet.
func TestEveryPreflightConfigFieldIsFilledSomewhere(t *testing.T) {
	root := repoRoot(t)

	// filledElsewhere are the fields cmd/panel does not pass, each with
	// the reason. A reason is not decoration: three of these four are
	// "zero means a documented default" and the fourth is "another
	// layer supplies it", and only the second kind would be a defect if
	// the other layer stopped.
	filledElsewhere := map[string]string{
		"HTTPClient": "nil means a client with a short timeout, which Run installs; " +
			"a wizard must not hang because a service is wedged rather than down",
		"Now": "nil means time.Now; only tests supply a clock",
		"MinFreeBytes": "zero means preflight.DefaultMinFreeBytes, so the floor lives " +
			"in one place rather than in a config file nobody would tune",
		"DeveloperGate": "filled by internal/panel/web.Server.preflightConfig from the " +
			"gate the server already holds; the config file carries the hash, not the gate",
	}

	var fields []string
	typ := reflect.TypeOf(preflight.Config{})
	for i := 0; i < typ.NumField(); i++ {
		fields = append(fields, typ.Field(i).Name)
	}
	if len(fields) == 0 {
		t.Fatal("preflight.Config has no fields; either the type moved or this check " +
			"stopped reading it, and a check that reads nothing reports nothing")
	}

	passed := preflightConfigKeys(t, filepath.Join(root, "cmd", "panel", "main.go"))
	if len(passed) == 0 {
		t.Fatal("no preflight.Config literal found in cmd/panel/main.go; either the " +
			"wizard is no longer configured there - in which case this check must " +
			"follow it - or nothing is configured at all")
	}

	for _, name := range fields {
		if passed[name] {
			if why, exempt := filledElsewhere[name]; exempt {
				t.Errorf("preflight.Config.%s is both passed by cmd/panel and listed as "+
					"filled elsewhere (%q). One of the two is out of date, and the "+
					"listing is the one a reader would trust.", name, why)
			}
			continue
		}
		if _, exempt := filledElsewhere[name]; exempt {
			continue
		}
		t.Errorf("preflight.Config.%s is never given a value.\n"+
			"cmd/panel builds the Config the setup wizard runs with, and the checks "+
			"are built from it - so a field nobody fills does not produce a failing "+
			"check, it produces no check at all. Either pass it there from panel.toml, "+
			"or add it to filledElsewhere in this test with the reason it needs no "+
			"value.", name)
	}

	// The other direction: an exemption for a field that no longer
	// exists is a sentence a reader would believe about code that is
	// gone.
	byName := map[string]bool{}
	for _, name := range fields {
		byName[name] = true
	}
	for _, name := range sortedNames(filledElsewhere) {
		if !byName[name] {
			t.Errorf("filledElsewhere names %q and preflight.Config has no such field; "+
				"the field was renamed or removed and this reason outlived it", name)
		}
	}
}

// preflightConfigKeys reads the field names given in the
// preflight.Config literal in one file.
//
// Parsed rather than grepped: a name inside a comment or a string is not
// a field being set, and this check exists because of a field nobody
// set.
func preflightConfigKeys(t *testing.T, path string) map[string]bool {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	keys := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Config" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "preflight" {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if ident, ok := kv.Key.(*ast.Ident); ok {
				keys[ident.Name] = true
			}
		}
		return true
	})
	return keys
}

func sortedNames(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
