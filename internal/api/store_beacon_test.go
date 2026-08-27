package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"testing"
)

// TestEveryBreakdownExprIsAllowed reads the constants out of this file's
// source and requires valid() to accept every one of them.
//
// The allowlist in valid() is written by hand, and a hand-written list
// mirroring a constant block is a list that drifts. The drift is silent
// in the direction that matters twice over: a new dimension added to the
// constants and forgotten in valid() makes a working breakdown start
// answering "not a beacon breakdown column", and the person who adds it
// discovers that from a bug report rather than a test.
//
// Read from the source rather than kept as a second list here, which
// would only move the drift one file along.
func TestEveryBreakdownExprIsAllowed(t *testing.T) {
	declared := declaredBreakdownExprs(t)
	if len(declared) < 5 {
		t.Fatalf("only %d breakdown constants found; the scan is broken, not the code", len(declared))
	}
	for name, value := range declared {
		if err := value.valid(); err != nil {
			t.Errorf("%s (%q) is a declared breakdown column that valid() refuses: %v", name, value, err)
		}
	}
}

// And the other direction: anything that is not one of them is refused.
// This is the assertion the SQL interpolation rests on.
func TestBreakdownExprRefusesAnythingElse(t *testing.T) {
	for _, bad := range []breakdownExpr{
		"",
		"path; DROP TABLE beacon_events",
		"1",
		"visitor_id",
		"path)) UNION SELECT password_sealed FROM panel_smtp --",
		"PATH",
	} {
		if err := bad.valid(); err == nil {
			t.Errorf("valid() accepted %q, which is interpolated straight into SQL", bad)
		}
	}
}

func declaredBreakdownExprs(t *testing.T) map[string]breakdownExpr {
	t.Helper()

	src, err := os.ReadFile("store_beacon.go")
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), "store_beacon.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}

	out := make(map[string]breakdownExpr)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := value.Type.(*ast.Ident)
			if !ok || ident.Name != "breakdownExpr" {
				continue
			}
			for i, name := range value.Names {
				if i >= len(value.Values) {
					continue
				}
				lit, ok := value.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				unquoted, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out[name.Name] = breakdownExpr(unquoted)
			}
		}
	}
	return out
}
