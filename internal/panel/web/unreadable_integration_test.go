//go:build integration

package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
)

// TestAPageThatSaysOkunamadiAlsoSaysWhyInTheLog.
//
// The structural tests hold that the sentence can only come from the one
// function. This holds that the function is actually reached, against a
// read API that is not there - which is the shape of every real
// occurrence: a timeout, a refused connection, a service that has not
// started.
//
// # What it is really asserting
//
// Not "a log line exists". That a customer's report becomes something an
// operator can act on: the section, the site, the range and the cause,
// in one line, at the moment the page drew the sentence.
//
// The symptom this comes from was seen twice in a screenshot and was
// undiagnosable both times, because the panel turned the error into
// Turkish and dropped it. Two days sat in the notes as "watch this".
func TestAPageThatSaysOkunamadiAlsoSaysWhyInTheLog(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "log-okunamadi"

	// An API that answers nothing: a listener that is closed, so every
	// call fails at connect. Closer to a stopped service than a handler
	// returning 500, and it exercises the transport error rather than a
	// status code.
	dead := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {}))
	base := dead.URL
	dead.Close()

	client, err := analytics.New(base, "irrelevant-token")
	if err != nil {
		t.Fatal(err)
	}
	srv.Analytics = client

	var logged bytes.Buffer
	srv.Logger = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{
		Level: slog.LevelWarn,
	}))

	owner := makeUser(t, store, "log-okunamadi-sahip", false)
	if err := store.AddMember(ctx, site, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	browser := newClient(t, server.URL)
	signIn(t, browser, server.URL, owner.Email, testAccountPassword).Body.Close()

	resp, err := browser.Get(server.URL + sitePath(site))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)

	// The page has to have said it, or this test is about a page that
	// worked.
	if !strings.Contains(body, "Okunamad") {
		t.Fatalf("the dashboard did not report an unreadable source, so there is "+
			"nothing for a log line to accompany. Status %d", resp.StatusCode)
	}

	line := logged.String()
	if line == "" {
		t.Fatal("the page told the reader a number could not be read and wrote " +
			"nothing to the log. That is the defect: a customer reports a panel " +
			"that will not draw, and the operator has no line, no error and no " +
			"idea which call it was")
	}
	for _, want := range []struct{ text, why string }{
		{"a section could not be read", "the line itself"},
		{"site=" + site, "which site"},
		{"where=", "which section"},
		{"span=", "how long a range was asked for - the field that separates " +
			"a service that is down from a query that is too slow"},
		{"err=", "the cause"},
	} {
		if !strings.Contains(line, want.text) {
			t.Errorf("the log does not contain %q (%s).\nWhat it wrote:\n%s",
				want.text, want.why, line)
		}
	}
	t.Logf("what an operator now gets:\n%s", strings.TrimSpace(line))
}
