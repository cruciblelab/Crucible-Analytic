package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A hook the shipped binary never sets is a feature nobody has.
//
// # The defect this exists because of, twice over
//
// internal/relupdate had a queue with Ask, Claim, Finish and ExpireStale,
// all tested, and nothing called Claim. The button wrote a row nobody
// read. queues_test.go is the answer to that one.
//
// This is the same shape one layer up. internal/applier now carries
// BackupFirst: a function field that, when set, takes a copy of the
// database before any DDL runs. Nil means no backup, and nil is what
// every test in that package uses on purpose - so the package is green
// either way, and so is every applier test, and so is the whole suite.
//
// The one place it has to be set is cmd/upgrader, which is the only
// binary that runs an applier. Delete that assignment and nothing goes
// red anywhere: schema upgrades quietly stop taking a backup first, and
// the way anybody finds out is an upgrade going wrong on a customer's
// machine with nothing to restore.
//
// # Why it is derived rather than listed
//
// The hooks are found by their shape - a func-typed field on Applier -
// so a second one is in scope the moment somebody writes it. A list
// would be right on the day it was written, which is the property that
// let the first queue ship with no consumer.

// applierHooks are the func-typed fields on applier.Applier.
func applierHooks(t *testing.T) []string {
	t.Helper()

	root := repoRootFromInvariants(t)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset,
		filepath.Join(root, "internal", "applier", "applier.go"), nil, 0)
	if err != nil {
		t.Fatalf("parsing internal/applier/applier.go: %v", err)
	}

	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name.Name != "Applier" {
			return true
		}
		st, ok := spec.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range st.Fields.List {
			if _, isFunc := field.Type.(*ast.FuncType); !isFunc {
				continue
			}
			for _, name := range field.Names {
				if name.IsExported() {
					out = append(out, name.Name)
				}
			}
		}
		return true
	})
	sort.Strings(out)
	return out
}

// assignedIn finds the fields something assigns on a value: `x.Field = `.
func assignedIn(t *testing.T, path string) map[string]bool {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range assign.Lhs {
			if sel, ok := lhs.(*ast.SelectorExpr); ok {
				out[sel.Sel.Name] = true
			}
		}
		return true
	})
	// A field set in the composite literal counts too, and is how
	// somebody would naturally write it.
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok {
					out[key.Name] = true
				}
			}
		}
		return true
	})
	return out
}

// TestEveryApplierHookIsSetByTheBinaryThatShipsIt.
func TestEveryApplierHookIsSetByTheBinaryThatShipsIt(t *testing.T) {
	hooks := applierHooks(t)
	if len(hooks) == 0 {
		t.Fatal("no func-typed fields were found on applier.Applier, so this check " +
			"examined nothing. If the hooks were given a different shape, this has " +
			"to change with them")
	}

	main := filepath.Join(repoRootFromInvariants(t), "cmd", "upgrader", "main.go")
	set := assignedIn(t, main)

	for _, hook := range hooks {
		if set[hook] {
			continue
		}
		t.Errorf("applier.Applier has a %s hook and cmd/upgrader never sets it.\n"+
			"Nil means the hook does nothing, and nothing goes red for that: the "+
			"applier's own tests leave it nil on purpose. An upgrade would simply "+
			"stop doing whatever %s is for, and the way anybody finds out is the "+
			"day it was needed.\n"+
			"Either set it in %s, or delete the field - a hook nobody wires is a "+
			"feature nobody has.", hook, hook, "cmd/upgrader/main.go")
	}
}

// TestTheHooksAreNamedForWhatTheyGuard is a smaller claim, and it is
// about the check above rather than about the code.
//
// The derivation finds func-typed fields. If somebody adds one that is
// an ordinary callback - a progress reporter, a clock - the check would
// demand the binary set it, which is not the rule. The names are the
// only thing distinguishing the two, so they are asserted: a hook here
// is part of the upgrade's safety, and says so.
func TestTheHooksAreNamedForWhatTheyGuard(t *testing.T) {
	for _, hook := range applierHooks(t) {
		if !strings.HasPrefix(hook, "Backup") {
			t.Errorf("applier.Applier has a func field called %s.\n"+
				"Every hook here so far is part of what makes an upgrade safe to "+
				"run, and TestEveryApplierHookIsSetByTheBinaryThatShipsIt insists "+
				"the binary sets it for that reason. If %s is something else - a "+
				"clock, a progress callback - that rule is wrong for it and both "+
				"tests need to learn the difference", hook, hook)
		}
	}
}
