package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A narrowed area must stay narrowed.
//
// internal/panel/web/stores.go declares one interface per area of the
// panel - what the backup section may ask the database for, what the
// version section may, and so on - and an accessor that hands it over.
// The whole point is that the compiler then refuses anything else: the
// backup page cannot read a user record, because backupStore has no
// method that does.
//
// That guarantee has exactly one hole, and it is the one this file
// closes. Server still carries the wide *panel.Store, so a line reading
// `s.Store.Whatever(...)` inside a narrowed file compiles perfectly and
// walks straight around the seam. Nothing about it looks wrong in
// review - it is the shorter of the two spellings, and it is what every
// other file in the package still does.
//
// So the rule is checked mechanically: a file that declares a function
// taking one of these interfaces has committed to the seam, and may not
// reach the wide type at all.
//
// # Why the list of narrowed files is derived rather than written
//
// A hand-written list would be right on the day it was written. The list
// here comes out of the syntax tree: any file with a function whose
// parameters name a stores.go interface is in it, so the sixth area is
// covered by the test the moment somebody narrows it - including if they
// forget this file exists.
//
// *Her halkası test edilmiş bir zincir, test edilmiş bir zincir
// değildir.*

// storesFile is where the interfaces are declared.
const storesFile = "stores.go"

// webPackage is the directory these rules are about.
func webPackage(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRootFromInvariants(t), "internal", "panel", "web")
}

// narrowScan is what one pass over the package found.
type narrowScan struct {
	fset *token.FileSet
	// files is every non-test file, by base name.
	files map[string]*ast.File
	// ifaces maps each interface declared in stores.go to the method
	// names it declares, embedded ones included.
	ifaces map[string][]string
	// direct is the methods each interface declares itself, and embeds
	// is the interfaces it embeds. Kept apart from ifaces because "is
	// this method ever called through this interface" is a question
	// about the declaration, not about the flattened set: no parameter
	// is ever typed operationStore, so its one method is only ever
	// called through something that embeds it.
	direct map[string][]string
	embeds map[string][]string
}

func scanWeb(t *testing.T) *narrowScan {
	t.Helper()
	out := &narrowScan{
		fset:   token.NewFileSet(),
		files:  map[string]*ast.File{},
		ifaces: map[string][]string{},
		direct: map[string][]string{},
		embeds: map[string][]string{},
	}
	pkgs, err := parser.ParseDir(out.fset, webPackage(t), func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.ParseComments)
	if err != nil {
		t.Fatalf("parsing internal/panel/web: %v", err)
	}
	pkg, ok := pkgs["web"]
	if !ok {
		t.Fatal("internal/panel/web did not parse as package web, so this test read nothing")
	}
	for path, f := range pkg.Files {
		out.files[filepath.Base(path)] = f
	}

	stores, ok := out.files[storesFile]
	if !ok {
		t.Fatalf("internal/panel/web/%s is gone. Either the seams were removed - in which "+
			"case say so and delete this test - or the file was renamed and this check is "+
			"now examining nothing", storesFile)
	}

	// Two passes: collect the declarations first, then flatten the
	// embedded ones, because an interface may embed one declared below
	// it.
	direct, embeds := out.direct, out.embeds
	ast.Inspect(stores, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		it, ok := spec.Type.(*ast.InterfaceType)
		if !ok {
			return true
		}
		name := spec.Name.Name
		direct[name] = nil
		for _, field := range it.Methods.List {
			if len(field.Names) == 0 {
				if id, ok := field.Type.(*ast.Ident); ok {
					embeds[name] = append(embeds[name], id.Name)
				}
				continue
			}
			for _, m := range field.Names {
				direct[name] = append(direct[name], m.Name)
			}
		}
		return true
	})
	if len(direct) == 0 {
		t.Fatalf("no interfaces were found in %s, so this test examined nothing", storesFile)
	}

	var flatten func(name string, depth int) []string
	flatten = func(name string, depth int) []string {
		if depth > 8 {
			t.Fatalf("the interfaces in %s embed each other in a loop", storesFile)
		}
		methods := append([]string(nil), direct[name]...)
		for _, e := range embeds[name] {
			methods = append(methods, flatten(e, depth+1)...)
		}
		sort.Strings(methods)
		return methods
	}
	for name := range direct {
		out.ifaces[name] = flatten(name, 0)
	}
	return out
}

// narrowedFiles are the files that took one of the interfaces as a
// parameter, and so committed to the seam.
func (s *narrowScan) narrowedFiles() map[string][]string {
	out := map[string][]string{}
	for name, f := range s.files {
		if name == storesFile {
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Type.Params == nil {
				continue
			}
			for _, p := range fn.Type.Params.List {
				id, ok := p.Type.(*ast.Ident)
				if !ok {
					continue
				}
				if _, ok := s.ifaces[id.Name]; ok {
					out[name] = append(out[name], fn.Name.Name)
				}
			}
		}
	}
	return out
}

// reachesTheWideStore finds every `x.Store.Method` in a file.
func reachesTheWideStore(fset *token.FileSet, f *ast.File) []string {
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		inner, ok := sel.X.(*ast.SelectorExpr)
		if !ok || inner.Sel.Name != "Store" {
			return true
		}
		if _, ok := inner.X.(*ast.Ident); !ok {
			return true
		}
		out = append(out, fset.Position(sel.Pos()).String()+": .Store."+sel.Sel.Name)
		return true
	})
	return out
}

// TestANarrowedFileDoesNotReachTheWideStore.
func TestANarrowedFileDoesNotReachTheWideStore(t *testing.T) {
	scan := scanWeb(t)
	narrowed := scan.narrowedFiles()
	if len(narrowed) == 0 {
		t.Fatal("no file in internal/panel/web takes one of the stores.go interfaces, so " +
			"either the seams were undone or this test no longer recognises them")
	}

	for _, name := range sortedKeys(narrowed) {
		for _, where := range reachesTheWideStore(scan.fset, scan.files[name]) {
			t.Errorf("%s declares %s, which takes a narrow store, and then reaches the wide "+
				"one anyway:\n    %s\n"+
				"The seam is only worth what the compiler enforces. One line like this and "+
				"the file is back to having all 99 methods available to it, with nothing "+
				"saying so. Add the method to the area's interface in %s, with the reason.",
				name, strings.Join(narrowed[name], ", "), where, storesFile)
		}
	}
}

// TestDrawingASectionCannotQueueWork.
//
// # What goes wrong without it
//
// Every one of these sections is drawn on GET and pressed on POST. The
// two paths are separated by a rule nobody can see: drawing must not
// queue anything. A section builder that requested a backup would turn
// every page load into a backup - and the page refreshes itself while
// one is running, so it would never stop.
//
// stores.go makes it structural instead. The *StatusFor functions take
// the reader interface, which has no method that queues anything, so the
// GET path cannot do it even by mistake.
//
// This checks both halves of that: the readers stay read-only, and the
// section builders keep taking readers rather than quietly widening to
// the full store when somebody needs one more method.
func TestDrawingASectionCannotQueueWork(t *testing.T) {
	scan := scanWeb(t)

	readers := 0
	for _, name := range sortedKeys(scan.ifaces) {
		if !strings.HasSuffix(name, "Reader") {
			continue
		}
		readers++
		for _, m := range scan.ifaces[name] {
			if strings.HasPrefix(m, "Request") || m == "BeginOperation" {
				t.Errorf("%s is a read interface and it declares %s.\n"+
					"The name says the GET path may hold it, and the method says the GET "+
					"path may queue work. One of the two is wrong", name, m)
			}
		}
	}
	if readers == 0 {
		t.Fatalf("no reader interfaces were found in %s, so this test examined nothing",
			storesFile)
	}

	// And the builders still take one.
	found := 0
	for _, name := range sortedKeys(scan.files) {
		for _, decl := range scan.files[name].Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !strings.HasSuffix(fn.Name.Name, "StatusFor") || fn.Type.Params == nil {
				continue
			}
			found++
			ok = false
			for _, p := range fn.Type.Params.List {
				if id, isIdent := p.Type.(*ast.Ident); isIdent &&
					strings.HasSuffix(id.Name, "Reader") {
					ok = true
				}
			}
			if !ok {
				t.Errorf("%s in %s builds a section and does not take a read-only store.\n"+
					"It is called from the GET path. Whatever it was handed, it can now "+
					"queue work on a page load", fn.Name.Name, name)
			}
		}
	}
	if found == 0 {
		t.Fatal("no *StatusFor functions were found in internal/panel/web, so half of this " +
			"test examined nothing")
	}
}

// calledThrough finds, for each interface, the methods actually called on
// a parameter of that type.
//
// # Why this is not "is the name called anywhere in the package"
//
// That was the first version of this check, and a mutation walked
// straight through it: adding CountUsers to diskStore did not fail,
// because server.go calls s.Store.CountUsers and the name was therefore
// "used". The check was answering a question nobody had asked.
//
// So the pairing is what gets recorded: the parameter names bound to
// each interface, and the methods called on those names.
func (s *narrowScan) calledThrough() map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for name, f := range s.files {
		if name == storesFile {
			continue
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Type.Params == nil || fn.Body == nil {
				continue
			}
			// Which local names carry a narrow store in this function.
			bound := map[string]string{}
			for _, p := range fn.Type.Params.List {
				id, ok := p.Type.(*ast.Ident)
				if !ok {
					continue
				}
				if _, ok := s.ifaces[id.Name]; !ok {
					continue
				}
				for _, n := range p.Names {
					bound[n.Name] = id.Name
				}
			}
			if len(bound) == 0 {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				recv, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				iface, ok := bound[recv.Name]
				if !ok {
					return true
				}
				if out[iface] == nil {
					out[iface] = map[string]bool{}
				}
				out[iface][sel.Sel.Name] = true
				return true
			})
		}
	}
	return out
}

// embedders is every interface that reaches iface by embedding, plus
// iface itself.
//
// operationStore is never a parameter type - it exists to be embedded -
// so BeginOperation is only ever called on a backupStore or an
// upgradeStore. Without this the check would report the one method the
// four button areas all share as unused.
func (s *narrowScan) embedders(iface string) []string {
	out := []string{iface}
	for _, other := range sortedKeys(s.direct) {
		if other == iface {
			continue
		}
		if s.reaches(other, iface, 0) {
			out = append(out, other)
		}
	}
	return out
}

func (s *narrowScan) reaches(from, want string, depth int) bool {
	if depth > 8 {
		return false
	}
	for _, e := range s.embeds[from] {
		if e == want || s.reaches(e, want, depth+1) {
			return true
		}
	}
	return false
}

// TestEveryNarrowMethodIsActuallyUsed.
//
// An interface that lists a method nothing calls is an interface that has
// stopped describing an area and started describing the Store again. It
// is the way this drifts back: somebody needs one more method, adds two
// while they are there, and the seam widens without anybody choosing to
// widen it.
func TestEveryNarrowMethodIsActuallyUsed(t *testing.T) {
	scan := scanWeb(t)
	used := scan.calledThrough()
	if len(used) == 0 {
		t.Fatal("no method was found being called on a narrow store, so this test " +
			"examined nothing")
	}

	for _, iface := range sortedKeys(scan.direct) {
		for _, m := range scan.direct[iface] {
			seen := false
			for _, holder := range scan.embedders(iface) {
				if used[holder][m] {
					seen = true
					break
				}
			}
			if !seen {
				t.Errorf("%s declares %s and nothing calls it through that interface.\n"+
					"Each of these is meant to be one area's whole database surface and "+
					"nothing more. A method nobody reaches is the seam widening on its "+
					"own - and it widens what the compiler will allow next time.", iface, m)
			}
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
