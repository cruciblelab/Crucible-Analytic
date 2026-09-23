package web

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const linkSecret = "Sirr1Jeton7f3a9c"

// The expected outputs are written out, not built from the prefix list: a
// test that computed them from tokenPathPrefixes would pass on whatever
// the list happened to say.
func TestLoggedPathHidesTheTokenInEveryLinkRoute(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"/katil/" + linkSecret, "/katil/[redacted]"},
		{"/sahiplen/" + linkSecret, "/sahiplen/[redacted]"},
		{"/gelistirici/" + linkSecret, "/gelistirici/[redacted]"},
		// Sent unclean, redirected by the mux to the invitation - and
		// logged before the mux sees it.
		{"/./katil/" + linkSecret, "/katil/[redacted]"},
		{"/katil/" + linkSecret + "/", "/katil/[redacted]"},
		// Nothing to hide.
		{"/katil/", "/katil/"},
		{"/katil", "/katil"},
		{"/site/acme/pano", "/site/acme/pano"},
		// A prefix is a path segment, not a string prefix.
		{"/katilmak/" + linkSecret, "/katilmak/" + linkSecret},
	} {
		if got := loggedPath(c.in); got != c.want {
			t.Errorf("loggedPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The access log itself, not the helper: what requestLog writes for a
// request to each link route.
func TestTheAccessLogNeverCarriesALinkToken(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{Logger: slog.New(slog.NewJSONHandler(&buf, nil))}
	h := s.requestLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	for _, route := range []string{"/katil/", "/sahiplen/", "/gelistirici/"} {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, route+linkSecret, nil))
	}
	out := buf.String()
	if strings.Contains(out, linkSecret) {
		t.Errorf("the access log carries a link token:\n%s", out)
	}
	if n := strings.Count(out, "[redacted]"); n != 3 {
		t.Errorf("%d redacted paths in the access log, want 3 - one per route:\n%s", n, out)
	}
}

// The deadline's line goes through the same rule. Asked of the answer the
// panel actually wires, not of a copy.
func TestTheDeadlineLineCarriesNoLinkToken(t *testing.T) {
	a := newTestServer(t).timeoutAnswer()
	if a.LogPath == nil {
		t.Fatal("the panel's deadline answer has no LogPath - its WARN line writes the " +
			"path as sent, and a timed-out invitation puts its token into panel_logs")
	}
	if got := a.LogPath("/katil/" + linkSecret); strings.Contains(got, linkSecret) {
		t.Errorf("the deadline line would log %q", got)
	}
}
