package web

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
	"github.com/cruciblelab/crucible-analytic/internal/upgrade"
)

// Refusals the upgrade button can produce, and the sentence each one
// turns into.
//
// The table is not the list. The list is derived below from what
// Store.RequestUpgrade actually returns, and a refusal that reaches this
// file without an entry here fails the test - which is the point: the
// way this breaks in practice is somebody adding a reason and not the
// sentence for it.
var upgradeRefusals = map[string]error{
	"ErrSettingNotWritable": panel.ErrSettingNotWritable,
	"ErrUpgradeLocked":      panel.ErrUpgradeLocked,
	"ErrUpgradeNotNeeded":   panel.ErrUpgradeNotNeeded,
	"ErrAlreadyInFlight":    upgrade.ErrAlreadyInFlight,
}

// TestEveryUpgradeRefusalGetsItsOwnSentence.
//
// # What goes wrong without it
//
// upgradeErrorText ends in a default case that says "the upgrade could
// not be requested, the detail is in the operation record". That line is
// correct for a database error nobody can act on, and useless for a
// refusal that has an answer - "you need the developer password", "one
// is already running".
//
// So a refusal added to RequestUpgrade and not added to the switch does
// not break anything visibly. It quietly downgrades a sentence somebody
// could act on into one they have to ring us about, which is exactly
// what the function's own comment says it exists to prevent.
//
// # Why the list is read out of the source
//
// A hand-written list of refusals is a list that is right on the day it
// is written. The names below come from the return statements in
// internal/panel/upgraderequest.go, so a fifth reason arrives here by
// itself.
//
// upgrade.ErrAlreadyInFlight is the one that cannot be read that way: it
// arrives through `return nil, err` after upgrade.Ask, so the source
// says only "err". It is named in the seed list instead, and the
// derivation is what proves the other three are still real.
func TestEveryUpgradeRefusalGetsItsOwnSentence(t *testing.T) {
	derived := refusalsReturnedBy(t, "RequestUpgrade")
	if len(derived) == 0 {
		t.Fatal("no refusals were read out of upgraderequest.go, so this test is " +
			"checking nothing. Has RequestUpgrade moved?")
	}

	for _, name := range derived {
		if _, ok := upgradeRefusals[name]; !ok {
			t.Errorf("RequestUpgrade can return %s and this test has no entry for it.\n"+
				"Add it to upgradeRefusals, and add a case to upgradeErrorText - "+
				"without one the caller reads the generic \"could not be requested\" "+
				"line instead of what to do about it", name)
		}
	}

	// Every refusal produces a sentence, and no two produce the same
	// one. Distinctness is what catches a copy-pasted case: a switch
	// where "locked" and "not needed" both return the locked message
	// passes every other check here.
	lang := testLanguage(t)
	generic := lang.T("saglik.yukselt.hata")
	seen := map[string]string{}
	for _, name := range sortedRefusalNames() {
		got := upgradeErrorText(lang, fmt.Errorf("wrapped: %w", upgradeRefusals[name]))
		switch {
		case got == generic:
			t.Errorf("%s falls through to the generic message: %q", name, got)
		case seen[got] != "":
			t.Errorf("%s and %s produce the same sentence: %q", name, seen[got], got)
		default:
			seen[got] = name
		}
	}
}

func sortedRefusalNames() []string {
	out := make([]string, 0, len(upgradeRefusals))
	for k := range upgradeRefusals {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// refusalsReturnedBy reads the error sentinels a function returns.
//
// Every `return nil, X` and `return nil, fmt.Errorf("...%w...", X)` in
// the named function, keeping identifiers that look like sentinels. A
// wrapped error counts as the same refusal: upgradeErrorText matches
// with errors.Is, and RequestUpgrade already wraps one of them.
func refusalsReturnedBy(t *testing.T, fnName string) []string {
	t.Helper()

	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed, so the source file cannot be found")
	}
	path := filepath.Join(filepath.Dir(here), "..", "upgraderequest.go")

	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	found := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != fnName {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ret, ok := n.(*ast.ReturnStmt)
			if !ok {
				return true
			}
			for _, r := range ret.Results {
				for _, name := range sentinelNames(r) {
					found[name] = true
				}
			}
			return true
		})
		return false
	})

	out := make([]string, 0, len(found))
	for k := range found {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// sentinelNames pulls Err-prefixed sentinels out of one return operand.
//
// A selector contributes its right half, so upgrade.ErrAlreadyInFlight
// reads as ErrAlreadyInFlight and matches the table by the name a person
// would use. fmt is skipped by package name rather than by spelling:
// fmt.Errorf starts with "Err" and was the first thing this found.
func sentinelNames(e ast.Expr) []string {
	var out []string
	ast.Inspect(e, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.SelectorExpr:
			if pkg, ok := v.X.(*ast.Ident); ok && pkg.Name == "fmt" {
				return false
			}
			if strings.HasPrefix(v.Sel.Name, "Err") {
				out = append(out, v.Sel.Name)
			}
			return false
		case *ast.Ident:
			if strings.HasPrefix(v.Name, "Err") {
				out = append(out, v.Name)
			}
		}
		return true
	})
	return out
}

// testLanguage is the base catalog, which is the one a customer reads.
func testLanguage(t *testing.T) *ui.Language {
	t.Helper()
	cats, err := ui.LoadCatalogs()
	if err != nil {
		t.Fatalf("LoadCatalogs: %v", err)
	}
	return cats.Base()
}
