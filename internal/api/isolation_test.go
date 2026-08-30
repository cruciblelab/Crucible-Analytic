// Every per-site route, read out of the router rather than remembered.
//
// The API's site boundary is one wrapper: siteHandler checks the bearer
// token's grant and refuses anything outside it. That wrapper is correct.
// The risk was never the wrapper - it is that a route gets registered
// without it, and nothing notices, because the tests that cover this
// boundary were two lists somebody typed.
//
// Those lists were measured against the router and they were short:
//
//	server.go + server_beacon.go register 34 per-site routes
//	server_test.go's list covers  9
//	server_beacon_test.go's list covers 17
//	                             ----
//	              never probed:     8
//
// The eight - beacon/titles, beacon/utm-sources, beacon/utm-mediums,
// beacon/utm-campaigns, beacon/utm-terms, beacon/utm-contents,
// beacon/refs, beacon/click-sources - all go through siteHandler and all
// behave correctly today. That is the point. They are correct because
// one `for` loop wraps them, and the day somebody registers the
// sixteenth breakdown outside that loop, or lifts one out of it to give
// it a special case, nothing in the suite would have said a word.
//
// So this file does not keep a list. It reads the registrations out of
// the source, expands the loop that generates fifteen of them, and
// probes every one. A route registered in a shape this file cannot read
// is a failure, not a silent skip - the whole failure mode being guarded
// against is a route nobody looked at.
//
// No build tag: it parses two files in this package and drives the
// handler through httptest, so it belongs in the merge gate.
package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// perSiteRoutes returns every route pattern the API registers that names
// a {site}, with {site} and {ip} still in place.
func perSiteRoutes(t *testing.T) []string {
	t.Helper()

	var found []string
	for _, file := range []string{"server.go", "server_beacon.go"} {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		found = append(found, routesIn(t, fset, f)...)
	}

	sort.Strings(found)
	return found
}

// routesIn walks one file for every mux.HandleFunc whose pattern names a
// {site}.
//
// Deliberately *not* "every call that wraps siteHandler". The first draft
// of this file did that, and it had the defect it was written to remove:
// a route registered without the wrapper is exactly the bug, and keying
// off the wrapper made that route invisible to the scan. The pattern is
// the honest question - this URL carries a site id, so what does it do
// with another customer's? - and it is answerable whether or not the
// registration remembered its wrapper.
func routesIn(t *testing.T, fset *token.FileSet, f *ast.File) []string {
	t.Helper()

	// Loop variables that range over a map literal, so a pattern built as
	// "…/beacon/" + path can be expanded into the fifteen paths that loop
	// actually registers. Without this the group breakdowns would be
	// invisible to this test, which is the same blindness it exists to
	// remove.
	rangeKeys := map[string][]string{}
	ast.Inspect(f, func(n ast.Node) bool {
		rng, ok := n.(*ast.RangeStmt)
		if !ok || rng.Key == nil {
			return true
		}
		key, ok := rng.Key.(*ast.Ident)
		if !ok {
			return true
		}
		lit, ok := rng.X.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if _, ok := lit.Type.(*ast.MapType); !ok {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if s, ok := stringLit(kv.Key); ok {
				rangeKeys[key.Name] = append(rangeKeys[key.Name], s)
			}
		}
		return true
	})

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
			s, _ := stringLit(pattern)
			if p := strings.TrimPrefix(s, "GET "); strings.Contains(p, "{site}") {
				out = append(out, p)
			}
		case *ast.BinaryExpr:
			// prefix + loopVariable
			prefix, ok := stringLit(pattern.X)
			if !ok || !strings.Contains(prefix, "{site}") {
				if !ok {
					t.Errorf("%s: a route is built from something this test cannot "+
						"read, so it cannot tell whether it is per-site; every "+
						"registration has to be visible here or it goes unprobed",
						fset.Position(pattern.Pos()))
				}
				return true
			}
			ident, ok := pattern.Y.(*ast.Ident)
			if !ok || len(rangeKeys[ident.Name]) == 0 {
				t.Errorf("%s: a per-site route is built from %s, which is not a "+
					"loop over a map literal this test can expand", fset.Position(pattern.Pos()), describe(pattern.Y))
				return true
			}
			for _, suffix := range rangeKeys[ident.Name] {
				out = append(out, strings.TrimPrefix(prefix, "GET ")+suffix)
			}
		default:
			t.Errorf("%s: route registered with an unrecognised pattern expression, "+
				"so this test cannot tell whether it is per-site; it fails rather "+
				"than skipping it", fset.Position(pattern.Pos()))
		}
		return true
	})
	return out
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

func describe(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "an expression"
}

// requestable turns a registered pattern into something requestable.
func requestable(pattern, site string) string {
	p := strings.ReplaceAll(pattern, "{site}", site)
	return strings.ReplaceAll(p, "{ip}", "203.0.113.1")
}

// TestEveryPerSiteRouteRefusesAnotherSitesToken.
//
// The claim the product makes to a customer who is handed an API token:
// this token reads your site and no other site on this machine. It is
// worth one assertion per door, because the cost of one door being wrong
// is one customer reading another's traffic with a valid credential and
// nothing in any log looking unusual.
func TestEveryPerSiteRouteRefusesAnotherSitesToken(t *testing.T) {
	h := newTestServer(t, &fakeStore{})
	routes := perSiteRoutes(t)

	// A floor, so a parser that stopped recognising registrations cannot
	// pass by finding nothing. The number is deliberately below the real
	// count: this guards against zero, it does not pin the total.
	if len(routes) < 30 {
		t.Fatalf("found %d per-site routes, which is fewer than this API has; "+
			"the registrations have changed shape and this test is now reading past them", len(routes))
	}

	for _, pattern := range routes {
		t.Run(strings.TrimPrefix(pattern, "/api/v1/sites/{site}/"), func(t *testing.T) {
			// site-a is ahmet's. site-b belongs to somebody else.
			if w := do(h, http.MethodGet, requestable(pattern, "site-a"), "ahmet-secret"); w.Code != http.StatusOK {
				t.Errorf("reading its own site -> %d, want 200; body %s", w.Code, w.Body)
			}
			w := do(h, http.MethodGet, requestable(pattern, "site-b"), "ahmet-secret")
			if w.Code != http.StatusForbidden {
				t.Errorf("reading another customer's site -> %d, want 403. "+
					"This route reached a handler without the token's grant being checked", w.Code)
			}
		})
	}
}

// TestARefusedRouteSaysNothingAboutWhetherTheSiteExists.
//
// The API answers 403 here where the panel answers 404, and that is a
// deliberate difference documented on siteHandler: a token holder is an
// operator reading their own configuration, not an anonymous browser,
// and telling them "your token does not cover this" beats making a
// misconfigured token look like a missing site.
//
// The difference is defensible only as long as the 403 is the same
// whether the site exists or not. If it ever varied, the endpoint would
// become the enumeration oracle the panel is careful not to be, reachable
// by any customer holding any valid token.
func TestARefusedRouteSaysNothingAboutWhetherTheSiteExists(t *testing.T) {
	// site-b exists in the store; nobody-here does not.
	h := newTestServer(t, &fakeStore{sites: []string{"site-a", "site-b"}})

	for _, pattern := range perSiteRoutes(t) {
		t.Run(strings.TrimPrefix(pattern, "/api/v1/sites/{site}/"), func(t *testing.T) {
			exists := do(h, http.MethodGet, requestable(pattern, "site-b"), "ahmet-secret")
			absent := do(h, http.MethodGet, requestable(pattern, "nobody-here"), "ahmet-secret")

			if exists.Code != absent.Code {
				t.Errorf("status differs: %d for a site that exists, %d for one that does not",
					exists.Code, absent.Code)
			}
			// The site id itself is echoed back, which leaks nothing: the
			// caller typed it. Everything else has to match.
			e := strings.ReplaceAll(exists.Body.String(), "site-b", "<SITE>")
			a := strings.ReplaceAll(absent.Body.String(), "nobody-here", "<SITE>")
			if e != a {
				t.Errorf("the refusals differ:\n  exists: %s\n  absent: %s", e, a)
			}
		})
	}
}

// TestTheHandWrittenRouteListsAreStillComplete is the two-way mirror.
//
// Two older tests - TestServer_SiteAuthorizationCoversEveryPerSiteRoute
// and the beaconRoutes list - assert useful things about routes they name
// individually, and there is no way to derive their concrete URLs from
// the router alone. They stay. What they cannot do is notice a route
// nobody added them to, so that is asked here instead: every registered
// per-site route has to appear in one of those two lists.
//
// This is the check whose absence let eight routes go unprobed.
func TestTheHandWrittenRouteListsAreStillComplete(t *testing.T) {
	covered := map[string]bool{}
	for _, suffix := range perSiteRouteSuffixes {
		covered["/api/v1/sites/{site}/"+suffix] = true
	}
	for _, route := range beaconRoutes {
		covered[strings.Replace(route, "site-a", "{site}", 1)] = true
	}

	var missing []string
	for _, pattern := range perSiteRoutes(t) {
		// {ip} is a wildcard the hand lists spell with a concrete address.
		p := strings.ReplaceAll(pattern, "{ip}", "203.0.113.1")
		if !covered[p] && !covered[pattern] {
			missing = append(missing, pattern)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d per-site route(s) appear in the router and in neither hand-written list:\n  %s\n"+
			"Add them to perSiteRouteSuffixes or beaconRoutes, so the assertions those lists carry "+
			"run against them too", len(missing), strings.Join(missing, "\n  "))
	}
}
