package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// Every service that writes an address reads privacy.ip_storage from the
// panel, and applies it.
//
// # What went wrong
//
// One of the two did. The beacon took the setting live in A6; the
// collector read ip_storage from its config file at startup and never
// again, while the panel displayed the setting as Live and the beacon
// honoured it. So a customer clicking masked or full moved one writer of
// the crossover join and left the other where it was.
//
// The two encodings never compare equal - the join keys on
// COALESCE(ip_hash, inet_send(ip)), and a token is not a network - so
// the two tables shared no keys at all: coverage 0%, every beacon
// address counted as one the collector never saw, and the panel
// explaining that as "the collector is not in the path". A diagnosis
// pointing at the network for a defect in a setting.
//
// The direction that reaches a visitor is worse, and it is the reachable
// one: full mode cannot be selected from the panel at all today (see
// PLAN.md §P5a for that separate finding), but masked can. A deployment
// installed with ip_storage = "full" in both files, whose customer
// selects masked, got a beacon that stopped tokenising and a collector
// that did not - under a disclosure page, derived from the beacon,
// saying only the masked network was kept.
//
// # Why this is structural and not only measured
//
// internal/storage measures the chain end to end against a real
// database: a mode stored by the panel changes the column the collector
// writes. What that measurement cannot see is the call site in
// cmd/collector - it resolves the mode itself, because a test cannot
// reach a closure inside main(). Delete the two lines from the binary
// and every functional test still passes.
//
// So the rule here is about the shape of the source: each writer
// declares a live resolution for the mode, and its binary both resolves
// and applies it. Neither half is enough, and the defect was exactly one
// half missing.
//
// # Why the list of writers is derived
//
// Not named. A writer is a package whose config declares a field tagged
// `toml:"ip_storage"` - which is what "this service decides how much of
// an address to store" looks like in this tree. A third one added
// tomorrow is held to the same rule without anybody remembering to come
// back here, and that is the failure mode this package exists for: three
// packages remembered, the fourth did not.
func TestEveryAddressWriterTakesTheIPModeFromThePanel(t *testing.T) {
	root := repoRootFromInvariants(t)

	writers := addressWriters(t, root)
	// Two today. Asserted so that a scan which stops finding them - a
	// renamed tag, a moved config file - fails instead of passing by
	// checking nothing. The number is the only hand-kept thing in this
	// test, and it is here rather than in place of the derivation.
	if len(writers) < 2 {
		t.Fatalf("found %d services reading ip_storage from a config file (%v); "+
			"the collector and the beacon both do, so this scan is not reaching them",
			len(writers), writers)
	}

	for _, pkg := range writers {
		t.Run(pkg, func(t *testing.T) {
			// Half one: the config package can resolve the setting from
			// the panel's table at all.
			if !resolvesIPModeLive(t, filepath.Join(root, "internal", pkg)) {
				t.Errorf("internal/%s reads ip_storage from its config file and has no "+
					"method resolving settings.KeyPrivacyIPStorage from a *settings.Source.\n"+
					"A writer that can only be told by its file diverges from one that "+
					"can be told by the panel, and the crossover join between them then "+
					"has two key spaces and no overlap.", pkg)
			}

			// Half two: the binary resolves it on every poll and hands it
			// to whatever does the writing.
			//
			// cmd/<pkg> by convention. A writer whose binary is not
			// there is reported as a question rather than passed over:
			// this test cannot tell "wired somewhere else" from "not
			// wired", and the second is what it was written to catch.
			main := filepath.Join(root, "cmd", pkg)
			if _, err := os.Stat(main); err != nil {
				t.Errorf("internal/%s is an address writer and cmd/%s does not exist, so "+
					"this test cannot see whether anything applies the setting: %v",
					pkg, pkg, err)
				return
			}
			resolves, applies := ipModeWiring(t, main)
			if !resolves {
				t.Errorf("cmd/%s never calls Privacy.Live, so the setting reaches this "+
					"process only through its config file", pkg)
			}
			if !applies {
				t.Errorf("cmd/%s resolves the mode and never calls SetIPMode, so the "+
					"value is computed and dropped.\n"+
					"A setting the panel calls live, that a process reads and does not "+
					"apply, is worse than one it never reads: the panel says it took "+
					"effect.", pkg)
			}
		})
	}
}

// addressWriters lists the internal packages whose config declares an
// ip_storage key.
func addressWriters(t *testing.T, root string) []string {
	t.Helper()

	var found []string
	entries, err := os.ReadDir(filepath.Join(root, "internal"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, "internal", entry.Name())
		if declaresIPStorageTag(t, dir) {
			found = append(found, entry.Name())
		}
	}
	return found
}

// declaresIPStorageTag reports whether any struct field in this package
// is tagged toml:"ip_storage".
func declaresIPStorageTag(t *testing.T, dir string) bool {
	t.Helper()

	var found bool
	for _, file := range goFilesIn(t, dir) {
		ast.Inspect(file, func(n ast.Node) bool {
			field, ok := n.(*ast.Field)
			if !ok || field.Tag == nil {
				return true
			}
			// Unquoted first: a struct tag's literal carries its
			// backquotes, and reflect.StructTag cannot read them.
			raw, err := strconv.Unquote(field.Tag.Value)
			if err != nil {
				return true
			}
			if reflect.StructTag(raw).Get("toml") == "ip_storage" {
				found = true
			}
			return true
		})
	}
	return found
}

// resolvesIPModeLive reports whether the package has a function that
// takes a *settings.Source and names the ip_storage key.
//
// Both conditions, because either alone is satisfied by something else:
// plenty of functions here take a Source, and the key is named by the
// definition in internal/panel too. What identifies a live resolution is
// a function that is handed the panel's table and asks it for this
// particular key.
func resolvesIPModeLive(t *testing.T, dir string) bool {
	t.Helper()

	for _, file := range goFilesIn(t, dir) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !takesSettingsSource(fn) {
				continue
			}
			var names bool
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if ok && sel.Sel.Name == "KeyPrivacyIPStorage" {
					names = true
				}
				return true
			})
			if names {
				return true
			}
		}
	}
	return false
}

func takesSettingsSource(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, param := range fn.Type.Params.List {
		star, ok := param.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		if sel, ok := star.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "Source" {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "settings" {
				return true
			}
		}
	}
	return false
}

// ipModeWiring reports whether a binary resolves the mode and applies
// it.
//
// By call name rather than by type, because the two receivers differ -
// the beacon hands it to a *beacon.Server and the collector to a
// *storage.Flusher - and what has to be true is the same of both: the
// value is fetched from the panel's source and handed to the thing that
// writes rows.
func ipModeWiring(t *testing.T, dir string) (resolves, applies bool) {
	t.Helper()

	for _, file := range goFilesIn(t, dir) {
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
			case "SetIPMode":
				applies = true
			case "Live":
				// Privacy.Live, not any Live: LiveLimits and friends are
				// separate methods, but a plain Live on some other
				// config section would be indistinguishable here without
				// looking at what it is called on.
				if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Privacy" {
					resolves = true
				}
			}
			return true
		})
	}
	return resolves, applies
}

// goFilesIn parses every non-test .go file directly in dir.
//
// Not recursive: a config file sits in its package's own directory, and
// walking down would let a subpackage's compliance stand in for its
// parent's.
func goFilesIn(t *testing.T, dir string) []*ast.File {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", filepath.Join(dir, name), err)
		}
		files = append(files, file)
	}
	return files
}
