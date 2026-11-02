package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// Every write of a deployment-wide setting from this package asks H10's
// question, or is listed here with the reason it does not.
//
// # Why
//
// H10 put the rule in panel.Access.AccessTo, and the settings page's one
// write path - Store.ApplySetting and ClearSetting - asks it. Store.
// SetSetting does not: it takes an actor id rather than an Access, so it
// cannot. Any handler that calls it with a global key is a second door
// around the rule, and the welcome wizard's time zone was one - the owner
// of one customer's site could move every site's clock. The rule written
// once is the rule written for the doors that existed that day.
//
// # The conditions
//
// A call is fine when either is readable off the source:
//
//   - every setting key its function names is per-site (resolved against
//     the registry, so a key whose scope changes moves the call into
//     question); or
//   - its function asks mayChangeDeploymentSettings.
//
// Otherwise it is reported, unless it is in deferredGlobalWrites with its
// reason. A name on that list is a claim about a function on the day it
// was typed, so the list is kept to what a decision put there.
func TestEveryDeploymentWideWriteAsksWhoOwnsTheDeployment(t *testing.T) {
	keys := settingKeyNames(t)
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var found, reported []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			writes := false
			asks := false
			var named []panel.Key
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.CallExpr:
					if sel, ok := n.Fun.(*ast.SelectorExpr); ok {
						switch sel.Sel.Name {
						case "SetSetting", "SetGuardedSetting":
							writes = true
						}
					}
					if id, ok := n.Fun.(*ast.SelectorExpr); ok && id.Sel.Name == "mayChangeDeploymentSettings" {
						asks = true
					}
				case *ast.SelectorExpr:
					if pkg, ok := n.X.(*ast.Ident); ok && pkg.Name == "panel" {
						if key, ok := keys[n.Sel.Name]; ok {
							named = append(named, key)
						}
					}
				}
				return true
			})
			if !writes {
				continue
			}
			where := name + ":" + fn.Name.Name
			found = append(found, where)

			global := []string{}
			for _, key := range named {
				def, ok := panel.DefinitionFor(key)
				if !ok || def.Scope != panel.ScopeSite {
					global = append(global, string(key))
				}
			}
			switch {
			case len(named) == 0:
				reported = append(reported, where+" writes a setting it does not name; "+
					"the key cannot be read off the source, so it cannot be shown to be per-site")
			case len(global) == 0, asks:
				// Per-site only, or it asks the question.
			case deferredGlobalWrites[where] != "":
			default:
				reported = append(reported, where+" writes "+strings.Join(global, ", ")+
					", which applies to every site, without asking mayChangeDeploymentSettings")
			}
		}
	}
	sort.Strings(reported)
	for _, r := range reported {
		t.Error(r)
	}

	// The scan reached the writers it is about: one per condition, and
	// every excused one still exists - an excuse for a function that is
	// gone is a name that would excuse the next one given it.
	for _, want := range []string{
		"welcome.go:saveSiteNames",  // per-site
		"welcome.go:saveTimezone",   // asks
		"visibility.go:saveVisible", // per-site, keys named in a map
	} {
		if !slices.Contains(found, want) {
			t.Errorf("the scan did not find %s; it is not reaching the writers it guards", want)
		}
	}
	for where := range deferredGlobalWrites {
		if !slices.Contains(found, where) {
			t.Errorf("deferredGlobalWrites excuses %s, which writes no setting any more; remove it", where)
		}
	}
}

// deferredGlobalWrites are the writers of a global setting that do not
// ask H10's question, each with the decision that left them.
var deferredGlobalWrites = map[string]string{
	// The technical setup wizard's site list (beacon.sites). The wizard is
	// the developer's; an owner reaches it only through the technical
	// door, which asks ownsAnySite - one of the deployment-wide gates the
	// owner decided (2026-11-02) to take in a turn of its own rather than
	// with the settings. PLAN H10 lists it first.
	"setup.go:saveSites": "H10: the owner's decision, the other gates later",
}

// settingKeyNames maps each exported Key constant of internal/panel to its
// value, read from that package's source.
func settingKeyNames(t *testing.T) map[string]panel.Key {
	t.Helper()
	out := map[string]panel.Key{}
	files, err := filepath.Glob("../*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				typ, ok := vs.Type.(*ast.Ident)
				if !ok || typ.Name != "Key" {
					continue
				}
				for i, id := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					out[id.Name] = panel.Key(value)
				}
			}
		}
	}
	// The resolution reached the registry: a key every reader knows.
	if out["KeyBeaconSites"] != panel.KeyBeaconSites {
		t.Fatalf("reading internal/panel's Key constants did not find KeyBeaconSites; found %d", len(out))
	}
	return out
}
