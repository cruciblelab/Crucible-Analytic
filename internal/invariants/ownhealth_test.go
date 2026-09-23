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

// PLAN §Z6, held against the source.
//
// The health page's per-service numbers were a surface whose inputs
// arrived only from the tests that checked it. Measured on the real read
// API before Z6: four failed queries, a request answered by its deadline,
// a client that gave up, and three log lines lost while panel_logs was
// locked - and the service's heartbeat row said counters {} and no last
// error. heartbeat.Reporter.Note had no production caller; the sink's
// loss counter had no reader; the "hata" counter had a label and no
// producer.
//
// So three rules, one per way that went wrong:
//
//   - every heartbeat.New in a service main is given, as Options.Log, the
//     sink that main got from logsink.Attach - the row's last error and
//     log-loss count come from there;
//   - and is given Counters: every service that writes a row counts
//     something, and a reporter without its counters writes a row that
//     says nothing about the service's work;
//   - every Counter constant internal/heartbeat declares is produced
//     somewhere outside the tests - put into a counters map by code that
//     runs. A label with no producer is a line the page promises and
//     never draws.
//
// The first version of this file had a fourth rule, for the panel's
// web.Server, because the panel wrote no heartbeat and the page drew its
// row from the process. That was a defect of its own (PLAN §V4b): the
// panel writes a row now, and falls under the first two rules like the
// others.

func TestEveryServicesHealthRowReadsItsLogCopy(t *testing.T) {
	root := repoRoot(t)
	beats := 0

	for _, name := range serviceBinaries(t, root) {
		for _, f := range goFilesIn(t, filepath.Join(root, "cmd", name)) {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name.Name != "main" || fn.Recv != nil || fn.Body == nil {
					continue
				}
				sinks := attachedSinks(fn.Body, localName(f, logsinkPath))

				for _, call := range callsOf(fn.Body, localName(f, heartbeatPath), "New") {
					beats++
					opts := optionsLiteral(call)
					if !fieldIsOneOf(opts, "Log", sinks) {
						t.Errorf("cmd/%s: heartbeat.New's Options.Log is not the sink main got from "+
							"logsink.Attach.\nThe row then carries no last error and no log-loss "+
							"count - measured before Z6, the read API's row said counters {} and "+
							"nothing else after four failed queries.", name)
					}
					if !hasField(opts, "Counters") {
						t.Errorf("cmd/%s: heartbeat.New is given no Counters.\nThe row then "+
							"says nothing about the service's own work - the read API's row "+
							"carried none at all until Z6.", name)
					}
				}
			}
		}
	}

	// Vacuity: four services write a heartbeat - every one the restarter
	// restarts. Fewer means the wiring moved where this cannot follow it,
	// or a service stopped reporting; restartbeats_test.go says which.
	if beats < 4 {
		t.Errorf("found %d heartbeat.New calls in service mains, want at least 4", beats)
	}
}

func TestEveryHeartbeatCounterHasAProducer(t *testing.T) {
	root := repoRoot(t)
	declared := heartbeatCounterConsts(t, root)

	produced := map[string][]string{}
	pt := parseTree(t, root)
	for dir, files := range pt.byDir {
		inHeartbeat := dir == "internal/heartbeat"
		for _, f := range files {
			hb := localName(f, heartbeatPath)
			name := func(e ast.Expr) string {
				switch k := e.(type) {
				case *ast.SelectorExpr:
					if pkg, ok := k.X.(*ast.Ident); ok && hb != "" && pkg.Name == hb {
						return k.Sel.Name
					}
				case *ast.Ident:
					if inHeartbeat {
						return k.Name
					}
				}
				return ""
			}
			record := func(e ast.Expr) {
				if n := name(e); declared[n] {
					produced[n] = append(produced[n], dir)
				}
			}
			ast.Inspect(f, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CompositeLit:
					// A key in a map literal: the shape every service's
					// Counters function returns.
					if _, isMap := x.Type.(*ast.MapType); !isMap {
						return true
					}
					for _, el := range x.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							record(kv.Key)
						}
					}
				case *ast.AssignStmt:
					// m[Counter] = v: how the reporter adds the loss count.
					for _, lhs := range x.Lhs {
						if ix, ok := lhs.(*ast.IndexExpr); ok {
							record(ix.Index)
						}
					}
				}
				return true
			})
		}
	}

	var missing []string
	for c := range declared {
		if len(produced[c]) == 0 {
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	for _, c := range missing {
		t.Errorf("heartbeat.%s is produced nowhere outside the tests.\nThe health page has a "+
			"label for it and a place in its order, and no service puts it into a row - the "+
			"page promises a number it can never draw. Before Z6 this was CounterErrors.", c)
	}
}

// attachedSinks names the variables body assigns as the second result of
// logsink.Attach - the sink, as opposed to the logger.
func attachedSinks(body *ast.BlockStmt, logsink string) map[string]bool {
	out := map[string]bool{}
	if logsink == "" {
		return out
	}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || len(as.Lhs) != 2 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok || !isSelector(call.Fun, logsink, "Attach") {
			return true
		}
		if id, ok := as.Lhs[1].(*ast.Ident); ok && id.Name != "_" {
			out[id.Name] = true
		}
		return true
	})
	return out
}

// hasField reports whether lit sets field at all.
func hasField(lit *ast.CompositeLit, field string) bool {
	if lit == nil {
		return false
	}
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == field {
				return true
			}
		}
	}
	return false
}

// optionsLiteral is the composite literal among call's arguments.
func optionsLiteral(call *ast.CallExpr) *ast.CompositeLit {
	for _, arg := range call.Args {
		if lit, ok := arg.(*ast.CompositeLit); ok {
			return lit
		}
	}
	return nil
}

// fieldIsOneOf reports whether lit sets field to one of the named
// variables.
func fieldIsOneOf(lit *ast.CompositeLit, field string, names map[string]bool) bool {
	if lit == nil {
		return false
	}
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == field {
			id, ok := kv.Value.(*ast.Ident)
			return ok && names[id.Name]
		}
	}
	return false
}

// heartbeatCounterConsts is every Counter* constant internal/heartbeat
// declares - read from the source, so a counter added there is under
// this rule without anybody listing it.
func heartbeatCounterConsts(t *testing.T, root string) map[string]bool {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(),
		filepath.Join(root, "internal", "heartbeat", "heartbeat.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			for _, n := range spec.(*ast.ValueSpec).Names {
				if strings.HasPrefix(n.Name, "Counter") {
					out[n.Name] = true
				}
			}
		}
	}
	if len(out) < 7 {
		t.Fatalf("found %d Counter constants in internal/heartbeat, want at least 7", len(out))
	}
	return out
}
