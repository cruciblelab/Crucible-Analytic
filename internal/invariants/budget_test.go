package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Two rules from PLAN §Z1, both held against the source rather than
// against anybody's memory of it.
//
//  1. Every database pool in the product is opened through
//     internal/resources, which sizes it from GOMAXPROCS. pgx's own
//     default sizes it from runtime.NumCPU - measured, that is the host's
//     core count even inside a 0.5-CPU quota - and a container given one
//     CPU on a thirty-two core host would get thirty-two connections per
//     pool.
//  2. Every service calls resources.Apply, which hands the memory limit
//     to the runtime. The runtime does not read it by itself - also
//     measured, inside a 64 MB cgroup.
//
// # Why the second list comes from the systemd units
//
// "A service" has to mean something a test can find, and the unit files
// are where this product says what runs as one. A binary added
// tomorrow with a unit and without the call fails here; a CLI tool with
// no unit (devpass, releasesign) is not a service and is not asked.
// Deriving the list from the units rather than typing it is PLAN's
// rule: *listeyi türet, ismi ekleme.*

// resourcesPath is the one package allowed to open a pool.
const resourcesPath = "github.com/cruciblelab/crucible-analytic/internal/resources"

const pgxpoolPath = "github.com/jackc/pgx/v5/pgxpool"

// localName is the name a file uses for an import path, or "" when the
// file does not import it. Resolved per file rather than assumed, so an
// aliased import cannot hide a call from this test.
func localName(f *ast.File, path string) string {
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil || p != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return filepath.Base(p)
	}
	return ""
}

// callsOf lists every call in n of the form <pkg>.<name>(...) where name
// is one of names.
func callsOf(n ast.Node, pkg string, names ...string) []*ast.CallExpr {
	var out []*ast.CallExpr
	if pkg == "" {
		return out
	}
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != pkg {
			return true
		}
		for _, name := range names {
			if sel.Sel.Name == name {
				out = append(out, call)
			}
		}
		return true
	})
	return out
}

func TestEveryPoolIsOpenedThroughResources(t *testing.T) {
	root := repoRoot(t)

	var outside []string
	insideResources := 0

	for _, dir := range sourceRoots {
		err := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if name := info.Name(); name == "testdata" || name == "scratchpad" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			calls := callsOf(f, localName(f, pgxpoolPath), "New", "NewWithConfig")
			if filepath.ToSlash(filepath.Dir(rel)) == "internal/resources" {
				insideResources += len(calls)
				return nil
			}
			for _, c := range calls {
				outside = append(outside, fset.Position(c.Pos()).String())
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}

	// Vacuity. If internal/resources itself stopped showing a call this
	// test recognises, the detector is looking for a shape the code no
	// longer has - and it would then find nothing anywhere else either,
	// and pass.
	if insideResources == 0 {
		t.Fatal("internal/resources contains no pgxpool.New or NewWithConfig call; " +
			"either the pool is opened some other way now or this test cannot see " +
			"the call, and in both cases its silence elsewhere means nothing")
	}

	sort.Strings(outside)
	for _, pos := range outside {
		t.Errorf("%s opens a database pool directly.\n"+
			"Use resources.Open, or resources.ParseConfig and resources.NewPool when "+
			"the configuration needs adjusting first. A pool opened directly gets "+
			"pgx's default size, which counts the host's cores rather than the "+
			"container's: one CPU on a thirty-two core host is thirty-two connections "+
			"per pool, and PostgreSQL's default ceiling is a hundred.", pos)
	}
}

// execStart finds the binary a unit runs from the install's bin
// directory.
var execStart = regexp.MustCompile(`(?m)^ExecStart=/opt/crucible-analytic/bin/([a-z0-9-]+)(\s|$)`)

// serviceBinaries is every cmd/<name> that a systemd unit runs.
func serviceBinaries(t *testing.T, root string) []string {
	t.Helper()
	units, err := filepath.Glob(filepath.Join(root, "release", "systemd", "*.service"))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	var out []string
	for _, u := range units {
		b, err := os.ReadFile(u)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range execStart.FindAllStringSubmatch(string(b), -1) {
			name := m[1]
			// A unit may run a script rather than a Go binary
			// (crucible-restart runs restart.sh); the regexp above does not
			// match a dotted name, and a cmd directory is what makes it Go.
			if _, err := os.Stat(filepath.Join(root, "cmd", name)); err != nil {
				continue
			}
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

func TestEveryServiceHandsItsMemoryLimitToTheRuntime(t *testing.T) {
	root := repoRoot(t)
	services := serviceBinaries(t, root)

	// Vacuity, with a number: there are five today. Fewer than four found
	// means the units moved or this pattern stopped matching them, and a
	// test over an empty list passes whatever the mains do.
	if len(services) < 4 {
		t.Fatalf("found %d service binaries in release/systemd (%v); expected at "+
			"least four - the unit files moved or ExecStart changed shape", len(services), services)
	}

	for _, name := range services {
		dir := filepath.Join(root, "cmd", name)
		found := false
		for _, f := range goFilesIn(t, dir) {
			pkg := localName(f, resourcesPath)
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				// In main itself: a helper that calls it is a helper
				// somebody can stop calling, and the limit is only worth
				// anything if it is set before the first large allocation.
				if !ok || fn.Name.Name != "main" || fn.Recv != nil || fn.Body == nil {
					continue
				}
				applies := callsOf(fn.Body, pkg, "Apply")
				if len(applies) > 0 {
					found = true
					checkAppliedAfterTheLogTree(t, name, f, fn, applies)
				}
			}
		}
		if !found {
			t.Errorf("cmd/%s runs as a service (a systemd unit starts it) and its main "+
				"does not call resources.Apply.\nWithout it the Go runtime does not know "+
				"the memory limit it lives under - measured, it reads none inside a "+
				"64 MB cgroup - and a service near its ceiling is killed by the kernel "+
				"rather than collecting harder.", name)
		}
	}
}

const loggingPath = "github.com/cruciblelab/crucible-analytic/internal/logging"

// checkAppliedAfterTheLogTree holds the second half of the rule: the line
// resources.Apply writes has to reach the log tree.
//
// Z1's first version called Apply straight after flag.Parse, which is
// before logging.Setup - so the one line saying what limit the process
// found went to the bootstrap logger, which is stderr. An installation
// configures a log directory, and from then on the operator reads the
// tree; the line was in neither the tree nor the panel's copy of it.
// Found by reading a measurement's own log, and KURULUM had already told
// the operator to look for it.
//
// Two conditions, because the first alone lets a real mistake through:
// Apply placed after Setup but before the logger variable is swapped to
// the tree still logs to stderr. So the argument has to be a variable
// assigned between Setup and the call.
func checkAppliedAfterTheLogTree(t *testing.T, service string, f *ast.File, fn *ast.FuncDecl, applies []*ast.CallExpr) {
	t.Helper()
	setups := callsOf(fn.Body, localName(f, loggingPath), "Setup")
	if len(setups) == 0 {
		t.Errorf("cmd/%s: main calls resources.Apply but never logging.Setup, so "+
			"there is no log tree for the budget line to reach - and this rule has "+
			"nothing to order it against", service)
		return
	}
	setup := setups[0]
	for _, call := range applies {
		if call.Pos() < setup.Pos() {
			t.Errorf("cmd/%s: resources.Apply runs before logging.Setup.\nThe line "+
				"saying which memory limit the process found then goes to the bootstrap "+
				"logger - stderr - and never reaches the log tree an installation's "+
				"operator reads.", service)
			continue
		}
		if len(call.Args) != 1 {
			continue
		}
		arg, ok := call.Args[0].(*ast.Ident)
		if !ok {
			t.Errorf("cmd/%s: resources.Apply is given %T, not a variable this rule "+
				"can follow back to the log tree", service, call.Args[0])
			continue
		}
		if !assignedBetween(fn.Body, arg.Name, setup.End(), call.Pos()) {
			t.Errorf("cmd/%s: resources.Apply(%s) comes after logging.Setup, but %s "+
				"is not reassigned in between - it is still the bootstrap logger, and "+
				"the budget line still goes only to stderr", service, arg.Name, arg.Name)
		}
	}
}

// assignedBetween reports whether body assigns to the variable name at a
// position after from and before to.
func assignedBetween(body *ast.BlockStmt, name string, from, to token.Pos) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || as.Pos() <= from || as.Pos() >= to {
			return true
		}
		for _, lhs := range as.Lhs {
			if id, ok := lhs.(*ast.Ident); ok && id.Name == name {
				found = true
			}
		}
		return true
	})
	return found
}
