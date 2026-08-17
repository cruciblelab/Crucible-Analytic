package ui

import (
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestRenderer(t *testing.T) *Renderer {
	t.Helper()
	cats, err := LoadCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	assets, err := LoadAssets()
	if err != nil {
		t.Fatal(err)
	}
	r, err := New(cats, assets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRenderProducesAWholeDocument(t *testing.T) {
	r := newTestRenderer(t)
	r.Version = "test-1"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Render(rec, req, http.StatusOK, "giris", &Page{
		Title:   "Giriş",
		Heading: "Giriş",
	})

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"<!doctype html>",
		`<html lang="tr" dir="ltr">`,
		"<title>Giriş · Crucible Analytic</title>",
		`name="robots"`,
		"</html>",
		"Devam etmek için giriş yapın.",
		"test-1",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page is missing %q", want)
		}
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got, _ := strconv.Atoi(rec.Header().Get("Content-Length")); got != rec.Body.Len() {
		t.Errorf("Content-Length %d, body %d", got, rec.Body.Len())
	}
}

// TestRenderedPagesAreNeverCached: panel HTML carries the customer's
// numbers and a session's CSRF token. A shared cache holding either is
// how one customer sees another's page.
func TestRenderedPagesAreNeverCached(t *testing.T) {
	r := newTestRenderer(t)
	rec := httptest.NewRecorder()
	r.Render(rec, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, "giris", &Page{})
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control = %q", got)
	}
}

func TestRenderEscapesEverythingItIsGiven(t *testing.T) {
	r := newTestRenderer(t)
	const attack = `<script>alert(1)</script>`
	rec := httptest.NewRecorder()
	r.Render(rec, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, "giris", &Page{
		Title:   attack,
		Heading: attack,
		Site:    SiteView{Name: attack},
		User:    UserView{Label: attack},
		Notices: []Notice{{
			Level:       NoticeWarn,
			Title:       attack,
			Body:        attack,
			ActionURL:   "javascript:alert(1)",
			ActionLabel: attack,
		}},
	})
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("a script tag survived into the page")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("the payload does not appear escaped either; check the assertion")
	}
	if strings.Contains(body, `href="javascript:alert(1)"`) {
		t.Fatal("a javascript: URL survived into an href")
	}
}

func TestChromeSaysWhoIsLooking(t *testing.T) {
	r := newTestRenderer(t)
	rec := httptest.NewRecorder()
	r.Render(rec, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, "giris", &Page{
		User: UserView{Label: "operator@example.com", Operator: true, ReadOnly: true},
		Site: SiteView{ID: "ornek", Name: "Örnek Site"},
		Nav: []NavItem{
			{Label: "Panel", URL: "/", Current: true},
			{Label: "Ayarlar", URL: "/ayarlar"},
		},
	})
	body := rec.Body.String()
	for _, want := range []string{
		"İşletmeci olarak görüyorsunuz",
		"Bu hesap yalnızca görüntüleme yetkisine sahip",
		"Örnek Site",
		`aria-current="page"`,
		`href="/ayarlar"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("chrome is missing %q", want)
		}
	}
}

func TestChromeStaysQuietWhenThereIsNothingToSay(t *testing.T) {
	r := newTestRenderer(t)
	rec := httptest.NewRecorder()
	r.Render(rec, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, "giris", &Page{})
	body := rec.Body.String()
	for _, unwanted := range []string{
		"İşletmeci olarak görüyorsunuz",
		"yalnızca görüntüleme",
		"<nav",
	} {
		if strings.Contains(body, unwanted) {
			t.Errorf("an empty page still rendered %q", unwanted)
		}
	}
}

func TestRenderFillsInAFormatterWhenTheHandlerDidNot(t *testing.T) {
	r := newTestRenderer(t)
	r.SetZone(time.FixedZone("+03", 3*60*60))
	rec := httptest.NewRecorder()
	// Page.F is nil: a template calling .F would panic without the
	// default, and the whole page would become a 500.
	r.Render(rec, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, "giris", &Page{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	// Unescape first: html/template writes "+" as "&#43;", so the
	// assertion has to be about what the reader sees rather than about
	// the bytes.
	if !strings.Contains(html.UnescapeString(rec.Body.String()), "+03") {
		t.Error("the footer does not name the zone the page is rendered in")
	}
}

func TestErrorPagesSayWhatHappenedInTurkish(t *testing.T) {
	r := newTestRenderer(t)
	cases := map[int]string{
		http.StatusBadRequest:          "İstek anlaşılamadı",
		http.StatusForbidden:           "Bu sayfaya erişiminiz yok",
		http.StatusNotFound:            "Sayfa bulunamadı",
		http.StatusMethodNotAllowed:    "Bu adres bu şekilde çağrılamaz",
		statusCSRFExpired:              "Form zaman aşımına uğradı",
		http.StatusInternalServerError: "Panel bu isteği tamamlayamadı",
		http.StatusBadGateway:          "Veri kaynağına şu an ulaşılamıyor",
		http.StatusServiceUnavailable:  "Panel şu an hazır değil",
	}
	for status, want := range cases {
		rec := httptest.NewRecorder()
		r.Error(rec, httptest.NewRequest(http.MethodGet, "/yok", nil), status)
		if rec.Code != status {
			t.Errorf("Error(%d) responded %d", status, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, want) {
			t.Errorf("Error(%d) does not say %q", status, want)
		}
		if strings.Contains(body, markPrefix) {
			t.Errorf("Error(%d) rendered a missing-key marker: %s", status, body)
		}
		if !strings.Contains(body, "Panele dön") {
			t.Errorf("Error(%d) leaves the reader with no way back", status)
		}
	}
}

func TestUnmappedStatusStillGetsAWrittenPage(t *testing.T) {
	r := newTestRenderer(t)
	rec := httptest.NewRecorder()
	r.Error(rec, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusTeapot)
	if rec.Code != http.StatusTeapot {
		t.Errorf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), markPrefix) {
		t.Fatal("an unmapped status produced a missing-key marker")
	}
	if !strings.Contains(rec.Body.String(), "Panel bu isteği tamamlayamadı") {
		t.Error("an unmapped status did not fall back to the 500 wording")
	}
}

func TestOutOfRangeStatusBecomes500(t *testing.T) {
	r := newTestRenderer(t)
	for _, status := range []int{0, 200, 302, 999} {
		rec := httptest.NewRecorder()
		r.Error(rec, httptest.NewRequest(http.MethodGet, "/", nil), status)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("Error(%d) responded %d, want 500", status, rec.Code)
		}
	}
}

func TestErrorReferenceReachesThePage(t *testing.T) {
	r := newTestRenderer(t)
	rec := httptest.NewRecorder()
	r.ErrorRef(rec, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusInternalServerError, "ref-abc123")
	if !strings.Contains(rec.Body.String(), "ref-abc123") {
		t.Fatal("the reference is not on the page, so a support call cannot start with it")
	}
}

// TestHtmxErrorsAreFragments: swapping a whole document into a div
// produces a page that looks broken in a way nobody can diagnose from a
// screenshot.
func TestHtmxErrorsAreFragments(t *testing.T) {
	r := newTestRenderer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	r.Error(rec, req, http.StatusForbidden)
	body := rec.Body.String()
	if strings.Contains(body, "<html") || strings.Contains(body, "<head") {
		t.Fatalf("an htmx request got a whole document: %s", body)
	}
	if !strings.Contains(body, "Bu sayfaya erişiminiz yok") {
		t.Errorf("the fragment does not carry the message: %s", body)
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status %d", rec.Code)
	}

	// A boosted link swaps the whole body, so it wants the full page.
	req.Header.Set("HX-Boosted", "true")
	rec = httptest.NewRecorder()
	r.Error(rec, req, http.StatusForbidden)
	if !strings.Contains(rec.Body.String(), "<html") {
		t.Error("a boosted request got a fragment")
	}
}

// TestUnknownPageFallsBackRatherThanPanics: the page name is a constant
// everywhere in this codebase, so reaching this is a programming error
// - and a programming error must not take the process down while
// serving traffic.
func TestUnknownPageFallsBackRatherThanPanics(t *testing.T) {
	r := newTestRenderer(t)
	rec := httptest.NewRecorder()
	r.Render(rec, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, "boyle-bir-sayfa-yok", &Page{})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Panel bu isteği tamamlayamadı") {
		t.Error("the fallback page is not the pre-rendered error page")
	}
}

// TestRenderFailureDoesNotSend200WithHalfAPage is the reason the
// renderer buffers. Writing straight to the ResponseWriter would send
// the status and everything up to the failure, and the reader would get
// a document that stops mid-sentence under a 200.
func TestRenderFailureDoesNotSend200WithHalfAPage(t *testing.T) {
	r := newTestRenderer(t)
	rec := httptest.NewRecorder()
	// A Data value the error template will fail on: it reaches for
	// fields this type does not have.
	r.Render(rec, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, "hata", &Page{
		Data: struct{ Yanlis string }{"veri"},
	})
	if rec.Code == http.StatusOK {
		t.Fatal("a failed render responded 200")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(strings.TrimSpace(body), "<!doctype html>") || !strings.Contains(body, "</html>") {
		t.Fatal("the response is a partial document")
	}
}

func TestHeadRequestsGetHeadersWithoutABody(t *testing.T) {
	r := newTestRenderer(t)
	rec := httptest.NewRecorder()
	r.Render(rec, httptest.NewRequest(http.MethodHead, "/", nil), http.StatusOK, "giris", &Page{})
	if rec.Body.Len() != 0 {
		t.Error("HEAD returned a body")
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Error("HEAD did not report a length")
	}
}

func TestSecurityHeadersAreSetOnEverything(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	rec := httptest.NewRecorder()
	SecurityHeaders(false, next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	for header, want := range map[string]string{
		"Content-Security-Policy":    contentSecurityPolicy,
		"X-Content-Type-Options":     "nosniff",
		"X-Frame-Options":            "DENY",
		"Referrer-Policy":            "same-origin",
		"Cross-Origin-Opener-Policy": "same-origin",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS was sent from a deployment that may only speak HTTP")
	}

	rec = httptest.NewRecorder()
	SecurityHeaders(true, next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("Strict-Transport-Security") == "" {
		t.Error("HSTS was not sent when it was asked for")
	}
}

func TestNotFoundHandler(t *testing.T) {
	r := newTestRenderer(t)
	rec := httptest.NewRecorder()
	r.NotFound().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hicbiryer", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Sayfa bulunamadı") {
		t.Error("the 404 page is not the written one")
	}
}

func TestNewRefusesToStartWithoutItsParts(t *testing.T) {
	if _, err := New(nil, nil, nil); err == nil {
		t.Fatal("New accepted nil language packs and nil assets")
	}
}

func TestPagesAreParsedSeparately(t *testing.T) {
	r := newTestRenderer(t)
	pages := r.Pages()
	if len(pages) < 2 {
		t.Fatalf("only %v parsed", pages)
	}
	// Each page defines "icerik". If they shared one template set the
	// last one parsed would win and every page would render the same
	// body.
	first := httptest.NewRecorder()
	r.Render(first, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusOK, "giris", &Page{})
	second := httptest.NewRecorder()
	r.Error(second, httptest.NewRequest(http.MethodGet, "/", nil), http.StatusNotFound)
	if strings.Contains(first.Body.String(), "Sayfa bulunamadı") {
		t.Error("the login page rendered the error page's body")
	}
	if strings.Contains(second.Body.String(), "Devam etmek için giriş yapın") {
		t.Error("the error page rendered the login page's body")
	}
}
