package web

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestEveryStatusThisPanelAnswersHasItsOwnWords.
//
// The renderer maps a status to a pair of catalog keys and falls back to
// the 500 wording for anything it does not know. That fallback is right
// as a fallback - a status reaching a browser with no words at all would
// be worse - but it is silent, and silence is the problem: the page then
// tells the reader the fault is ours when it is not.
//
// This was not hypothetical. The security phase added 413 for a body
// over the cap, and the whole point of that work was to report a size
// problem *as* a size problem rather than as a stale CSRF token. The
// status line said 413 and the page still said "the panel could not
// complete this request" - the fix half-landed, and every test passed.
//
// So the statuses are read out of the handlers rather than listed here.
// A list would be a mirror, and the failure mode of a mirror is exactly
// the one above: somebody adds a status and does not know there is a
// second place to add it.
func TestEveryStatusThisPanelAnswersHasItsOwnWords(t *testing.T) {
	srv := newTestServer(t)
	base := srv.Renderer.Catalogs().Base()

	statuses, err := statusesAnsweredBy(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) < 5 {
		t.Fatalf("found only %d statuses in the handlers; the scan is not "+
			"reading them, so this test is not checking anything", len(statuses))
	}

	for status, where := range statuses {
		t.Run(http.StatusText(status)+" "+strconv.Itoa(status), func(t *testing.T) {
			// The 500 page is allowed to be the 500 page.
			if status == http.StatusInternalServerError {
				return
			}

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			rec := httptest.NewRecorder()
			srv.Renderer.Error(rec, req, status)

			body := rec.Body.String()
			if got := rec.Code; got != status {
				t.Fatalf("rendered %d for a %d error", got, status)
			}
			if strings.Contains(body, base.T("hata.500.baslik")) {
				t.Errorf("%s answers %d and the page shows the 500 wording, which "+
					"tells the reader the fault is ours\n"+
					"add [metin.hata.%d] to every catalog and the status to "+
					"errorKeys in the renderer",
					where, status, status)
			}
			if want := base.T("hata." + strconv.Itoa(status) + ".baslik"); !strings.Contains(body, want) {
				t.Errorf("%s answers %d but the page does not carry its title %q",
					where, status, want)
			}
		})
	}
}

// TestEveryLanguageCarriesTheErrorPages.
//
// The catalog's own completeness check already refuses a pack missing a
// key the base language has, so this is narrower: it asserts that an
// error page rendered in a non-base language is not the marked-up
// "missing key" form. A reader who meets an error page is having a bad
// day already; meeting it in a language they cannot read, or as a raw
// key, is the version of that day this project can prevent.
func TestEveryLanguageCarriesTheErrorPages(t *testing.T) {
	srv := newTestServer(t)
	statuses, err := statusesAnsweredBy(t)
	if err != nil {
		t.Fatal(err)
	}

	for _, lang := range srv.Renderer.Catalogs().Languages() {
		for status := range statuses {
			title := lang.T("hata." + strconv.Itoa(status) + ".baslik")
			body := lang.T("hata." + strconv.Itoa(status) + ".govde")
			for _, text := range []string{title, body} {
				// T marks a key no pack defines rather than returning "".
				// The marker is what is being looked for; its exact
				// characters are the renderer's business, so this checks
				// for the key itself showing through instead.
				if strings.Contains(text, "hata."+strconv.Itoa(status)) {
					t.Errorf("%s has no words for %d: %q", lang.Code, status, text)
				}
			}
		}
	}
}

// statusRef matches the status arguments the handlers pass to the error
// renderer: http.StatusForbidden, or a constant this package declares
// itself such as statusCSRFExpired.
var statusRef = regexp.MustCompile(`Error(?:In)?\(w, r, (?:lang, )?([A-Za-z.]+)`)

// statusesAnsweredBy reads the handlers and returns every status they
// hand the error renderer, mapped to the file it was found in.
//
// Names are resolved through net/http's own StatusText rather than a
// table written here, and an unresolvable name **fails** rather than
// being skipped: "we looked and found nothing" and "we could not look"
// are different facts, and only the first should let a suite go green.
func statusesAnsweredBy(t *testing.T) (map[int]string, error) {
	t.Helper()

	byName := map[string]int{
		// This package's own constant for a stale form. 419 is not in
		// the RFC, so StatusText cannot name it.
		"statusCSRFExpired": statusCSRFExpired,
	}
	for code := 100; code < 600; code++ {
		text := http.StatusText(code)
		if text == "" {
			continue
		}
		byName["http.Status"+strings.ReplaceAll(text, " ", "")] = code
	}

	files, err := packageSources()
	if err != nil {
		return nil, err
	}
	out := map[int]string{}
	for name, src := range files {
		for _, m := range statusRef.FindAllStringSubmatch(src, -1) {
			ref := m[1]
			code, ok := byName[ref]
			if !ok {
				t.Errorf("%s answers %s and this test cannot work out which status "+
					"that is, so it is not being checked; give it a name "+
					"StatusText produces, or add it to byName", name, ref)
				continue
			}
			if _, seen := out[code]; !seen {
				out[code] = name
			}
		}
	}
	return out, nil
}
