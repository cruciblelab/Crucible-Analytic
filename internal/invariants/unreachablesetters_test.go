package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Nothing the panel's store can be told is told to it only by tests.
//
// # The defect this closes, and why it is a class
//
// panel.Store had a method called SetIPTokenKeyConfigured. It recorded
// whether the deployment held an IP token key, and the settings gate
// refused privacy.ip_storage = "full" unless it had been called. Two
// tests called it. No production code did, ever - so the field was false
// on every installation there has ever been, full mode could not be
// selected at all, and the setup wizard's own check for the key read the
// same field and reported "not configured" on every install, including
// the ones that had one.
//
// The tests passed. They passed *because* they were the only callers:
// each set the field, then asserted the behaviour that follows from it,
// and the behaviour followed. A gate whose input arrives only from the
// thing testing it is a gate that is always in whatever state the test
// left it.
//
// That is the second time this week's shape in this repository. The
// first was preflight.checkService, which asked for service addresses
// that cmd/panel never supplied; the invariant written then reads
// preflight.Config's *fields*, and it could not see this one, because
// this was not a field but a method. So the rule is here as well, about
// the other half of the same surface.
//
// # Why setters, and why only the panel's store
//
// Because a setter is what a deployment fact arrives through, and the
// failure is silent in one direction: unset means "we were not told",
// which the gates correctly treat as no. Every other method shape either
// returns something a caller has to use, or is called by name from a
// template, where no scan of the call graph can see it. Setters are not
// called from templates - a template renders, it does not configure - so
// this is the one shape where "no non-test caller" means what it says.
//
// The panel's store is where the deployment's own facts live, and it is
// the type both defects were on. Widening this to every type in the tree
// would need an exception list for interface implementations and
// template-called methods, and a list of names is what this package
// exists to avoid.
//
// # What the failure asks
//
// Not "add a caller". A setter nothing calls is either a fact the
// product should be reading from somewhere it can actually see - which
// is what 5b did, moving the question to the services' own heartbeat
// rows - or it is dead, and the honest fix is to delete it. A mutation
// surviving is a question, and sometimes the answer is that the code
// does nothing.
//
// # The two it found on its first run
//
// Both are in unreachableSetters below, with what was measured about
// each. They are recorded rather than fixed because neither is a line
// of wiring: one is a decision about what an unaudited settings path
// should do, the other needs a page, a capability rule and a
// last-owner rule. PLAN.md carries them; this map is what stops them
// from being forgotten quietly, which is the only outcome worse than
// leaving them open.
func TestNoStoreSetterIsCalledOnlyByTests(t *testing.T) {
	root := repoRootFromInvariants(t)

	setters := storeSetters(t, filepath.Join(root, "internal", "panel"))
	// Ten or so today. Asserted so that a scan which stops finding them
	// - a renamed receiver, a moved file - fails rather than passing by
	// examining nothing. The number is the only hand-kept thing here,
	// and it is a floor beside the derivation rather than instead of it.
	if len(setters) < 5 {
		t.Fatalf("found %d setters on panel.Store (%v); there are several, so this scan is "+
			"not reaching them", len(setters), setters)
	}

	production, tests := callersOfNames(t, root, setters)
	for _, name := range setters {
		if production[name] > 0 {
			if _, excused := unreachableSetters[name]; excused {
				t.Errorf("panel.Store.%s is listed in unreachableSetters and now has %d "+
					"production caller(s). Remove the entry - an exception that no longer "+
					"applies is a note that stops being read.", name, production[name])
			}
			continue
		}
		if reason, excused := unreachableSetters[name]; excused {
			// Logged rather than silent. An exception nobody sees is an
			// exception nobody revisits, and the point of writing the
			// reason down was that somebody would read it.
			t.Logf("panel.Store.%s has no production caller, recorded: %s", name, reason)
			continue
		}
		if tests[name] == 0 {
			// Called by nothing at all. A different sentence, because the
			// fix is not the same: this one is simply dead.
			t.Errorf(`panel.Store.%s is called by nothing, not even a test.

Dead code on the type that holds a deployment's own facts. Delete it, or
call it from wherever the fact actually arrives.`, name)
			continue
		}
		t.Errorf(`panel.Store.%s is called by %d test file(s) and by no production code.

That is what SetIPTokenKeyConfigured was, and it made privacy.ip_storage
= "full" unselectable on every deployment there has ever been while the
tests for it passed - because the tests were the only thing that ever
set it.

A gate whose input arrives only from the thing testing it is always in
whatever state the test left it. Either the product reads this fact from
somewhere it can see (5b moved the same question to the services' own
heartbeat rows), or the setter is dead and deleting it is the honest
fix.`, name, tests[name])
	}
}

// unreachableSetters are the setters with no production caller today,
// each with what was measured about it.
//
// A map with reasons rather than a list of names, which is this
// package's standing rule: an exception is allowed when it carries why.
// Both entries are open findings, not decisions - and both are the same
// shape as the defect this test exists for, which is why they surfaced
// the first time it ran.
var unreachableSetters = map[string]string{
	// The developer-access policy (ask / deny / open). Measured: the
	// setting *can* be changed, because access.developer is an ordinary
	// registered enum on the settings page, so the owner is not locked
	// out. What this method uniquely does is clear
	// access.developer_open_until when the policy leaves "open", and the
	// generic path does not - so a deployment that was open until a
	// timestamp and is now on "ask" shows that timestamp on its settings
	// page, reading as though it is one word from being open again. The
	// behaviour is display-only: DevAccessPolicyFor reads the policy
	// first and never consults the window unless it says open.
	//
	// The method's own comment also says "the generic settings path
	// writes no audit entry for anything", and that was true when it was
	// written and is not now: B2's operations channel audits every
	// change through web/settings.go's BeginOperation. So the reason the
	// method was written has gone, and only the window-clearing is left.
	//
	// Two honest fixes and the choice is the owner's: route this key
	// through the method, or delete it and let the stale timestamp show.
	// PLAN.md, open findings.
	"SetDevAccessPolicy": "no page control exists; the setting itself is reachable through " +
		"the settings page, so only the open-until clearing is unreachable (PLAN.md)",

	// Disabling an account. The login path honours it - auth.go refuses
	// a disabled user, twice - and nothing in the product can set it. So
	// an account can never be disabled, on any deployment.
	//
	// Not wired here because it is not wiring: it needs a control on the
	// members page, a rule for who may disable whom, and the last-owner
	// protection that every other member operation already has
	// (C9.1c exists because three write paths did not). Its own phase.
	"SetDisabled": "the login path refuses a disabled account but nothing can disable one; " +
		"needs a page, a capability rule and the last-owner rule (PLAN.md)",
}

// storeSetters is every Set* method on *Store in a package directory.
//
// Read from the syntax tree rather than by grep, so a method declared
// across lines or with an unusual signature is still found - and so that
// a name like SettingsView, which begins with "Set" as text, is not
// mistaken for one: the check is on the receiver and on the name's
// shape, which is Set followed by an upper-case letter.
func storeSetters(t *testing.T, dir string) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", dir, err)
	}

	seen := map[string]bool{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || len(fn.Recv.List) != 1 {
					continue
				}
				star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
				if !ok {
					continue
				}
				ident, ok := star.X.(*ast.Ident)
				if !ok || ident.Name != "Store" {
					continue
				}
				name := fn.Name.Name
				if !strings.HasPrefix(name, "Set") || len(name) < 4 {
					continue
				}
				if r := name[3]; r < 'A' || r > 'Z' {
					continue
				}
				seen[name] = true
			}
		}
	}

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// callersOfNames counts, per name, how many production files and how many
// test files mention a call to it.
//
// By selector expression, from the syntax tree of every Go file in the
// tree - so a name that only appears inside a comment or a string does
// not count as a caller. The declaration itself is not a call, and a
// FuncDecl's name is not a SelectorExpr, so it does not count either.
func callersOfNames(t *testing.T, root string, names []string) (production, tests map[string]int) {
	t.Helper()
	wanted := map[string]bool{}
	for _, n := range names {
		wanted[n] = true
	}
	production, tests = map[string]int{}, map[string]int{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// .git holds packed objects, not Go source, and walking it
			// costs seconds.
			if name := d.Name(); name == ".git" || name == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return nil // a file this build excludes is not this test's business
		}

		found := map[string]bool{}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if wanted[sel.Sel.Name] {
				found[sel.Sel.Name] = true
			}
			return true
		})

		bucket := production
		if strings.HasSuffix(path, "_test.go") {
			bucket = tests
		}
		for name := range found {
			bucket[name]++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return production, tests
}
