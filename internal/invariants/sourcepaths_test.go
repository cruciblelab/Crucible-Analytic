package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// No text a running service can show names this repository (W4, item 3).
//
// The setup wizard told installers to run `go run ./cmd/devpass` and
// `psql -f internal/panel/schema.sql`; the health page said the heartbeat
// table "internal/heartbeat/schema.sql" might be missing; three startup
// errors said `go run ./cmd/devpass`. A customer has a release package -
// bin/, schema/, systemd/, release/install.sh - and none of those paths.
// Every one of those sentences was written by somebody looking at the
// repository, which is the one place the reader is not.
//
// The rule is on string literals and message catalogue values, which is
// where text a reader sees comes from. Comments are free to name the
// tree; they are read by people who have it.
var sourceTreeText = regexp.MustCompile(`go run |(^|[\s"'(:])(internal|cmd|deploy)/[a-z0-9_*]+/|crucible\.toml|README'deki`)

// sourceTreeExempt are directories whose strings may name the tree, with
// why.
var sourceTreeExempt = map[string]string{
	"internal/sast":        "developer tooling run from a checkout; never in the release package",
	"internal/schemafiles": "the schema files' identities, the paths build.sh copies them from",
	"internal/invariants":  "this package names the tree to check it",
}

func TestNoServiceTextNamesTheSourceTree(t *testing.T) {
	root := repoRootFromInvariants(t)
	fset := token.NewFileSet()
	scanned := 0
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.Walk(filepath.Join(root, top), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if info.IsDir() {
				if _, exempt := sourceTreeExempt[rel]; exempt {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			scanned++
			ast.Inspect(file, func(n ast.Node) bool {
				if _, ok := n.(*ast.ImportSpec); ok {
					return false
				}
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				if m := sourceTreeText.FindString(value); m != "" {
					t.Errorf("%s: a string names the source tree (%q). A customer has the "+
						"release package, not this repository: name bin/, schema/ or "+
						"release/install.sh instead", fset.Position(lit.Pos()), strings.TrimSpace(m))
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 100 {
		t.Fatalf("scanned %d Go files; the walk is not seeing the tree", scanned)
	}

	catalogs, err := filepath.Glob(filepath.Join(root, "internal", "panel", "ui", "messages", "*.toml"))
	if err != nil || len(catalogs) == 0 {
		t.Fatalf("no message catalogues found (%v)", err)
	}
	for _, path := range catalogs {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "#") {
				continue
			}
			if m := sourceTreeText.FindString(line); m != "" {
				t.Errorf("%s:%d: a message names the source tree (%q)", filepath.Base(path), i+1,
					strings.TrimSpace(m))
			}
		}
	}
}
