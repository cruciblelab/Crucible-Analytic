package settings

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// The key strings in this package are a hand copy of the panel's, and
// that duplication is deliberate: internal/panel's registry must not be
// dragged into the traffic path, so the services carry their own
// constants.
//
// What was missing is the half that makes a deliberate copy safe. A key
// that differs by one character is a setting a customer changes in the
// panel and the service never reads: no error, no log line, no failed
// write - the value is stored, the page says saved, and nothing happens.
// That is the exact shape this project has spent its time hunting, and
// it was sitting in a package with no test file at all.

// TestEveryServiceKeyIsARealSetting.
//
// Reads this package's own source rather than a list written beside it.
// A list would be a third copy of the same strings, and the third copy
// is the one nobody updates.
//
// One direction only, and the asymmetry is the point. A service constant
// naming a key the panel does not define is silently broken. The reverse
// - a registry key no service reads - is a normal state: most settings
// are read by the panel itself, and several are read at startup rather
// than live.
func TestEveryServiceKeyIsARealSetting(t *testing.T) {
	// The whole package, not live.go by name. A filename written into a
	// test is a list of one, and a constant added in a new file beside it
	// would be a key this check had simply never heard of - which is the
	// failure above, arrived at from the other side.
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing this package: %v", err)
	}

	found := map[string]string{} // constant name -> key string
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				spec, ok := n.(*ast.ValueSpec)
				if !ok {
					return true
				}
				for i, name := range spec.Names {
					if len(name.Name) < 4 || name.Name[:3] != "Key" || i >= len(spec.Values) {
						continue
					}
					lit, ok := spec.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					found[name.Name] = value
				}
				return true
			})
		}
	}

	if len(found) == 0 {
		t.Fatal("no Key constants found in this package; this test would pass by checking " +
			"nothing, which is how it would look on the day somebody renamed them")
	}

	for name, key := range found {
		if _, ok := panel.DefinitionFor(panel.Key(key)); !ok {
			t.Errorf("settings.%s is %q and the panel has no setting by that name.\n"+
				"A service reading it gets the fallback for ever: the panel cannot "+
				"write a key it does not define, so the value never changes and "+
				"nothing anywhere says why", name, key)
		}
	}
}
