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
//   - its function asks speaksForDeployment; or
//   - it writes only through Store.RecordSetting, which since W1 asks the
//     same question itself (DeploymentAccess for a deployment-wide key,
//     AccessFor for a site's) - the store, not the page, is where a write
//     that every wizard shares is refused.
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
		f, r := scanSettingWriters(t, fset, keys, name, src)
		found = append(found, f...)
		reported = append(reported, r...)
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

// scanSettingWriters classifies every setting writer in one source file:
// found names each function that writes a setting, reported each one that
// breaks the rule. Split out so the rule can be asked of a source written
// for the purpose (TestTheWriteRuleTellsAnAskingStoreFromAnUnaskingOne):
// the package itself has had no SetSetting caller since W1, and a branch
// no file reaches is a branch no mutation can be caught in.
func scanSettingWriters(t *testing.T, fset *token.FileSet, keys map[string]panel.Key, name string, src []byte) (found, reported []string) {
	t.Helper()
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
		unasked := false // a write the store does not decide for itself
		asks := false
		var named []panel.Key
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok {
					switch sel.Sel.Name {
					case "SetSetting", "SetGuardedSetting":
						writes, unasked = true, true
					case "RecordSetting":
						writes = true
					}
				}
				if id, ok := n.Fun.(*ast.SelectorExpr); ok && id.Sel.Name == "speaksForDeployment" {
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
		case len(global) == 0, asks, !unasked:
			// Per-site only, or it asks the question, or the store does.
		case deferredGlobalWrites[where] != "":
		default:
			reported = append(reported, where+" writes "+strings.Join(global, ", ")+
				", which applies to every site, without asking speaksForDeployment")
		}
	}
	return found, reported
}

// TestTheWriteRuleTellsAnAskingStoreFromAnUnaskingOne asks the rule of a
// source written for it: a deployment-wide write through SetSetting, which
// cannot ask, is reported; the same write through RecordSetting, which
// asks in the store, is not; and a SetSetting writer that asks
// speaksForDeployment itself is not either.
func TestTheWriteRuleTellsAnAskingStoreFromAnUnaskingOne(t *testing.T) {
	keys := settingKeyNames(t)
	src := []byte(`package web

func unasked(s *Server) { s.Store.SetSetting(ctx, panel.KeyBeaconSites, "", v, nil) }
func recorded(s *Server) { s.Store.RecordSetting(ctx, p, panel.KeyBeaconSites, "", v) }
func asked(s *Server) {
	if s.speaksForDeployment(ctx, p) {
		s.Store.SetSetting(ctx, panel.KeyBeaconSites, "", v, nil)
	}
}
`)
	found, reported := scanSettingWriters(t, token.NewFileSet(), keys, "fixture.go", src)
	if len(found) != 3 {
		t.Fatalf("found %v, want the three writers", found)
	}
	if len(reported) != 1 || !strings.HasPrefix(reported[0], "fixture.go:unasked ") {
		t.Errorf("reported %q, want only fixture.go:unasked", reported)
	}
}

// deferredGlobalWrites are the writers of a global setting that do not
// ask H10's question, each with the decision that left them.
//
// Empty since W1. Its one entry was the setup wizard's site list, excused
// because the technical door asked ownsAnySite - a gate the owner had
// decided to take in a turn of its own. The live rig then measured what
// that gate let through (the owner of one site cut the list to her own,
// and the beacon refused the other customer's events), and the door, the
// wizard and RecordSetting itself now ask the deployment's question.
var deferredGlobalWrites = map[string]string{}

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

// TestEverySettingWriteHereLeavesARecord (H13).
//
// The store has two kinds of write: SetSetting and SetGuardedSetting,
// which store a value and nothing else, and ApplySetting, ClearSetting
// and RecordSetting, which also write the audit row - who changed what
// from what. Every form in this package is somebody changing the
// deployment, so it takes the second kind. Four took the first: the
// setup wizard's site list (the beacon's allowlist, and on the live rig
// the installer wrote it after the handover with nothing to show for it),
// its visibility step, and the welcome wizard's site names and time zone.
func TestEverySettingWriteHereLeavesARecord(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "SetSetting", "SetGuardedSetting", "ResetGuardedSetting":
				t.Errorf("%s: %s writes a setting and records nothing; use RecordSetting "+
					"(a form that decides its own entitlement) or ApplySetting/ClearSetting "+
					"(the settings page's path)", fset.Position(call.Pos()), sel.Sel.Name)
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no source files read; the check reads nothing and so reports nothing")
	}
}
