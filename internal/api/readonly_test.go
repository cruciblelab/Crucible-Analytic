package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every route this API registers names GET, read out of the router
// rather than remembered.
//
// # The jewel this guards
//
// PLAN.md §3.5, J4: *the support token can never write.* The owner's
// words: "yoken yazamaz ama analitiği okuyabilecek güçte olmalı."
//
// That promise has two layers. The database layer is held by
// readonlyrole_integration_test.go: the role this service connects as
// holds SELECT and three named exceptions. This file holds the other
// one, and it is the cheaper of the two to break.
//
// # The failure this exists for, and it is not "somebody adds a POST"
//
// A POST route would at least be visible in review. The quiet one is a
// registration with no method at all:
//
//	mux.HandleFunc("/api/v1/sites/{site}/summary", s.handler)
//
// Go's ServeMux matches that pattern for every method. The handler only
// reads, so nothing misbehaves and no test fails - but the surface of
// the API has stopped being read-only, and the next handler written
// against that pattern inherits a door nobody meant to open. A method
// is one word, it is invisible when missing, and the whole promise is
// in it.
//
// # Why the source and not the mux
//
// A runtime probe can only ask about routes it knows to ask about, so
// it answers "the routes I listed reject POST" - which is the shape of
// assertion isolation_test.go was written to replace. Reading the
// registrations means a route added tomorrow is in the answer without
// anybody adding it to a list.
//
// A registration this file cannot read is a failure rather than a skip,
// for the same reason: the failure mode being guarded against is a
// route nobody looked at.
//
// *İstemciye güvenme, sadece sunucuya güven* - ve sunucunun yüzeyi
// kayıttaki satırdır.
func TestEveryRegisteredRouteNamesGET(t *testing.T) {
	dir := "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	var patterns []string

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		patterns = append(patterns, registeredPatterns(t, fset, f)...)
	}

	// Vacuity. A reader that finds nothing reports the same silence as
	// an API with no routes, and this package has twenty-odd.
	if len(patterns) == 0 {
		t.Fatal("no route registrations were found at all; this test is looking in " +
			"the wrong place and would have passed whatever the router did")
	}

	for _, p := range patterns {
		if !strings.HasPrefix(p, "GET ") {
			t.Errorf("route %q does not name GET.\n"+
				"This API is the read-only one: the token handed to a caller is "+
				"promised to read and never write, and a pattern without a method "+
				"matches POST, PUT, PATCH and DELETE as well. If a write endpoint "+
				"is genuinely wanted here, that is a decision about what the support "+
				"token is, not a routing detail - PLAN.md §3.5 J4.", p)
		}
	}
}

// registeredPatterns is every pattern passed to HandleFunc in f,
// including the method prefix.
//
// Deliberately not routesIn from isolation_test.go, which answers a
// different question: that one keeps only per-site routes and strips the
// method, because it probes site authorization. This one keeps every
// route and keeps the method, because the method is the claim. They
// read the same registrations for two different reasons, and both fail
// rather than skip on a shape they cannot read - so neither hides
// behind the other.
func registeredPatterns(t *testing.T, fset *token.FileSet, f *ast.File) []string {
	t.Helper()

	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "HandleFunc" {
			return true
		}
		switch pattern := call.Args[0].(type) {
		case *ast.BasicLit:
			if s, ok := stringLit(pattern); ok {
				out = append(out, s)
			} else {
				t.Errorf("%s: a route pattern is a string this test cannot unquote",
					fset.Position(pattern.Pos()))
			}
		case *ast.BinaryExpr:
			// prefix + something. The method lives in the prefix, which
			// is the only part this test needs; what the suffix expands
			// to is isolation_test.go's question.
			if s, ok := stringLit(pattern.X); ok {
				out = append(out, s)
			} else {
				t.Errorf("%s: a route is built from an expression whose leading "+
					"string this test cannot read, so it cannot tell which method "+
					"the route answers", fset.Position(pattern.Pos()))
			}
		default:
			t.Errorf("%s: route registered with an unrecognised pattern expression; "+
				"this test fails rather than skipping it, because an unread "+
				"registration is exactly the thing it exists to notice",
				fset.Position(pattern.Pos()))
		}
		return true
	})
	return out
}
