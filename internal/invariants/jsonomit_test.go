package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// No JSON tag uses omitempty on a type omitempty cannot omit.
//
// # The defect
//
// `json:"x,omitempty"` does nothing to a struct-typed field. encoding/json
// omits a field when it is a false, 0, "", a nil pointer or interface, or
// an empty array, slice, map or string - a struct is none of those, so
// the key is written whatever it holds. There is no error, no vet
// warning, and the field marshals *correctly*: the tag simply does not
// do the one thing it was written for.
//
// Found in privacy.Notice.EffectiveSince, on the one field where it
// mattered. That field is the date privacy.ip_storage was last written,
// and it is zero on every deployment that never changed the setting from
// the panel - which is most of them. With omitempty the JSON carried
//
//	"effective_since": "0001-01-01T00:00:00Z"
//
// and the only reader of that field is a customer's own privacy page
// comparing it against the date it last saw. A page doing that would
// have read a change where there was none, on every poll, forever.
//
// The fix is omitzero (Go 1.24), which consults IsZero and leaves the
// key out.
//
// # Why a structural rule and not a fixed test
//
// Because the defect is invisible at the point it is written and the
// next one will be written by somebody who read this same line. Both
// spellings compile, both look deliberate, and the difference only shows
// up in a consumer that is not in this repository. Nothing about
// reviewing the diff would catch it.
//
// Arrays are in the rule for the same reason and by the same mechanism:
// a [4]byte is never empty either, and `omitempty` on one is the same
// silent no-op.
//
// # The two-way mirror
//
// The derived side is every JSON-tagged field in the tree whose written
// type omitempty cannot omit. The hand-written side is `deliberate`
// below - fields where an always-present key is the intention, each with
// a reason. It is empty today, and an entry that stops matching a real
// field fails too: an exemption nobody needs is an exemption nobody
// re-reads.
func TestNoJSONTagUsesOmitemptyOnATypeItCannotOmit(t *testing.T) {
	// Fields where the key being always present is deliberate. Empty
	// today. Keyed "package/path.Type.Field" with the reason beside it.
	deliberate := map[string]string{}

	found := scanOmitempty(t)

	// A scan that reached nothing would agree with any list. The tree
	// has hundreds of JSON tags; a walk reading fewer than a hundred is
	// reading the wrong thing.
	if found.tags < 100 {
		t.Fatalf("only %d json tags seen in %d files; this scan is not reading the tree",
			found.tags, found.files)
	}

	for _, v := range found.violations {
		if v.undecided {
			t.Errorf("%s (%s:%d) carries `omitempty` on a %s, and this rule cannot "+
				"tell from the spelling whether omitempty can omit it.\n"+
				"Classify it: add the type to stdlibStructs if it is a struct (the "+
				"tag then does nothing and the field needs omitzero), or leave a note "+
				"in `deliberate` saying it is not. Passing it over silently is the "+
				"answer that under-reports.", v.id, v.file, v.line, v.kind)
			continue
		}
		if why, ok := deliberate[v.id]; ok {
			t.Logf("%s: omitempty on %s, deliberate: %s", v.id, v.kind, why)
			continue
		}
		t.Errorf("%s (%s:%d) has `omitempty` on a %s, which omitempty cannot omit.\n"+
			"The key is written on every encode whatever the field holds - no error, "+
			"no vet warning, and the value marshals correctly, so nothing in this "+
			"repository notices. A consumer comparing the field against the last "+
			"value it saw reads a zero where the design meant absence.\n"+
			"Use `omitzero` (Go 1.24, consults IsZero), or add %s to `deliberate` "+
			"in this test with the reason the key belongs there.",
			v.id, v.file, v.line, v.kind, v.id)
	}

	// And the other direction: an exemption for a field that is no
	// longer written that way.
	live := map[string]bool{}
	for _, v := range found.violations {
		if !v.undecided {
			live[v.id] = true
		}
	}
	for id := range deliberate {
		if !live[id] {
			t.Errorf("`deliberate` names %s, which no longer carries omitempty on a "+
				"type it cannot omit. Remove the entry - an exemption nobody needs is "+
				"an exemption nobody re-reads.", id)
		}
	}
}

// The scan can see the defect it looks for.
//
// A detector's silence does not show that it reached the thing it looks
// for; this repository has been fooled by exactly that before. So the
// same function that reads the tree is pointed at a source with one of
// each shape, and at the spellings that must *not* be reported - a
// pointer, a slice, a string, and the same struct field written with
// omitzero.
//
// Without this the test above would pass just as happily with a walk
// that parsed nothing, a tag reader that never matched, or a type test
// that answered false for every struct.
func TestTheOmitemptyScanFindsTheShapesItClaimsTo(t *testing.T) {
	const src = `package p

import (
	"net/netip"
	"time"
)

type Inner struct{ A int }

type Sample struct {
	Caught    time.Time  ` + "`json:\"caught,omitempty\"`" + `
	Also      Inner      ` + "`json:\"also,omitempty\"`" + `
	Addr      netip.Addr ` + "`json:\"addr,omitempty\"`" + `
	Fixed     [4]byte    ` + "`json:\"fixed,omitempty\"`" + `
	Anonymous struct{ B int } ` + "`json:\"anon,omitempty\"`" + `

	Fine      *time.Time ` + "`json:\"fine,omitempty\"`" + `
	AlsoFine  []Inner    ` + "`json:\"also_fine,omitempty\"`" + `
	StillFine string     ` + "`json:\"still_fine,omitempty\"`" + `
	Correct   time.Time  ` + "`json:\"correct,omitzero\"`" + `
	Untagged  time.Time
	NoOption  time.Time  ` + "`json:\"no_option\"`" + `
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "sample.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}

	structs := map[string]bool{"p.Inner": true, "p.Sample": true}
	var got []string
	for _, v := range omitemptyIn(fset, f, "p", "sample.go", structs, map[string]string{}) {
		got = append(got, strings.TrimPrefix(v.id, "p.Sample."))
	}
	sort.Strings(got)

	want := []string{"Addr", "Also", "Anonymous", "Caught", "Fixed"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the scan reports %v, want %v.\n"+
			"Missing entries mean the rule above cannot see that shape; extra ones "+
			"mean it reports a spelling omitempty handles correctly.", got, want)
	}

	// The undecided verdict fires, and says which type it could not
	// classify.
	//
	// Asserted because an unexercised branch is a branch that can be
	// wrong for free - and this one is the rule's whole defence against
	// a type it has never heard of: without it such a field is passed
	// over as "omitempty works here", which is the answer that hides
	// the defect.
	const outside = `package p

import "github.com/somebody/else/thing"

type Q struct {
	Unknown thing.Value ` + "`json:\"unknown,omitempty\"`" + `
}
`
	of, err := parser.ParseFile(token.NewFileSet(), "outside.go", outside, 0)
	if err != nil {
		t.Fatal(err)
	}
	unknown := omitemptyIn(token.NewFileSet(), of, "p", "outside.go",
		map[string]bool{"p.Q": true}, map[string]string{})
	switch {
	case len(unknown) != 1:
		t.Errorf("a field of a type from outside this module produced %d findings, "+
			"want one question", len(unknown))
	case !unknown[0].undecided:
		t.Error("a type this rule cannot classify was reported as a violation rather " +
			"than as a question; the message would tell somebody to use omitzero on a " +
			"type where omitempty may be exactly right")
	case !strings.Contains(unknown[0].kind, "thing.Value"):
		t.Errorf("the question does not name the type it could not classify: %q",
			unknown[0].kind)
	}

	// And every entry of stdlibStructs, not just the two the source
	// above happens to name.
	//
	// The first version of this test checked time.Time and netip.Addr
	// and the comment on stdlibStructs said the list was checked here.
	// Five of its seven entries were not: a typo in any of them - or a
	// type that stopped being a struct - would have been a hole in the
	// rule with a comment claiming otherwise. The fixture is generated
	// from the map so the two cannot drift.
	for named := range stdlibStructs {
		pkg, name, ok := strings.Cut(named, ".")
		if !ok {
			t.Errorf("stdlibStructs key %q is not pkg.Type", named)
			continue
		}
		source := "package q\n\nimport \"" + stdlibImport[pkg] + "\"\n\n" +
			"type S struct {\n\tF " + named + " `json:\"f,omitempty\"`\n}\n"
		qf, err := parser.ParseFile(token.NewFileSet(), "q.go", source, 0)
		if err != nil {
			t.Errorf("stdlibStructs entry %q does not make a parseable field: %v", named, err)
			continue
		}
		found := omitemptyIn(token.NewFileSet(), qf, "q", "q.go",
			map[string]bool{"q.S": true}, map[string]string{})
		if len(found) != 1 {
			t.Errorf("the scan does not report `omitempty` on a %s field, although "+
				"stdlibStructs says it is a struct.\nEvery entry of that map is a "+
				"promise the rule can see; an entry it cannot see is a hole with a "+
				"comment over it.", named)
			continue
		}
		if !strings.Contains(found[0].kind, name) {
			t.Errorf("the scan reports a %s field as %q, which does not name the type",
				named, found[0].kind)
		}
	}
}

// stdlibImport is the import path each qualifier in stdlibStructs comes
// from, for the generated fixture above.
//
// Here rather than derived from the qualifier because "big" is
// math/big and "netip" is net/netip - a qualifier is not a path, and
// guessing would make the fixture fail to parse for a reason that has
// nothing to do with the rule.
var stdlibImport = map[string]string{
	"time":  "time",
	"netip": "net/netip",
	"url":   "net/url",
	"big":   "math/big",
	"sync":  "sync",
}

type omitemptyUse struct {
	id   string // package/path.Type.Field
	kind string // what the field's type is, for the message
	file string
	line int
	// undecided is set when the type could not be classified from the
	// spelling - a type from outside this module that stdlibStructs
	// does not name. Not a violation and not a pass: a question.
	undecided bool
}

type omitemptyScan struct {
	violations []omitemptyUse
	tags       int
	files      int
}

// scanOmitempty walks the tree twice: once to learn which named types
// are structs, once to read the tags.
//
// Twice because a field's type is written before the reader knows what
// it is - `Foo` in one file is a struct declared in another, and
// `pkg.Foo` is a struct declared in a package this walk has not reached
// yet. A single pass would have to guess, and a rule that guesses in the
// safe direction reports nothing.
func scanOmitempty(t *testing.T) omitemptyScan {
	t.Helper()
	root := repoRoot(t)

	structs := map[string]bool{} // "internal/privacy.Notice"
	type source struct {
		f   *ast.File
		pkg string
		rel string
	}
	var parsed []source

	fset := token.NewFileSet()
	var out omitemptyScan
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
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			pkg := filepath.ToSlash(filepath.Dir(rel))
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			out.files++
			for name := range structTypesIn(f) {
				structs[pkg+"."+name] = true
			}
			parsed = append(parsed, source{f, pkg, filepath.ToSlash(rel)})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if out.files < 50 {
		t.Fatalf("only %d source files parsed; the walk is not reading the tree", out.files)
	}

	for _, p := range parsed {
		out.violations = append(out.violations,
			omitemptyIn(fset, p.f, p.pkg, p.rel, structs, modulePackages(p.f))...)
		out.tags += jsonTagsIn(p.f)
	}
	return out
}

// structTypesIn returns the names this file declares as struct types.
func structTypesIn(f *ast.File) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		if _, isStruct := spec.Type.(*ast.StructType); isStruct {
			out[spec.Name.Name] = true
		}
		return true
	})
	return out
}

// modulePackages maps each import's local name to the package directory
// it refers to, for imports inside this module.
//
// Only this module's own packages: a type from a dependency cannot be
// resolved by reading this tree, and none of them appear on a
// JSON-tagged field here. If one ever does, it lands in unknownStructs
// below rather than passing silently.
func modulePackages(f *ast.File) map[string]string {
	const mod = "github.com/cruciblelab/crucible-analytic/"
	out := map[string]string{}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if !strings.HasPrefix(path, mod) {
			continue
		}
		dir := strings.TrimPrefix(path, mod)
		name := dir[strings.LastIndex(dir, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = dir
	}
	return out
}

// stdlibStructs are the standard-library types this rule knows are
// structs.
//
// A short list rather than a type checker: go/types would need the
// packages built, and the question here is answerable from the spelling
// for every type this tree actually puts a JSON tag on. Anything not
// listed and not local is treated as omittable, which is the direction
// that under-reports - so the list is checked against the tree by
// TestTheOmitemptyScanFindsTheShapesItClaimsTo, which fails if a shape
// stops being recognised.
var stdlibStructs = map[string]bool{
	"time.Time":      true,
	"netip.Addr":     true,
	"netip.Prefix":   true,
	"netip.AddrPort": true,
	"url.URL":        true,
	"big.Int":        true,
	"sync.Mutex":     true,
}

// omitemptyIn reports the fields in this file that carry omitempty on a
// type omitempty cannot omit.
func omitemptyIn(fset *token.FileSet, f *ast.File, pkg, rel string, structs map[string]bool, imports map[string]string) []omitemptyUse {
	var out []omitemptyUse
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok {
			return true
		}
		st, ok := spec.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, field := range st.Fields.List {
			if field.Tag == nil || !hasJSONOption(field.Tag.Value, "omitempty") {
				continue
			}
			kind, never := neverEmpty(field.Type, pkg, structs, imports)
			if kind == "" {
				continue // omitempty means something for this type
			}
			for _, name := range field.Names {
				out = append(out, omitemptyUse{
					id:        pkg + "." + spec.Name.Name + "." + name.Name,
					kind:      kind,
					file:      rel,
					line:      fset.Position(name.Pos()).Line,
					undecided: !never,
				})
			}
		}
		return true
	})
	return out
}

// neverEmpty says whether omitempty can omit a field of this written
// type, and names the type for the failure message.
func neverEmpty(expr ast.Expr, pkg string, structs map[string]bool, imports map[string]string) (string, bool) {
	switch e := expr.(type) {
	case *ast.StructType:
		return "struct literal", true
	case *ast.ArrayType:
		if e.Len != nil {
			return "fixed-size array", true
		}
		return "", false // a slice is omitted when empty
	case *ast.Ident:
		if structs[pkg+"."+e.Name] {
			return "struct (" + e.Name + ")", true
		}
		return "", false
	case *ast.SelectorExpr:
		qualifier, ok := e.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		named := qualifier.Name + "." + e.Sel.Name
		if dir, local := imports[qualifier.Name]; local {
			if structs[dir+"."+e.Sel.Name] {
				return "struct (" + named + ")", true
			}
			return "", false
		}
		if stdlibStructs[named] {
			return "struct (" + named + ")", true
		}
		// A type from outside this module that nothing here classifies.
		//
		// Reported rather than passed over, and that is the whole
		// direction of this rule. Treating an unknown type as omittable
		// is the answer that under-reports: the day somebody adds an
		// `omitempty` to a field of some dependency's struct type, a
		// silent "not a struct" would let it through, and the key would
		// be written on every encode with nothing failing.
		//
		// It also fixes the hole a mutation found here. The positive
		// control below checks that every entry of stdlibStructs is
		// recognised, but it cannot see an entry *removed* - nothing in
		// the standard library will tell this test what is a struct. So
		// removing url.URL from the list must not make the rule quietly
		// weaker; with this branch it makes the rule say "I cannot
		// decide this one", which is a sentence a person reads.
		return "unclassified type " + named, false
	}
	// Pointers, maps, interfaces, channels, functions and the basic
	// types: omitempty means something for all of them.
	return "", false
}

// hasJSONOption reports whether a struct tag's json entry carries this
// option.
func hasJSONOption(tag, option string) bool {
	value, ok := reflect.StructTag(strings.Trim(tag, "`")).Lookup("json")
	if !ok {
		return false
	}
	parts := strings.Split(value, ",")
	for _, part := range parts[1:] {
		if part == option {
			return true
		}
	}
	return false
}

// jsonTagsIn counts the json struct tags in a file, so the scan can say
// whether it read anything at all.
func jsonTagsIn(f *ast.File) int {
	n := 0
	ast.Inspect(f, func(node ast.Node) bool {
		field, ok := node.(*ast.Field)
		if !ok || field.Tag == nil {
			return true
		}
		if _, has := reflect.StructTag(strings.Trim(field.Tag.Value, "`")).Lookup("json"); has {
			n++
		}
		return true
	})
	return n
}
