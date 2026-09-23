package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// PLAN §Z2, held against the source.
//
// An http.Server's WriteTimeout does not stop a handler; it closes the
// connection under one. Measured on the real binaries: the read API sent
// 0 bytes and no status line to four requests out of eight, and logged
// nothing; the panel sent 0 bytes for a page whose table was locked, and
// its access log wrote status=200. internal/deadline answers such a
// request with a 503 while there is time to deliver it, and this test
// holds every server that can truncate to it.
//
// # Which servers are asked
//
// Every http.Server literal in product code that sets WriteTimeout. The
// condition is the exemption: a server without one - fullproxy, which
// carries the customer's own downloads and streams - closes nothing
// under a handler, so there is nothing to answer for. No name list for
// that half.
//
// # What counts as wired
//
// The literal's Handler is a call to deadline.Handler, or a method of
// the same package that reaches one through other methods of that
// package - the panel builds its chain in Handler() and wraps it in
// withDeadline. Either way the call must pass the *same expression* the
// literal gives WriteTimeout: a deadline derived from a different number
// is a deadline that can be longer than the timeout it exists to beat.

const deadlinePath = "github.com/cruciblelab/crucible-analytic/internal/deadline"

// deadlineExempt are servers that set WriteTimeout and are deliberately
// not wrapped, each with the reason. Keyed by directory.
//
// The one entry is a measured cost against a defect that is not there,
// and it says what would make it wrong.
var deadlineExempt = map[string]string{
	"internal/beacon": "Nothing a beacon handler calls waits on anything but the request " +
		"body: Sink.Enqueue is a select with a default, GeoResolver.Resolve reads in-memory " +
		"range tables, and the disclosure is read from memory. A deadline would bound " +
		"nothing, and TimeoutHandler costs the event path: measured, 14.3-16.1 us -> " +
		"31.8-34.3 us per event, 62 -> 74 allocations, six runs each, no overlap. The " +
		"day a beacon handler waits on a database this entry is wrong.",
}

// parsedTree is every non-test file under sourceRoots, by directory.
type parsedTree struct {
	fset  *token.FileSet
	byDir map[string][]*ast.File
}

func parseTree(t *testing.T, root string) *parsedTree {
	t.Helper()
	pt := &parsedTree{fset: token.NewFileSet(), byDir: map[string][]*ast.File{}}
	n := 0
	for _, src := range sourceRoots {
		err := filepath.Walk(filepath.Join(root, src), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(pt.fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			dir := filepath.ToSlash(rel)
			pt.byDir[dir] = append(pt.byDir[dir], f)
			n++
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if n < 50 {
		t.Fatalf("only %d source files parsed; the walk is not reading the tree", n)
	}
	return pt
}

// serverLiteral is one http.Server{...} found in the tree.
type serverLiteral struct {
	dir          string // relative to the repository root
	pos          string
	writeTimeout ast.Expr
	handler      ast.Expr
}

func findServerLiterals(pt *parsedTree) []serverLiteral {
	var out []serverLiteral
	for dir, files := range pt.byDir {
		for _, f := range files {
			httpName := localName(f, "net/http")
			if httpName == "" {
				continue
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Server" {
					return true
				}
				if id, ok := sel.X.(*ast.Ident); !ok || id.Name != httpName {
					return true
				}
				p := pt.fset.Position(lit.Pos())
				sl := serverLiteral{dir: dir, pos: filepath.Base(p.Filename) + ":" + strconv.Itoa(p.Line)}
				sl.pos = dir + "/" + sl.pos
				for _, el := range lit.Elts {
					kv, ok := el.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok {
						continue
					}
					switch key.Name {
					case "WriteTimeout":
						sl.writeTimeout = kv.Value
					case "Handler":
						sl.handler = kv.Value
					}
				}
				out = append(out, sl)
				return true
			})
		}
	}
	return out
}

func TestEveryServerThatCanTruncateAnswersBeforeItDoes(t *testing.T) {
	pt := parseTree(t, repoRoot(t))
	literals := findServerLiterals(pt)

	timed := 0
	exemptSeen := map[string]bool{}
	for _, sl := range literals {
		if sl.writeTimeout == nil {
			continue
		}
		timed++
		if _, ok := deadlineExempt[sl.dir]; ok {
			exemptSeen[sl.dir] = true
			continue
		}
		want := types.ExprString(sl.writeTimeout)
		if !reachesDeadline(pt.byDir[sl.dir], sl.handler, want, 4) {
			t.Errorf("%s: this http.Server sets WriteTimeout: %s and its Handler does not "+
				"pass through deadline.Handler with that same expression.\nWithout it a "+
				"request that outruns the timeout gets 0 bytes and no status line, and "+
				"nothing is logged - measured on the read API and the panel. Wrap the "+
				"handler in deadline.Handler(h, %s, answer, logger), or, if no handler "+
				"here can wait on anything, add the directory to deadlineExempt with "+
				"the measurement that says so.", sl.pos, want, want)
		}
	}

	// Vacuity, with a number: the API, the panel and the beacon set one
	// today. Fewer found means this walk stopped seeing them, and a rule
	// over an empty set passes whatever the servers do.
	if timed < 3 {
		t.Fatalf("found %d http.Server literals with a WriteTimeout, expected at least 3 "+
			"(api, panel, beacon)", timed)
	}

	// The exemption list's stale half: an entry for a directory whose
	// server no longer sets a WriteTimeout exempts nothing today and
	// would silently exempt whatever is put there next.
	for dir := range deadlineExempt {
		if !exemptSeen[dir] {
			t.Errorf("deadlineExempt names %s, which has no http.Server with a "+
				"WriteTimeout any more - remove the entry", dir)
		}
	}
}

// reachesDeadline reports whether expr is, or reaches through methods of
// the package whose files are given, a call deadline.Handler(_, <want>, ...).
func reachesDeadline(files []*ast.File, expr ast.Expr, want string, depth int) bool {
	if depth == 0 || expr == nil {
		return false
	}
	methods := map[string]*ast.FuncDecl{}
	deadlineName := map[*ast.FuncDecl]string{}
	for _, f := range files {
		name := localName(f, deadlinePath)
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv != nil && fn.Body != nil {
				methods[fn.Name.Name] = fn
				deadlineName[fn] = name
			}
		}
	}

	var found bool
	var visit func(n ast.Node, pkgName string, depth int)
	visit = func(n ast.Node, pkgName string, depth int) {
		if depth == 0 || found {
			return
		}
		ast.Inspect(n, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || found {
				return !found
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && pkgName != "" && id.Name == pkgName && sel.Sel.Name == "Handler" {
				if len(call.Args) >= 2 && types.ExprString(call.Args[1]) == want {
					found = true
					return false
				}
			}
			// A method of this package: follow it.
			if fn, ok := methods[sel.Sel.Name]; ok {
				visit(fn.Body, deadlineName[fn], depth-1)
			}
			return true
		})
	}

	// The literal's own file may import the deadline package too.
	for _, f := range files {
		if name := localName(f, deadlinePath); name != "" {
			visit(expr, name, depth)
		}
	}
	if !found {
		visit(expr, "", depth)
	}
	return found
}

// The panel's second half. Its access log records the status written
// through it, so the deadline has to sit inside it: outside, the log goes
// on writing the handler's intention - status=200 for a response nobody
// received, which is what was measured.
func TestThePanelsAccessLogSeesTheDeadlinesAnswer(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "internal", "panel", "web")
	var handler *ast.FuncDecl
	for _, f := range goFilesIn(t, dir) {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Recv != nil && fn.Name.Name == "Handler" && fn.Body != nil {
				handler = fn
			}
		}
	}
	if handler == nil {
		t.Fatal("no Handler method in internal/panel/web - the chain moved")
	}

	var logCalls []*ast.CallExpr
	ast.Inspect(handler.Body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "requestLog" {
				logCalls = append(logCalls, call)
			}
		}
		return true
	})
	if len(logCalls) != 1 {
		t.Fatalf("Handler calls requestLog %d times, want exactly 1", len(logCalls))
	}

	inside := false
	for _, arg := range logCalls[0].Args {
		ast.Inspect(arg, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "withDeadline" {
					inside = true
				}
			}
			return true
		})
	}
	if !inside {
		t.Errorf("internal/panel/web Handler: withDeadline is not inside requestLog.\n" +
			"The access log records the status written through it; with the deadline " +
			"outside, a request that ran out of time is logged with the status its " +
			"handler meant to send - measured, status=200 for 0 bytes delivered.")
	}
}
