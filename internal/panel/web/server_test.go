package web

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	cats, err := ui.LoadCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	assets, err := ui.LoadAssets()
	if err != nil {
		t.Fatal(err)
	}
	r, err := ui.New(cats, assets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{
		Renderer: r,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Zone:     time.UTC,
		Language: "tr",
	}
}

func TestHomeServesTheLoginPage(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Devam etmek için giriş yapın") {
		t.Error("the login page did not render")
	}
}

// TestWrongMethodIs405NotFound: "this page does not exist" and "you
// called it wrong" send whoever is debugging in different directions.
func TestWrongMethodIs405NotFound(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST / = %d, want 405", rec.Code)
	}
	if got := rec.Header().Get("Allow"); got == "" {
		t.Error("405 without an Allow header leaves the caller guessing")
	}
	if !strings.Contains(rec.Body.String(), "Bu adres bu şekilde çağrılamaz") {
		t.Error("the 405 page is not the written one")
	}
}

func TestUnknownPathIsTheWritten404(t *testing.T) {
	s := newTestServer(t)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/boyle-bir-sayfa-yok", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Sayfa bulunamadı") {
		t.Error("the 404 page is not the written one")
	}
}

func TestAssetsAreServedFromTheBinary(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	for _, name := range []string{"panel.css", "htmx.min.js"} {
		url := s.Renderer.Assets().URL(name)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s = %d", url, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("GET %s returned nothing", url)
		}
		if !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
			t.Errorf("GET %s is not cached", url)
		}
	}
}

// TestEveryResponseCarriesTheSecurityHeaders checks the middleware
// wraps the whole tree rather than one branch of it. Assets are the
// easy one to leave out.
func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()
	paths := []string{"/", "/yok", s.Renderer.Assets().URL("panel.css")}
	for _, path := range paths {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s has no CSP", path)
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s is missing nosniff", path)
		}
	}
}

// TestPagesAreNotCachedButAssetsAre is the pair of rules that must not
// be collapsed into one: a page carries the customer's data and a CSRF
// token, an asset carries a hash in its URL.
func TestPagesAreNotCachedButAssetsAre(t *testing.T) {
	s := newTestServer(t)
	h := s.Handler()

	page := httptest.NewRecorder()
	h.ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(page.Header().Get("Cache-Control"), "no-store") {
		t.Errorf("page Cache-Control = %q", page.Header().Get("Cache-Control"))
	}

	asset := httptest.NewRecorder()
	h.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, s.Renderer.Assets().URL("panel.css"), nil))
	if strings.Contains(asset.Header().Get("Cache-Control"), "no-store") {
		t.Errorf("asset Cache-Control = %q", asset.Header().Get("Cache-Control"))
	}
}

func TestAccessLogRecordsWhatWasSentAndNotTheQueryString(t *testing.T) {
	var out strings.Builder
	s := newTestServer(t)
	s.Logger = slog.New(slog.NewTextHandler(&out, nil))

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/yok?token=gizli-deger", nil))

	line := out.String()
	if !strings.Contains(line, "status=404") {
		t.Errorf("the access log does not record the status that was sent: %s", line)
	}
	if strings.Contains(line, "gizli-deger") {
		t.Errorf("the access log contains the query string, which is where a reset token would be: %s", line)
	}
}

// TestListenAndServeDrainsOnCancel checks the binary can be stopped by
// a signal without dropping an in-flight response, which is what
// separates a deploy from an outage.
func TestListenAndServeDrainsOnCancel(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	s := newTestServer(t)
	s.ListenAddr = addr

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.ListenAndServe(ctx) }()

	// Wait for the listener to come up rather than sleeping a fixed
	// time, which is flaky on a loaded machine.
	var resp *http.Response
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + addr + "/")
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("the server never accepted a connection: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "<!doctype html>") {
		t.Error("the served page is not a document")
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Error("a real response carried no CSP")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the server did not shut down")
	}
}

func TestPortAlreadyInUseIsReported(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	s := newTestServer(t)
	s.ListenAddr = ln.Addr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.ListenAndServe(ctx); err == nil {
		t.Fatal("binding an occupied port did not report an error")
	}
}

// TestTheBrowsersLanguageIsServedWhenTheDeploymentHasNoPreference is the
// case a Turkish-first product forgets: a colleague on the same
// deployment who does not read Turkish.
func TestTheBrowsersLanguageIsServedWhenTheDeploymentHasNoPreference(t *testing.T) {
	s := newTestServer(t)
	s.Language = "" // no configured default, so the browser decides

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Language", "en-GB,en;q=0.9")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `<html lang="en" dir="ltr">`) {
		t.Error("the page did not declare English")
	}
	if !strings.Contains(body, "Sign in to continue.") {
		t.Errorf("the page is not in English:\n%s", body)
	}
	if got := rec.Header().Get("Content-Language"); got != "en" {
		t.Errorf("Content-Language = %q", got)
	}
	// The body depends on the header, so a cache must be told.
	if got := rec.Header().Get("Vary"); !strings.Contains(got, "Accept-Language") {
		t.Errorf("Vary = %q", got)
	}
}

// TestTheConfiguredLanguageBeatsTheBrowser: the deployment's setting is
// a choice somebody made, and a browser's default list is not.
func TestTheConfiguredLanguageBeatsTheBrowser(t *testing.T) {
	s := newTestServer(t) // Language: "tr"
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Language", "en-GB,en;q=0.9")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "Devam etmek için giriş yapın") {
		t.Error("the configured language was overridden by the browser")
	}
}

// TestErrorPagesFollowTheNegotiatedLanguage. An error page is exactly
// where somebody needs to understand the words, so it is exactly where
// falling back to the wrong language is worst.
func TestErrorPagesFollowTheNegotiatedLanguage(t *testing.T) {
	s := newTestServer(t)
	s.Language = ""
	req := httptest.NewRequest(http.MethodGet, "/boyle-bir-sayfa-yok", nil)
	req.Header.Set("Accept-Language", "en")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Page not found") {
		t.Errorf("the 404 page is not in English:\n%s", rec.Body.String())
	}
}
