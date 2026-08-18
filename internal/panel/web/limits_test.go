package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"testing"
)

// TestOversizedBodiesAreRefusedBeforeTheyAreRead is the regression test
// for an unauthenticated memory exhaustion this panel shipped with for
// three phases.
//
// Go's ParseForm reads an entire urlencoded body into memory with no cap
// - only multipart bodies get one, through ParseMultipartForm. So every
// POST here was an unbounded allocation reachable by anybody, before
// authentication. A measured 64 MiB body cost about 128 MiB of heap and
// was *then* rejected for a missing CSRF token, having already been paid
// for; a handful of concurrent ones takes the process out.
//
// CWE-770. The reason it survived review is that nothing in any handler
// looks wrong - the missing thing is a line nobody wrote.
func TestOversizedBodiesAreRefusedBeforeTheyAreRead(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	post := func(size int) (int, uint64) {
		t.Helper()
		// Streamed, not built. strings.Repeat would allocate the body in
		// the test before the request was made, and the measurement would
		// be of this function rather than of the server. Streaming is also
		// what the attack looks like: a sender who never stops.
		body := io.MultiReader(
			strings.NewReader("parola="),
			io.LimitReader(endlessBytes{}, int64(size)),
		)

		var before, after runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)

		req := httptest.NewRequest(http.MethodPost, LoginPath, body)
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		runtime.ReadMemStats(&after)
		return rec.Code, after.TotalAlloc - before.TotalAlloc
	}

	// The size is reported as a size. Before parsing moved ahead of the
	// CSRF check this came back 419 - "your token is stale" - which sends
	// whoever is debugging to reload a page that fails again the same
	// way.
	status, _ := post(8 * maxFormBytes)
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413: a well-formed request with too much in it is "+
			"a different thing to fix than a malformed one", status)
	}

	// The property that matters is not a byte count - it is that the
	// count stops depending on the body. A fixed ceiling would be a test
	// about this machine's allocator; this is a test about the cap.
	_, small := post(2 * maxFormBytes)
	_, large := post(64 * maxFormBytes)
	if large > small*4 {
		t.Errorf("a body 32x larger allocated %d bytes against %d - the body is being "+
			"read before it is refused, so cost still scales with what a caller sends",
			large, small)
	}
}

// TestOrdinaryFormsStillWork guards the other direction. A limit set so
// low that real submissions fail is a denial of service somebody
// deployed on purpose.
func TestOrdinaryFormsStillWork(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	// The largest form in the panel is the wizard's site list. A hundred
	// hostnames is far beyond any real deployment and nowhere near the
	// cap.
	sites := make([]string, 100)
	for i := range sites {
		sites[i] = "site-" + strings.Repeat("x", 40)
	}
	form := url.Values{
		"csrf_token": {"whatever"},
		"siteler":    {strings.Join(sites, "\n")},
	}
	req := httptest.NewRequest(http.MethodPost, LoginPath, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code == http.StatusRequestEntityTooLarge {
		t.Errorf("a %d-byte form was refused as too large; the cap is too low",
			len(form.Encode()))
	}
}

// TestEveryFormHandlerGoesThroughTheCappedParser.
//
// The middleware caps the body, and parseForm turns an exceeded cap into
// 413 rather than 400. A handler calling r.ParseForm directly still gets
// the cap - the middleware is not optional - but reports it as a bad
// request, which sends whoever is debugging to look at their field
// names. Asserted on the source because the alternative is remembering,
// and the eight call sites here were written across three phases by
// somebody thinking about something else each time.
func TestEveryFormHandlerGoesThroughTheCappedParser(t *testing.T) {
	files, err := packageSources()
	if err != nil {
		t.Fatal(err)
	}
	for name, src := range files {
		if name == "limits.go" {
			continue // where ParseForm is legitimately called
		}
		if strings.Contains(src, "r.ParseForm()") {
			t.Errorf("%s calls r.ParseForm directly; use s.parseForm so an oversized "+
				"body is reported as 413 rather than 400", name)
		}
	}
}

// packageSources reads this package's non-test Go files, so a rule about
// how handlers are written can be asserted rather than remembered.
func packageSources() (map[string]string, error) {
	entries, err := os.ReadDir(".")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		out[name] = string(src)
	}
	return out, nil
}

// endlessBytes is a body that never ends, which is what a caller trying
// to exhaust this process actually sends.
type endlessBytes struct{}

func (endlessBytes) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'A'
	}
	return len(p), nil
}
