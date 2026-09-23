package invariants

import (
	"go/ast"
	"path/filepath"
	"testing"
)

// PLAN §Z4, held against the source.
//
// A service's monitoring - its heartbeat row and the panel's copy of its
// log - writes through a pool of its own, opened by
// resources.OpenMonitor. Measured on the read API under 150 seconds of
// sustained load on a one-connection pool, when both shared the work
// pool: the heartbeat row did not advance once after startup, and 7 of
// the 33 WARN and ERROR lines in the log tree reached panel_logs. With
// the monitoring pool: three beats out of three on schedule, 36 of 36
// lines.
//
// The rule is on the wiring in each service's main, which is the only
// place the two pools meet: every logsink.Attach and every heartbeat.New
// there is handed a variable that main assigned from
// resources.OpenMonitor. The list of mains comes from the systemd units,
// as in budget_test.go.

const (
	logsinkPath   = "github.com/cruciblelab/crucible-analytic/internal/logsink"
	heartbeatPath = "github.com/cruciblelab/crucible-analytic/internal/heartbeat"
)

func TestMonitoringWritesThroughItsOwnPool(t *testing.T) {
	root := repoRoot(t)
	attaches, beats := 0, 0

	for _, name := range serviceBinaries(t, root) {
		for _, f := range goFilesIn(t, filepath.Join(root, "cmd", name)) {
			res := localName(f, resourcesPath)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name.Name != "main" || fn.Recv != nil || fn.Body == nil {
					continue
				}
				monitors := monitorPools(fn.Body, res)

				for _, call := range callsOf(fn.Body, localName(f, logsinkPath), "Attach") {
					attaches++
					if len(call.Args) < 2 || !isMonitor(call.Args[1], monitors) {
						t.Errorf("cmd/%s: logsink.Attach is given a pool that main did not open "+
							"with resources.OpenMonitor.\nThe panel's copy of the log then waits "+
							"behind the work it reports on - measured, 7 of 33 lines reached "+
							"the panel under sustained load.", name)
					}
				}
				for _, call := range callsOf(fn.Body, localName(f, heartbeatPath), "New") {
					beats++
					if !optionsPoolIsMonitor(call, monitors) {
						t.Errorf("cmd/%s: heartbeat.New's Options.Pool is not a pool main opened "+
							"with resources.OpenMonitor.\nThe heartbeat then waits behind the "+
							"work it reports on - measured, the row did not advance once in "+
							"150 seconds, and the health page shows a busy service as stale.", name)
					}
				}
			}
		}
	}

	// Vacuity, with numbers: all five services attach a log sink; three
	// of them write a heartbeat (the panel and the upgrader do not).
	// Fewer means the calls moved out of main, where this rule cannot
	// follow them - move the rule with them.
	if attaches < 5 {
		t.Errorf("found %d logsink.Attach calls in service mains, want at least 5", attaches)
	}
	if beats < 3 {
		t.Errorf("found %d heartbeat.New calls in service mains, want at least 3", beats)
	}
}

// monitorPools names the variables body assigns from res.OpenMonitor.
func monitorPools(body *ast.BlockStmt, res string) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || len(as.Lhs) == 0 {
			return true
		}
		// The right-hand side is the call itself, not an expression that
		// contains one: f(resources.OpenMonitor(...)) is whatever f
		// returns.
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "OpenMonitor" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || res == "" || pkg.Name != res {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok {
			out[id.Name] = true
		}
		return true
	})
	return out
}

func isMonitor(e ast.Expr, monitors map[string]bool) bool {
	id, ok := e.(*ast.Ident)
	return ok && monitors[id.Name]
}

// optionsPoolIsMonitor finds the Pool field of heartbeat.New's options
// literal.
func optionsPoolIsMonitor(call *ast.CallExpr, monitors map[string]bool) bool {
	for _, arg := range call.Args {
		lit, ok := arg.(*ast.CompositeLit)
		if !ok {
			continue
		}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Pool" {
				return isMonitor(kv.Value, monitors)
			}
		}
	}
	return false
}
