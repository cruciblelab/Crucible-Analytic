package invariants

import (
	"go/ast"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"testing"
)

// PLAN §4 #38 (B3e), held against the source.
//
// The panel restarts a service by writing a request row; the service
// reads it and exits cleanly, and its supervisor starts it again
// (internal/restartreq). The panel offers the button for every service
// that writes a heartbeat row - that row is how the page names the
// service, and how it later says "restarted". So a main that writes a
// heartbeat and runs no watcher is a button that writes its row, says
// "waiting", and never does anything.
//
// What is checked, in every main that calls heartbeat.New - the list is
// the tree's, not written here:
//
//   - a restartreq.Watcher is built and its Run is started on a goroutine;
//   - its Started is that main's reporter's Started method. The request is
//     the database's clock and so is that start; a start from anywhere
//     else (time.Now, another reporter) compares two clocks, and a
//     process whose clock is behind the database's exits again after
//     every restart;
//   - its Pool is a pool main opened with resources.OpenMonitor, so the
//     question is asked on the connections that stay free when the work's
//     pool is exhausted (Z4: the one state a restart is most for);
//   - its Exit cancels, with restartreq.ErrRequested, the context made by
//     context.WithCancelCause from signal.NotifyContext's - and that signal
//     context is used for nothing else. That last part is what makes the
//     exit "the path SIGTERM takes": every consumer that watched the
//     signal now watches the derived context, so no part of the shutdown
//     is reachable only by a signal;
//   - the watcher and the stopping line run on that derived context.

const restartreqPath = "github.com/cruciblelab/crucible-analytic/internal/restartreq"

func TestEveryHeartbeatServiceHearsRestartRequests(t *testing.T) {
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		t.Fatal(err)
	}

	var beating []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		for _, f := range goFilesIn(t, filepath.Join(root, "cmd", name)) {
			hb := localName(f, heartbeatPath)
			if hb == "" {
				continue
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name.Name != "main" || fn.Recv != nil || fn.Body == nil {
					continue
				}
				if len(callsOf(fn.Body, hb, "New")) == 0 {
					continue
				}
				beating = append(beating, name)
				for _, problem := range restartWiring(f, fn.Body) {
					t.Errorf("cmd/%s: %s", name, problem)
				}
			}
		}
	}
	sort.Strings(beating)

	// Vacuity, with a number: four services write a heartbeat (collector,
	// beacon, analytics-api, panel; the upgrader reads the others' rows
	// and writes none). Fewer means heartbeat.New moved out of main, where
	// this rule cannot follow it - move the rule with it.
	if len(beating) < 4 {
		t.Errorf("found %d mains calling heartbeat.New (%v), want at least 4", len(beating), beating)
	}
}

// restartWiring lists what is wrong with one main's restart wiring.
func restartWiring(f *ast.File, body *ast.BlockStmt) []string {
	rr := localName(f, restartreqPath)
	if rr == "" {
		return []string{"main writes a heartbeat and does not import internal/restartreq, " +
			"so the panel's restart button for this service writes a request nobody reads"}
	}

	monitors := monitorPools(body, localName(f, resourcesPath))
	signals := assignedFrom(body, localName(f, "os/signal"), "NotifyContext", 0)

	var problems []string

	// The reporter is the one main runs: a second heartbeat.New built and
	// not run has a Started that never becomes known, so a watcher wired
	// to it never acts.
	reporters := map[string]bool{}
	for r := range assignedFrom(body, localName(f, heartbeatPath), "New", 0) {
		if goRunOnAny(body, r, "Run") {
			reporters[r] = true
		}
	}
	if len(reporters) != 1 {
		problems = append(problems, "main runs "+strconv.Itoa(len(reporters))+" heartbeat "+
			"reporters, want exactly one - the start a restart request is compared with "+
			"is that reporter's, and with none or two there is no one start")
	}

	// ctx, exit := context.WithCancelCause(<a signal context>)
	derived, cancels := map[string]bool{}, map[string]bool{}
	ctxPkg := localName(f, "context")
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 2 || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok || !isSelector(call.Fun, ctxPkg, "WithCancelCause") || len(call.Args) != 1 {
			return true
		}
		if parent, ok := call.Args[0].(*ast.Ident); !ok || !signals[parent.Name] {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok {
			derived[id.Name] = true
		}
		if id, ok := as.Lhs[1].(*ast.Ident); ok {
			cancels[id.Name] = true
		}
		return true
	})

	if len(signals) == 0 {
		problems = append(problems, "main takes no signal.NotifyContext, so this rule cannot "+
			"say what the path SIGTERM takes is - the shutdown moved")
	}
	if len(derived) == 0 {
		problems = append(problems, "main does not derive a context with "+
			"context.WithCancelCause from signal.NotifyContext's.\nA restart request has "+
			"to end the same context a signal ends, with restartreq.ErrRequested as the "+
			"cause, or it takes a path of its own that the shutdown was never written for")
	}
	for sig := range signals {
		// Uses of the signal context other than its definition: exactly
		// one, the parent of the derived context.
		if uses := identUses(body, sig); uses != 1 {
			problems = append(problems, "the signal context "+sig+" is used "+strconv.Itoa(uses)+
				" times besides its definition, want once - as the parent of "+
				"context.WithCancelCause.\nAnything else watching it is a path only a "+
				"signal reaches: a restart request would leave it running")
		}
	}

	// w := &restartreq.Watcher{...}
	watchers := map[string]*ast.CompositeLit{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		e := as.Rhs[0]
		if u, ok := e.(*ast.UnaryExpr); ok {
			e = u.X
		}
		lit, ok := e.(*ast.CompositeLit)
		if !ok || !isSelector(lit.Type, rr, "Watcher") {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok {
			watchers[id.Name] = lit
		}
		return true
	})
	if len(watchers) == 0 {
		return append(problems, "main writes a heartbeat and builds no restartreq.Watcher "+
			"(assigned to a variable), so the panel's restart button for this service "+
			"writes a request nobody reads")
	}

	for name, lit := range watchers {
		fields := map[string]ast.Expr{}
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if key, ok := kv.Key.(*ast.Ident); ok {
					fields[key.Name] = kv.Value
				}
			}
		}
		if !isMonitor(fields["Pool"], monitors) {
			problems = append(problems, "the watcher "+name+"'s Pool is not a pool main opened "+
				"with resources.OpenMonitor.\nThe question is then asked behind the work it "+
				"is meant to rescue: an exhausted work pool is a service the panel cannot restart")
		}
		if !isMethodOf(fields["Started"], reporters, "Started") {
			problems = append(problems, "the watcher "+name+"'s Started is not the method "+
				"Started of the reporter main built with heartbeat.New.\nThe request is the "+
				"database's clock and so is that reporter's start; any other start compares "+
				"two clocks, and a process behind the database exits again after every restart")
		}
		if !cancelsWithRequested(fields["Exit"], cancels, rr) {
			problems = append(problems, "the watcher "+name+"'s Exit is not a function that "+
				"calls the cancel of context.WithCancelCause(<signal context>) with "+
				"restartreq.ErrRequested.\nAnything else is not the path SIGTERM takes, and "+
				"the stopping line cannot say the panel asked")
		}
		if !goRunOn(body, name, "Run", derived) {
			problems = append(problems, "the watcher "+name+" is built and its Run is not "+
				"started on a goroutine with the derived context - a watcher that never runs "+
				"is the same button that does nothing, with a call site that looks right")
		}
	}

	problems = append(problems, stoppingLine(body, rr, derived)...)
	return problems
}

// stoppingLine checks `w := restartreq.LogStopping(<derived ctx>, ...)`
// and a call w() in the same main: the line is arranged on the context a
// restart request ends, and main waits for it before saying the
// shutdown is complete.
func stoppingLine(body *ast.BlockStmt, rr string, derived map[string]bool) []string {
	waits := map[string]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok || !isSelector(call.Fun, rr, "LogStopping") || len(call.Args) == 0 {
			return true
		}
		if c, ok := call.Args[0].(*ast.Ident); !ok || !derived[c.Name] {
			return true
		}
		if id, ok := as.Lhs[0].(*ast.Ident); ok {
			waits[id.Name] = true
		}
		return true
	})
	if len(waits) == 0 {
		return []string{"main does not call restartreq.LogStopping on the derived context, " +
			"so nothing says why the process stopped - a restart from the panel and a signal " +
			"look the same in the log"}
	}
	called := false
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && len(call.Args) == 0 {
			if id, ok := call.Fun.(*ast.Ident); ok && waits[id.Name] {
				called = true
			}
		}
		return true
	})
	if !called {
		return []string{"main never calls the function restartreq.LogStopping returned, so " +
			"nothing orders the stopping line before \"shutdown complete\" - measured on the " +
			"read API, the two came out the other way round, and a main that returns first " +
			"exits with no stopping line at all"}
	}
	return nil
}

// assignedFrom names the variables at position index of every assignment
// whose right-hand side is the call <pkg>.<fn>(...).
func assignedFrom(body *ast.BlockStmt, pkg, fn string, index int) map[string]bool {
	out := map[string]bool{}
	if pkg == "" {
		return out
	}
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Rhs) != 1 || len(as.Lhs) <= index {
			return true
		}
		call, ok := as.Rhs[0].(*ast.CallExpr)
		if !ok || !isSelector(call.Fun, pkg, fn) {
			return true
		}
		if id, ok := as.Lhs[index].(*ast.Ident); ok {
			out[id.Name] = true
		}
		return true
	})
	return out
}

// identUses counts the identifiers named name in body that are not the
// left-hand side of a definition.
func identUses(body *ast.BlockStmt, name string) int {
	defined := map[*ast.Ident]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok {
			for _, l := range as.Lhs {
				if id, ok := l.(*ast.Ident); ok {
					defined[id] = true
				}
			}
		}
		return true
	})
	uses := 0
	ast.Inspect(body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && id.Name == name && !defined[id] {
			uses++
		}
		return true
	})
	return uses
}

// isMethodOf reports whether e is <v>.<method> for a v in vars - the
// method value, not a call of it.
func isMethodOf(e ast.Expr, vars map[string]bool, method string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && vars[id.Name]
}

// cancelsWithRequested reports whether e is a function literal whose
// body calls one of cancels with <rr>.ErrRequested.
func cancelsWithRequested(e ast.Expr, cancels map[string]bool, rr string) bool {
	lit, ok := e.(*ast.FuncLit)
	if !ok {
		return false
	}
	found := false
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 1 {
			return true
		}
		if id, ok := call.Fun.(*ast.Ident); !ok || !cancels[id.Name] {
			return true
		}
		if isSelector(call.Args[0], rr, "ErrRequested") {
			found = true
		}
		return true
	})
	return found
}

// goRunOnAny reports whether body has `go <recv>.<method>(...)`, whatever
// its arguments.
func goRunOnAny(body *ast.BlockStmt, recv, method string) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		g, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		if sel, ok := g.Call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == method {
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == recv {
				found = true
			}
		}
		return true
	})
	return found
}

// goRunOn reports whether body has `go <recv>.<method>(<ctx>, ...)` with
// ctx one of ctxs. recv is a variable or a package's local name.
func goRunOn(body *ast.BlockStmt, recv, method string, ctxs map[string]bool) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		g, ok := n.(*ast.GoStmt)
		if !ok {
			return true
		}
		sel, ok := g.Call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != method {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != recv {
			return true
		}
		if len(g.Call.Args) > 0 {
			if c, ok := g.Call.Args[0].(*ast.Ident); ok && ctxs[c.Name] {
				found = true
			}
		}
		return true
	})
	return found
}
