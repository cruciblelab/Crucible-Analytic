//go:build integration

package web

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestEveryPostRouteRefusesARequestWithNoToken is the CSRF coverage
// test.
//
// Every state-changing route in this panel goes through acceptPost, and
// the way to know that is to send each one a POST with no token and
// watch it be refused. A route added later that forgets - which is the
// only way this goes wrong - fails here rather than in production.
//
// It runs against a real database on purpose. A storeless server answers
// 503 to everything, so the same test in the unit suite would pass
// without a single handler having been reached.
//
// The refusal is a 419 for a missing token, a 403 or 404 where authority
// or the resource is checked first, or a redirect to the sign-in form
// where there is no session. What must never appear is a 2xx, or a
// redirect anywhere else: both mean the request was carried out.
//
// # Why the routes are read from the source
//
// This used to carry a hand-written list of ten paths, and it had gone
// short: three handlers that call acceptPost - the recovery form, the
// mail page and the settings page - had no entry, so the one test whose
// whole purpose is catching the route somebody forgot was itself
// forgetting three.
//
// A hand list is dangerous not when it is wrong but when it is short.
// Wrong goes red; short stays green. So the list is derived instead:
// every handler in this package that calls acceptPost, paired with the
// pattern server.go registers it under. Adding a state-changing route
// now adds a case here whether or not anybody remembers to.
//
// The same correction B6 made for the per-site isolation tests, for the
// same reason and after the same kind of miss.
func TestEveryPostRouteRefusesARequestWithNoToken(t *testing.T) {
	srv, _ := setupTestServer(t)
	h := srv.Handler()

	routes := stateChangingRoutes(t)
	if len(routes) < 10 {
		t.Fatalf("only %d state-changing routes were derived; the extraction is broken "+
			"and this test would pass by probing almost nothing", len(routes))
	}
	for _, path := range routes {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path,
				strings.NewReader("islem=ekle&eposta=biri@example.invalid"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			switch {
			case rec.Code >= 200 && rec.Code < 300:
				t.Errorf("POST with no CSRF token answered %d; the request was carried out",
					rec.Code)
			case rec.Code == http.StatusSeeOther:
				// A redirect to the sign-in form is a refusal: the
				// request was turned away for having no session, before
				// the token was ever looked at. A redirect anywhere else
				// after a state-changing POST is the success path.
				if to := rec.Header().Get("Location"); !strings.HasPrefix(to, LoginPath) {
					t.Errorf("POST with no CSRF token answered 303 to %q; a redirect "+
						"anywhere but the sign-in form means it happened", to)
				}
			}
		})
	}
}

// TestAStorelessPanelRefusesRatherThanCrashes.
//
// The routes reachable without credentials - the sign-in form, an
// invitation link, a developer link, the first-run page - all read a row
// before they know who is asking, and each one dereferencing a nil Store
// is a remote crash rather than an error page. ListenAndServe refuses to
// start without a Store, so this state should never exist; the guards
// are there because "unreachable" and "takes the process down" are a bad
// pair of properties to hold at the same time.
//
// What is asserted is that the request is *refused*, not which refusal.
// Different routes stop at different points - /giris stops at the CSRF
// check before it ever reaches the database - and pinning each one to a
// status would be a test about handler ordering rather than about
// surviving a missing database. The failure this catches is a panic,
// which fails the test by unwinding through it.
func TestAStorelessPanelRefusesRatherThanCrashes(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.Store = nil
	h := srv.Handler()

	// The routes that read a row before they know who is asking. The
	// sign-in form is here as a POST: drawing it needs no database - and
	// refusing to draw it would mean a panel whose database is briefly
	// unreachable cannot show anybody the page explaining why - but
	// submitting it does.
	for _, path := range []string{
		LoginPath, ClaimPathPrefix + "x", DevAccessPathPrefix + "x",
		SetupPathPrefix + wizardSteps[0].ID,
	} {
		t.Run(path, func(t *testing.T) {
			// A POST, because that is the path that reads a body and then
			// a row - the longest walk before anything checks anything.
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("a=b"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			rec := httptest.NewRecorder()

			// A panic here fails the test rather than the process, which
			// is the difference this middleware exists to make.
			h.ServeHTTP(rec, req)

			if rec.Code < 400 {
				t.Errorf("answered %d with no database; the request was carried out", rec.Code)
			}
		})
	}
}

// TestListenAndServeRefusesWithoutAStore is the other half: a deployment
// in that state must not start at all, so the middleware above is a net
// rather than a feature.
func TestListenAndServeRefusesWithoutAStore(t *testing.T) {
	srv, _ := setupTestServer(t)
	srv.Store = nil
	srv.ListenAddr = "127.0.0.1:0"
	if err := srv.ListenAndServe(t.Context()); err == nil {
		t.Fatal("a panel with no database started")
	}
}

// stateChangingRoutes pairs every handler that calls acceptPost with the
// path server.go registers it under, wildcards filled in.
//
// Two sources, both read from disk: the handlers come from every
// non-test file in this package, the patterns from server.go. Neither
// side is a list anybody maintains, which is the point - see the comment
// on TestEveryPostRouteRefusesARequestWithNoToken.
func stateChangingRoutes(t *testing.T) []string {
	t.Helper()

	guarded := handlersCallingAcceptPost(t)
	if len(guarded) == 0 {
		t.Fatal("no handler was found calling acceptPost; the extraction is broken")
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing server.go: %v", err)
	}

	// Wildcards get a value the router will match. What is behind them
	// does not need to exist: a 404 for a missing site is a refusal, and
	// the assertion is only ever that the POST was not carried out.
	fill := strings.NewReplacer(
		"{site}", "bir-site",
		"{token...}", "bir-jeton",
		"{step...}", "bir-adim",
		"{kirilim}", "bir-kirilim",
		"{liste}", "bir-liste",
	)

	var out []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "HandleFunc" {
			return true
		}
		handler, ok := call.Args[1].(*ast.SelectorExpr)
		if !ok || !guarded[handler.Sel.Name] {
			return true
		}
		pattern, ok := routePattern(call.Args[0])
		if !ok {
			t.Errorf("could not read the pattern for %s; this route would go unprobed",
				handler.Sel.Name)
			return true
		}
		out = append(out, fill.Replace(pattern))
		return true
	})
	return out
}

// handlersCallingAcceptPost is the set of methods whose body calls it.
func handlersCallingAcceptPost(t *testing.T) map[string]bool {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}

	out := map[string]bool{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "acceptPost" {
						out[fn.Name.Name] = true
					}
					return true
				})
			}
		}
	}
	return out
}

// routePattern renders a registration argument back into a path.
//
// The patterns are string literals, named constants, or the two joined
// with +. Constants are resolved by name against the package's own
// values rather than re-parsed, so a renamed constant is a compile
// error here instead of a route that quietly stops being probed.
func routePattern(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.Ident:
		v, ok := routeConstants[e.Name]
		return v, ok
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, okL := routePattern(e.X)
		right, okR := routePattern(e.Y)
		return left + right, okL && okR
	}
	return "", false
}

// routeConstants resolves the names server.go builds its patterns from.
//
// Written out rather than evaluated, because a test that evaluated the
// source would be re-implementing the compiler. Referencing the real
// identifiers means the compiler checks this side: delete or rename one
// and this file stops building.
var routeConstants = map[string]string{
	"DevAccessPathPrefix":    DevAccessPathPrefix,
	"SetupPathPrefix":        SetupPathPrefix,
	"LoginPath":              LoginPath,
	"RecoveryPath":           RecoveryPath,
	"SecondFactorPath":       SecondFactorPath,
	"LogoutPath":             LogoutPath,
	"AccountPath":            AccountPath,
	"TOTPQRPath":             TOTPQRPath,
	"MembersPathPrefix":      MembersPathPrefix,
	"membersPathSuffix":      membersPathSuffix,
	"settingsPathSuffix":     settingsPathSuffix,
	"dashboardPathSuffix":    dashboardPathSuffix,
	"breakdownPathSegment":   breakdownPathSegment,
	"addressListPathSegment": addressListPathSegment,
	"DevAccessRequestsPath":  DevAccessRequestsPath,
	"MailPath":               MailPath,
	"HealthPath":             HealthPath,
	"ClaimPathPrefix":        ClaimPathPrefix,
	"WelcomePathPrefix":      WelcomePathPrefix,
	"TechnicalDoorPath":      TechnicalDoorPath,
}

// TestEveryRouteIsEitherGuardedOrDeliberatelyNot.
//
// The test above proves every guarded route refuses an untokened POST.
// It cannot see the failure that matters more: a route that changes
// something and never reaches acceptPost at all. Such a handler passes
// every CSRF test there is, by not being in any of them.
//
// So both sides are written down. One is read from the source - which
// handlers call acceptPost - and the other is this list of routes that
// deliberately do not, each with the reason. A new route joins one side
// or the other, and a handler that quietly stops calling acceptPost
// moves between them; either way this fails.
//
// The list is the half that can be wrong on purpose, which is why every
// entry carries why. A reason that stops being true is a bug somebody
// can see; an unexplained name is one nobody can.
func TestEveryRouteIsEitherGuardedOrDeliberatelyNot(t *testing.T) {
	// Routes with no CSRF gate, and why each is safe without one.
	unguarded := map[string]string{
		"devAccessHandler": "redeems a one-time developer link, which is a URL somebody " +
			"clicks from a terminal or a message. The token is the authorization and it " +
			"is consumed on use; requiring a form post first would mean a link that " +
			"cannot be clicked. Every failure renders the same page, so it confirms " +
			"nothing to somebody guessing.",
		"totpQRHandler":      "renders the enrolment QR for the signed-in account. Read-only.",
		"dashboardHandler":   "read-only.",
		"detailHandler":      "read-only.",
		"addressListHandler": "read-only.",
		"home":               "read-only.",
	}

	guarded := handlersCallingAcceptPost(t)

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "server.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing server.go: %v", err)
	}

	var registered []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); !ok || sel.Sel.Name != "HandleFunc" {
			return true
		}
		if handler, ok := call.Args[1].(*ast.SelectorExpr); ok {
			registered = append(registered, handler.Sel.Name)
		}
		return true
	})
	if len(registered) == 0 {
		t.Fatal("no routes were found in server.go; this test would pass by comparing nothing")
	}

	for _, name := range registered {
		_, exempted := unguarded[name]
		switch {
		case guarded[name] && exempted:
			t.Errorf("%s calls acceptPost and is also listed as deliberately unguarded. "+
				"One of the two is out of date, and the list is the half that can be wrong", name)
		case !guarded[name] && !exempted:
			t.Errorf("%s is registered as a route, never calls acceptPost, and is not listed "+
				"as deliberately unguarded.\n"+
				"If it changes anything, it is doing so without a CSRF check and no test above "+
				"can see that - a handler outside every list passes every list. If it is "+
				"read-only, add it to `unguarded` with the reason.", name)
		}
	}

	// And the list does not outlive the routes it describes.
	for name := range unguarded {
		found := false
		for _, r := range registered {
			if r == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is listed as deliberately unguarded but is no longer registered; "+
				"a stale exemption is how a future handler of the same name inherits a "+
				"decision nobody made about it", name)
		}
	}
}
