package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// An account's sign-in budget is reset in one place: the step that
// establishes the session.
//
// # The defect this exists for
//
// The password, the second-factor code and the recovery code are three
// doors into one account and are counted against one budget. The
// password step used to reset that budget the moment the password was
// right - before the code had been asked for - so somebody holding the
// password could type seven wrong codes, sign in with the password again,
// and type seven more. Measured on 2026-09-28: seventy codes checked in
// 1.6 seconds, against a limit of eight per fifteen minutes. A six-digit
// code falls to that in about two hours, and the second factor was
// decoration for exactly the person it exists to stop.
//
// Nothing failed. Every step was correct on its own; the defect was where
// the reset stood. So the rule is about where: a reset anywhere but
// completeLogin is a reset an attacker can reach by repeating the step
// before it.
//
// # What is checked
//
// Every call to ClearLoginFailures in the tree, by enclosing function. A
// second caller is the failure; so is the one disappearing, because then
// nothing resets the budget and somebody who fumbled once carries it into
// every later visit. (A lift by a site's owner, catalogue #25, forgets
// failures too, and does it inside its own transaction in
// internal/panel - not through this call - with a limit of its own.)
//
// *Bütçeyi sıfırlayan adım, saldırganın tekrarlayabildiği bir adımsa,
// bütçe yoktur.*
func TestOnlyACompletedSignInResetsTheSignInBudget(t *testing.T) {
	const want = "internal/panel/web.completeLogin"

	root := repoRoot(t)
	fset := token.NewFileSet()
	var callers []string
	for _, dir := range sourceRoots {
		err := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			pkg := filepath.ToSlash(filepath.Dir(rel))
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				// Closures inside a function are that function's calls: a
				// reset handed to a helper still runs where the function
				// runs.
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "ClearLoginFailures" {
						callers = append(callers, pkg+"."+fn.Name.Name)
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(callers)

	if len(callers) != 1 || callers[0] != want {
		t.Errorf("ClearLoginFailures is called from %v; want exactly %s.\n"+
			"The budget covers the password, the code and the recovery code together, "+
			"and any step before the session exists can be repeated by whoever is at "+
			"the form - a reset there hands them a fresh budget for the next door.",
			callers, want)
	}
}
