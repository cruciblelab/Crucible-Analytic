package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The packages that listen on a socket, and why each one does.
//
// This list is the "against a list, not against memory" half. The other
// half is read out of the source below; the two have to agree, and a
// disagreement in either direction is a failure that names the side
// that moved.
//
// Adding a package here is the moment somebody reads the rules that
// apply to a listening process, which is the entire point of making
// them add it.
var listeningPackages = map[string]string{
	"internal/api": "the read-only analytics API - bearer tokens, " +
		"loopback by default, behind a reverse proxy",
	"internal/beacon": "the endpoint a visitor's browser posts to; " +
		"the second of two services the internet reaches directly",
	"internal/fullproxy": "full mode - terminates TLS for the customer's " +
		"site and forwards plaintext to the backend",
	"internal/panel/web": "the customer's web interface",
	"internal/proxy": "passthrough mode - the collector's default, and " +
		"the only listener in this repository that is not an http.Server",
}

// ownAcceptLoop lists the packages that run their own Accept loop
// instead of handing the listener to net/http.
//
// It is a subset of the list above and it matters more, because
// net/http supplies two things this package then has to supply itself:
// a per-connection recover, and the header timeouts. Both were missing
// from internal/proxy until somebody noticed.
var ownAcceptLoop = map[string]string{
	"internal/proxy": "reads the ClientHello for the fingerprint and " +
		"splices bytes without decrypting them, so there is no request " +
		"for net/http to parse and no http.Server to inherit from",
}

// sourceRoots are the trees walked. Test files are skipped: a test may
// legitimately start a server with no timeout to measure what happens
// without one.
var sourceRoots = []string{"internal", "cmd"}

// ---------------------------------------------------------------- walk

type serverLit struct {
	pkg  string
	file string
	line int
	keys map[string]bool
}

type acceptLoop struct {
	pkg  string
	file string
	line int
}

type goroutine struct {
	pkg  string
	file string
	line int
	// recovers is true when the goroutine body defers a recover helper
	// itself.
	recovers bool
	// calls are the same-package functions its body calls, for the one
	// hop the rule allows - see TestEveryGoroutine below.
	calls []string
	// exemption is the reason written beside a goroutine that
	// deliberately has no recover.
	exemption string
}

type scan struct {
	servers    []serverLit
	listens    map[string][]string // package -> files that call net/tls.Listen
	acceptLoop []acceptLoop
	goroutines []goroutine
	// recoveringFuncs is the set of "pkg.Func" that defer a recover
	// helper, so a goroutine calling one is covered.
	recoveringFuncs map[string]bool
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// internal/invariants -> repository root
	return filepath.Dir(filepath.Dir(wd))
}

// scanTree parses every non-test .go file under the source roots.
func scanTree(t *testing.T) *scan {
	t.Helper()
	root := repoRoot(t)

	s := &scan{
		listens:         map[string][]string{},
		recoveringFuncs: map[string]bool{},
	}
	fset := token.NewFileSet()
	files := 0

	for _, dir := range sourceRoots {
		err := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			pkg := filepath.ToSlash(filepath.Dir(rel))

			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
			if err != nil {
				return err
			}
			files++
			scanFile(s, fset, f, pkg, rel, src)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	// A parser that read nothing would agree with every list.
	if files < 50 {
		t.Fatalf("only %d source files parsed; the walk is not reading the tree", files)
	}
	return s
}

func scanFile(s *scan, fset *token.FileSet, f *ast.File, pkg, rel string, src []byte) {
	// Functions whose body defers a recover helper, for the one-hop rule.
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if bodyDefersRecover(fn.Body) {
			s.recoveringFuncs[pkg+"."+fn.Name.Name] = true
		}
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {

		case *ast.CompositeLit:
			if !isSelector(node.Type, "http", "Server") {
				return true
			}
			keys := map[string]bool{}
			for _, elt := range node.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if id, ok := kv.Key.(*ast.Ident); ok {
						keys[id.Name] = true
					}
				}
			}
			s.servers = append(s.servers, serverLit{
				pkg: pkg, file: rel, line: fset.Position(node.Pos()).Line, keys: keys,
			})

		case *ast.CallExpr:
			if isSelector(node.Fun, "net", "Listen") || isSelector(node.Fun, "tls", "Listen") ||
				isSelector(node.Fun, "net", "ListenConfig") {
				s.listens[pkg] = append(s.listens[pkg], rel)
			}

		case *ast.ForStmt:
			// An Accept *loop*, not merely an Accept call: a listener
			// wrapper implementing net.Listener calls Accept once, and
			// that is not the thing this rule is about.
			ast.Inspect(node, func(inner ast.Node) bool {
				call, ok := inner.(*ast.CallExpr)
				if !ok {
					return true
				}
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Accept" {
					s.acceptLoop = append(s.acceptLoop, acceptLoop{
						pkg: pkg, file: rel, line: fset.Position(call.Pos()).Line,
					})
				}
				return true
			})

		case *ast.GoStmt:
			g := goroutine{pkg: pkg, file: rel, line: fset.Position(node.Pos()).Line}
			if lit, ok := node.Call.Fun.(*ast.FuncLit); ok && lit.Body != nil {
				g.recovers = bodyDefersRecover(lit.Body)
				g.calls = calledNames(lit.Body)
			}
			g.exemption = exemptionAbove(fset, src, node.Pos())
			s.goroutines = append(s.goroutines, g)
		}
		return true
	})
}

// bodyDefersRecover reports whether a block defers something whose name
// begins with "recover".
//
// By name rather than by resolving the call, because the rule is a
// convention this repository keeps - recoverConn - and a test that
// resolved types would still be checking that the convention was
// followed. What it must not do is match a *call* to recover() buried
// somewhere in the body: only a deferred call at this block's own level
// covers this goroutine.
func bodyDefersRecover(body *ast.BlockStmt) bool {
	for _, stmt := range body.List {
		d, ok := stmt.(*ast.DeferStmt)
		if !ok {
			continue
		}
		if name := calleeName(d.Call.Fun); strings.HasPrefix(strings.ToLower(name), "recover") {
			return true
		}
	}
	return false
}

func calledNames(body *ast.BlockStmt) []string {
	var out []string
	ast.Inspect(body, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if name := calleeName(call.Fun); name != "" {
				out = append(out, name)
			}
		}
		return true
	})
	return out
}

func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

func isSelector(e ast.Expr, pkg, name string) bool {
	star, ok := e.(*ast.StarExpr)
	if ok {
		e = star.X
	}
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg && sel.Sel.Name == name
}

// exemptionAbove returns the reason written on a `// no-recover:` line
// immediately above a statement.
//
// A marker with a reason rather than a bare suppression: the goroutine
// that closes a listener when the context ends genuinely has no
// connection to protect, and saying so in one line is better than
// either a silent exception list in this file or a recover that
// pretends to guard something.
func exemptionAbove(fset *token.FileSet, src []byte, pos token.Pos) string {
	line := fset.Position(pos).Line
	lines := strings.Split(string(src), "\n")
	// The whole contiguous comment block above the statement, not a
	// fixed number of lines: a reason worth writing is usually longer
	// than one line, and a window that cut it off would refuse the
	// exemption for being well explained.
	for i := line - 2; i >= 0; i-- {
		text := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(text, "//") {
			break
		}
		if idx := strings.Index(text, "no-recover:"); idx >= 0 {
			return strings.TrimSpace(text[idx+len("no-recover:"):])
		}
	}
	return ""
}

// ------------------------------------------------------------- the rules

// TestEveryHTTPServerBoundsItsHeaderRead.
//
// ReadHeaderTimeout is the slowloris bound: without it a client can open
// a connection, send one byte of a header a minute, and hold a
// goroutine and a file descriptor open indefinitely. Go's zero value is
// no timeout, so this is a property every server has to opt into and
// three of four once did.
func TestEveryHTTPServerBoundsItsHeaderRead(t *testing.T) {
	s := scanTree(t)

	if len(s.servers) < 4 {
		t.Fatalf("found %d http.Server literals; this repository has more than that, so the scan is wrong", len(s.servers))
	}
	for _, srv := range s.servers {
		if !srv.keys["ReadHeaderTimeout"] {
			t.Errorf("%s:%d builds an http.Server with no ReadHeaderTimeout; "+
				"Go's zero value is no timeout, and a client sending one header byte a minute holds the connection open forever",
				srv.file, srv.line)
		}
	}
	t.Logf("%d http.Server literals checked", len(s.servers))
}

// TestTheListeningPackagesAreTheOnesOnTheList is the two-way mirror.
//
// A new package that opens a socket is a new place where every rule
// about listening processes has to be applied again - timeouts,
// recover, what the address defaults to, whether anything publishes it.
// The list above is where a person meets those rules, and this test is
// what makes them meet it.
func TestTheListeningPackagesAreTheOnesOnTheList(t *testing.T) {
	s := scanTree(t)

	found := map[string]bool{}
	for pkg := range s.listens {
		found[pkg] = true
	}
	for _, srv := range s.servers {
		found[srv.pkg] = true
	}

	for pkg := range found {
		if _, listed := listeningPackages[pkg]; !listed {
			t.Errorf("%s listens on a socket and is not in listeningPackages; "+
				"add it with a reason, having first checked it sets its timeouts and recovers per connection", pkg)
		}
	}
	for pkg, why := range listeningPackages {
		if !found[pkg] {
			t.Errorf("listeningPackages claims %s listens (%q) and nothing in it does any more; "+
				"remove the entry so the list stays worth reading", pkg, why)
		}
	}

	names := make([]string, 0, len(found))
	for pkg := range found {
		names = append(names, pkg)
	}
	sort.Strings(names)
	t.Logf("listening packages: %s", strings.Join(names, ", "))
}

// TestTheOwnAcceptLoopPackagesAreTheOnesOnTheList.
//
// The sharper half of the list above. A package that hands its listener
// to net/http inherits a per-connection recover and a header timeout; a
// package that runs its own Accept loop inherits neither and has to
// write both. internal/proxy is the only one, and it did not have them.
func TestTheOwnAcceptLoopPackagesAreTheOnesOnTheList(t *testing.T) {
	s := scanTree(t)

	found := map[string]bool{}
	for _, loop := range s.acceptLoop {
		found[loop.pkg] = true
	}
	if len(found) == 0 {
		t.Fatal("no Accept loop found anywhere; the scan is not recognising one")
	}

	for pkg := range found {
		if _, listed := ownAcceptLoop[pkg]; !listed {
			t.Errorf("%s runs its own Accept loop and is not in ownAcceptLoop; "+
				"net/http is not there to recover its panics or bound its reads, so it must do both itself", pkg)
		}
	}
	for pkg, why := range ownAcceptLoop {
		if !found[pkg] {
			t.Errorf("ownAcceptLoop claims %s accepts connections itself (%q), and it no longer does", pkg, why)
		}
	}
}

// TestEveryGoroutineInAnAcceptLoopPackageRecovers.
//
// Go tears the whole process down on an unrecovered panic in any
// goroutine, and recover() only sees panics raised on its own
// goroutine - so a recover in the connection handler does nothing for
// the two splice goroutines it starts.
//
// The collector sits in front of the customer's website. "The process
// dies" therefore means "the customer's site goes down", and an
// attacker who found the input only has to send it again after each
// restart. That is why this rule is checked here rather than trusted to
// review.
//
// One hop is allowed: a goroutine whose body calls a same-package
// function that defers the recover is covered, because that is what the
// connection goroutine actually does. Anything else needs a
// `// no-recover: reason` line, and the reason has to be there.
func TestEveryGoroutineInAnAcceptLoopPackageRecovers(t *testing.T) {
	s := scanTree(t)

	checked := 0
	for _, g := range s.goroutines {
		if _, relevant := ownAcceptLoop[g.pkg]; !relevant {
			continue
		}
		checked++
		if g.recovers {
			continue
		}
		if g.exemption != "" {
			t.Logf("%s:%d exempt - %s", g.file, g.line, g.exemption)
			continue
		}
		covered := false
		for _, name := range g.calls {
			if s.recoveringFuncs[g.pkg+"."+name] {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("%s:%d starts a goroutine with no recover, and calls nothing that has one. "+
				"A panic here takes down the process, and with it the customer's site. "+
				"Either defer a recover helper, or write `// no-recover: <reason>` above it",
				g.file, g.line)
		}
	}
	if checked < 3 {
		t.Fatalf("only %d goroutines examined in the accept-loop packages; the scan is not finding them", checked)
	}
	t.Logf("%d goroutines examined", checked)
}
