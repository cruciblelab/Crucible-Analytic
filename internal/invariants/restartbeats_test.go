package invariants

import (
	"go/ast"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/relupdate"
)

// PLAN §V4b, held against the source.
//
// After a release the upgrader rings the restarter, and release/restart.sh
// restarts every unit named in its UNITS line. Then relupdate waits for
// each restarted service's heartbeat row, and a service that never
// writes one reads as a service that never came back: the previous
// binaries are put back.
//
// The panel was restarted and wrote no row. Its tests beat for it, so
// they passed; measured on the real binary, a panel running for 75
// seconds wrote nothing and relupdate's own check answered
// missing=[panel_user] - every release rolled back, on every deployment
// that had the restarter on, since the day it was added.
//
// So the rule is derived from the one list that decides what gets
// restarted: every unit in restart.sh runs a binary whose main writes a
// heartbeat, and the check waits for exactly as many rows as there are
// units. A service added to the restart without a heartbeat - or a
// heartbeat removed from one - fails here rather than on a customer's
// first update.

var restartUnits = regexp.MustCompile(`(?m)^UNITS="([^"]+)"`)

func TestEveryRestartedServiceReportsBack(t *testing.T) {
	root := repoRoot(t)
	script, err := os.ReadFile(filepath.Join(root, "release", "restart.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := restartUnits.FindSubmatch(script)
	if m == nil {
		t.Fatal(`release/restart.sh has no UNITS="..." line; the list of restarted services moved`)
	}
	units := strings.Fields(string(m[1]))

	for _, unit := range units {
		body, err := os.ReadFile(filepath.Join(root, "release", "systemd", unit+".service"))
		if err != nil {
			t.Errorf("restart.sh restarts %s and release/systemd has no such unit: %v", unit, err)
			continue
		}
		exec := execStart.FindSubmatch(body)
		if exec == nil {
			t.Errorf("%s.service runs nothing from the install's bin directory", unit)
			continue
		}
		name := string(exec[1])
		if !mainBeats(t, filepath.Join(root, "cmd", name)) {
			t.Errorf("restart.sh restarts %s (cmd/%s), and cmd/%s's main writes no heartbeat.\n"+
				"relupdate waits for every restarted service's row after a release; this one "+
				"never writes one, so every release is rolled back as a service that did not "+
				"come back. Measured before V4b's fix, for the panel.", unit, name, name)
		}
	}

	// The check waits for as many rows as there are restarted services.
	// Names are compared nowhere here - a unit is not a role, and the role
	// a service connects as lives in its config - but a count that
	// disagrees means one of the two lists has a service the other does
	// not.
	if len(relupdate.HealthServices) != len(units) {
		t.Errorf("relupdate waits for %d services (%v) and restart.sh restarts %d (%v)",
			len(relupdate.HealthServices), relupdate.HealthServices, len(units), units)
	}
	if len(units) < 4 {
		t.Errorf("restart.sh restarts %d units, want at least 4 - the line changed shape", len(units))
	}
}

// mainBeats reports whether the main in dir starts a heartbeat: assigns
// the result of heartbeat.New to a variable and runs that variable's Run
// on a goroutine.
//
// Both halves, because a reporter that is built and never run writes
// nothing - the same row-less service, with a call site that looks right.
func mainBeats(t *testing.T, dir string) bool {
	t.Helper()
	for _, f := range goFilesIn(t, dir) {
		hb := localName(f, heartbeatPath)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "main" || fn.Recv != nil || fn.Body == nil || hb == "" {
				continue
			}
			reporters := map[string]bool{}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
					return true
				}
				call, ok := as.Rhs[0].(*ast.CallExpr)
				if !ok || !isSelector(call.Fun, hb, "New") {
					return true
				}
				if id, ok := as.Lhs[0].(*ast.Ident); ok {
					reporters[id.Name] = true
				}
				return true
			})
			runs := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				g, ok := n.(*ast.GoStmt)
				if !ok {
					return true
				}
				if sel, ok := g.Call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" {
					if id, ok := sel.X.(*ast.Ident); ok && reporters[id.Name] {
						runs = true
					}
				}
				return true
			})
			if runs {
				return true
			}
		}
	}
	return false
}
