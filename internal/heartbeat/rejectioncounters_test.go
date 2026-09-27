package heartbeat

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// TestRejectionCountersIsEveryReason holds the list to the constants.
//
// The list is how a reader knows which counters are parts of
// CounterRejected, and the page draws exactly the list. So a reason
// declared here and left out of it would be counted, carried in every
// heartbeat row, and drawn nowhere - the quiet failure the panel's own
// counter tests exist for, one level down where they cannot see it.
//
// The set is read from this package's source, by name: every
// CounterRejected<Something> constant. Both directions, and once each.
func TestRejectionCountersIsEveryReason(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "heartbeat.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]string{} // value -> constant name
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs := spec.(*ast.ValueSpec)
			for i, name := range vs.Names {
				if !strings.HasPrefix(name.Name, "CounterRejected") || name.Name == "CounterRejected" {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					t.Fatalf("%s is not a string literal; this test reads the values from source", name.Name)
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				declared[value] = name.Name
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no CounterRejected<Reason> constants; the test is not reading what it thinks it is")
	}

	listed := map[string]int{}
	for _, key := range RejectionCounters {
		listed[key]++
		if _, ok := declared[key]; !ok {
			t.Errorf("RejectionCounters lists %q, which is not a CounterRejected<Reason> constant", key)
		}
		if listed[key] > 1 {
			t.Errorf("RejectionCounters lists %q twice; the page would draw one reason as two", key)
		}
	}
	for value, name := range declared {
		if listed[value] == 0 {
			t.Errorf("%s (%q) is a reason the beacon can count and RejectionCounters leaves out, "+
				"so no page would ever draw it", name, value)
		}
		// Named after its total, so a reader of a raw row can tell which
		// numbers add up to which.
		if !strings.HasPrefix(value, CounterRejected+"_") {
			t.Errorf("%s is %q; a part of %q should start with %q", name, value, CounterRejected, CounterRejected+"_")
		}
	}
}
