//go:build integration

package web

import (
	"net/http"
	"net/http/httptest"
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
func TestEveryPostRouteRefusesARequestWithNoToken(t *testing.T) {
	srv, _ := setupTestServer(t)
	h := srv.Handler()

	routes := []string{
		LoginPath,
		SecondFactorPath,
		LogoutPath,
		AccountPath,
		TechnicalDoorPath,
		MembersPathPrefix + "bir-site" + membersPathSuffix,
		ClaimPathPrefix + "bir-jeton",
		WelcomePathPrefix + welcomeSteps[0].ID,
		SetupPathPrefix + wizardSteps[0].ID,
		DevAccessPathPrefix + "bir-jeton",
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
