package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every service that writes rows honours a recording pause (PLAN §4,
// #3), and says so.
//
// # Why this is structural
//
// The pause reaches a writer through a closure in its main(): the
// settings poll reads the pause for the site and hands it to the writer.
// internal/storage and internal/beacon measure the writers themselves - a
// paused flush writes nothing, a paused site's event is not queued - and
// what those measurements cannot see is whether a binary ever tells its
// writer. Delete the two lines from a main and every functional test
// still passes, while the panel shows a pause the service ignores.
//
// # Why the writers are derived
//
// A writer is whatever reports rows written in its heartbeat, because
// that is what "this service puts rows in a table" looks like to the
// panel. Two rules, both from the tree:
//
//   - every map literal that carries heartbeat.CounterWritten carries
//     heartbeat.CounterPaused beside it - the key whose presence tells
//     the panel the build honours a pause;
//   - every command whose heartbeat reports rows written, directly or
//     through a package's counter function, reads the pause setting and
//     hands it on: SetPaused on the server that answers the events, or
//     SetUntil on a pause that is also given to a writer as its Pause.
//
// The last clause is the collector's shape, and the one a first version
// of this test missed: the pause is its own value, made before the
// heartbeat and the flusher, so a main that set it and reported it but
// never handed it to the flusher passed - a pause read, counted at zero,
// and never applied.
func TestEveryRowWriterHonoursTheRecordingPause(t *testing.T) {
	root := repoRootFromInvariants(t)
	fset := token.NewFileSet()

	// Rule one, over every package.
	var counterMaps int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "testdata", "scratchpad", "dist":
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
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			keys := map[string]bool{}
			for _, elt := range lit.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if sel, ok := kv.Key.(*ast.SelectorExpr); ok {
						keys[sel.Sel.Name] = true
					}
				}
			}
			if keys["CounterWritten"] {
				counterMaps++
				if !keys["CounterPaused"] {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s:%d reports rows written without heartbeat.CounterPaused; the panel "+
						"reads that key's absence as a build that goes on recording through a pause",
						rel, fset.Position(lit.Pos()).Line)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Two today: the collector's map in its main, the beacon's in its
	// package. A scan that stops finding them checks nothing.
	if counterMaps < 2 {
		t.Fatalf("found %d heartbeat maps reporting rows written; the collector and the beacon "+
			"both have one, so this scan is not reaching them", counterMaps)
	}

	// Rule two, over the commands.
	mains, err := filepath.Glob(filepath.Join(root, "cmd", "*", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	var writers []string
	for _, path := range mains {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var reportsWritten, readsPause, setsServer bool
		setOn := map[string]bool{}   // receivers of SetUntil
		givenAs := map[string]bool{} // values of a Pause: field
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				switch node.Sel.Name {
				case "CounterWritten", "HeartbeatCounters":
					reportsWritten = true
				case "KeyCollectionPausedUntil":
					readsPause = true
				}
			case *ast.CallExpr:
				if sel, ok := node.Fun.(*ast.SelectorExpr); ok {
					switch sel.Sel.Name {
					case "SetPaused":
						setsServer = true
					case "SetUntil":
						if id, ok := sel.X.(*ast.Ident); ok {
							setOn[id.Name] = true
						}
					}
				}
			case *ast.KeyValueExpr:
				key, ok := node.Key.(*ast.Ident)
				value, isIdent := node.Value.(*ast.Ident)
				if ok && isIdent && key.Name == "Pause" {
					givenAs[value.Name] = true
				}
			}
			return true
		})
		handsOn := setsServer
		for name := range setOn {
			handsOn = handsOn || givenAs[name]
		}
		if !reportsWritten {
			continue
		}
		rel, _ := filepath.Rel(root, path)
		writers = append(writers, rel)
		if !readsPause {
			t.Errorf("%s reports rows written and never reads settings.KeyCollectionPausedUntil: "+
				"a pause set in the panel would not reach it", rel)
		}
		if !handsOn {
			t.Errorf("%s hands no pause to its writer - no SetPaused, and no SetUntil on a value "+
				"that is also some writer's Pause: the setting would be read and dropped", rel)
		}
	}
	if len(writers) < 2 {
		t.Fatalf("found %d commands that report rows written (%v); the collector and the beacon "+
			"both do, so this scan is not reaching them", len(writers), writers)
	}
}
