package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// The container suite's log dump has to be registered before the step
// that fails.
//
// # The three nights this is written after
//
// nightly.yml went red on 4, 5 and 6 September with the same line:
//
//	docker_test.go:73: docker compose up failed: exit status 1
//	Container ca-e2e-init-1  service "init" didn't complete successfully: exit 1
//
// and, under it, forty lines of TimescaleDB's start-up chatter. The init
// container's own message - one sentence naming the fault - was not in
// the report on any of the three nights.
//
// It was not missing because nobody had written a good dump. A cleanup
// that prints every service's last two hundred lines was already there,
// added after an earlier night where sixty lines had not been enough. It
// sat six lines below the `up` call, and `up` is the step that fails.
// t.Fatalf ends the function, so a t.Cleanup written below it is never
// registered - not "registered and skipped", never reached. The failing
// path fell back to a smaller inline dump, truncated to the last forty
// lines of all services run together, which the database wins every
// time.
//
// So: the good diagnostic existed, the failure could not reach it, and
// three nights were spent on a report that could not name its own cause.
//
// *Bir teşhis, ona ulaşamayan bir yol için yok demektir.*
//
// # What is checked, and why this shape
//
// One ordering: in every function that brings the compose project up,
// the compose-log cleanup is registered before the `up` call. Not "no
// fatal before the cleanup" - there is a legitimate one, the failure to
// write the override file, which happens before any container exists
// and has nothing to dump.
//
// Both sides are read out of the syntax tree. The functions are found by
// looking for the `up` call rather than by name, so a second suite that
// starts a stack of its own is in scope the moment it is written, and
// the cleanup is found by what it runs - a compose `logs` command -
// rather than by a comment or a variable name.
func TestTheComposeLogDumpIsRegisteredBeforeTheStackStarts(t *testing.T) {
	path := filepath.Join(repoRoot(t), "e2e", "docker_test.go")

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	checked := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		upAt := firstComposeCall(fn.Body, "up")
		if !upAt.IsValid() {
			continue
		}
		checked++

		logsAt := composeLogCleanup(fn.Body)
		if !logsAt.IsValid() {
			t.Errorf("%s starts the compose project at %s and never registers a "+
				"cleanup that prints its logs.\nA failure there reports only what "+
				"`up` itself said, which names the service that exited and not why",
				fn.Name.Name, fset.Position(upAt))
			continue
		}
		if logsAt > upAt {
			t.Errorf("%s registers its compose-log cleanup at %s, below the `up` call "+
				"at %s.\n`up` is the step that fails, t.Fatalf ends the function, and a "+
				"t.Cleanup below it is never registered - so the run that most needs "+
				"these logs is the one run that does not get them",
				fn.Name.Name, fset.Position(logsAt), fset.Position(upAt))
		}
	}

	if checked == 0 {
		t.Fatalf("%s has no function that runs a compose `up`, so this test read "+
			"nothing. Either the suite moved or the call is spelled a way this "+
			"cannot see", path)
	}
}

// firstComposeCall returns the position of the first `…command("<verb>", …)`
// call in a body, or the zero position.
//
// The receiver is not checked. `command` with a compose verb as its
// first argument is specific enough here, and pinning the receiver's
// name would make this fail the day the helper is renamed - which is a
// rename, not a lost diagnostic.
func firstComposeCall(body *ast.BlockStmt, verb string) token.Pos {
	found := token.NoPos
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "command" || len(call.Args) == 0 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if lit.Value != `"`+verb+`"` {
			return true
		}
		if !found.IsValid() || call.Pos() < found {
			found = call.Pos()
		}
		return true
	})
	return found
}

// composeLogCleanup returns the position of the first
// `t.Cleanup(func(){ … command("logs", …) … })` in a body.
//
// What identifies it is the command it runs, not its comment and not
// the variable it assigns to. A cleanup that has stopped reading the
// logs has stopped being this cleanup, whatever it is still called.
func composeLogCleanup(body *ast.BlockStmt) token.Pos {
	found := token.NoPos
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Cleanup" || len(call.Args) != 1 {
			return true
		}
		lit, ok := call.Args[0].(*ast.FuncLit)
		if !ok {
			return true
		}
		if !firstComposeCall(lit.Body, "logs").IsValid() {
			return true
		}
		if !found.IsValid() || call.Pos() < found {
			found = call.Pos()
		}
		return true
	})
	return found
}
