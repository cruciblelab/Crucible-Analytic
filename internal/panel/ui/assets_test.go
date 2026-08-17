package ui

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// htmxSHA256 pins the vendored copy of htmx. See static/VENDOR.md.
//
// This is the one file in the repository nobody reads during review, so
// an accidental edit, a bad merge, or a "patched" build pasted in by
// somebody helpful would otherwise land unnoticed and ship to every
// customer. Updating htmx means changing this constant deliberately, in
// the same commit as the file.
const htmxSHA256 = "71ea67185bfa8c98c39d31717c6fce5d852370fcdfd129db4543774d3145c0de"

func TestVendoredHTMXMatchesItsRecordedHash(t *testing.T) {
	body, err := staticFS.ReadFile("static/htmx.min.js")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != htmxSHA256 {
		t.Fatalf("htmx.min.js hash is %s, recorded %s.\n"+
			"If you updated htmx on purpose, update static/VENDOR.md and this constant together.", got, htmxSHA256)
	}
}

func TestAssetURLsCarryAContentHash(t *testing.T) {
	a, err := LoadAssets()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range a.Names() {
		url := a.URL(name)
		if !strings.HasPrefix(url, AssetPrefix) {
			t.Errorf("%s serves from %q", name, url)
		}
		if url == AssetPrefix+name {
			t.Errorf("%s has no hash in its URL, so a year of caching would serve a stale file", name)
		}
	}
	if a.URL("yok.css") != "" {
		t.Error("an unknown asset returned a URL")
	}
}

func TestEveryExpectedAssetIsEmbedded(t *testing.T) {
	a, err := LoadAssets()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"panel.css", "htmx.min.js"} {
		if a.URL(want) == "" {
			t.Errorf("%s is not embedded; the panel would render unstyled or without htmx", want)
		}
	}
	// VENDOR.md lives in the same directory and must not be served.
	for _, name := range a.Names() {
		if strings.HasSuffix(name, ".md") {
			t.Errorf("%s is being served; only the closed content-type list should reach a browser", name)
		}
	}
}

func TestAssetHandlerCachesForeverAndRevalidates(t *testing.T) {
	a, err := LoadAssets()
	if err != nil {
		t.Fatal(err)
	}
	url := a.URL("panel.css")
	h := a.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", url, rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/css; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q", got)
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}

	// A conditional request must be answered 304 with no body.
	for _, header := range []string{etag, "W/" + etag, "\"other\", " + etag, "*"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("If-None-Match", header)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotModified {
			t.Errorf("If-None-Match %q = %d, want 304", header, rec.Code)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("If-None-Match %q returned a body", header)
		}
	}

	// A stale validator must get the file.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("If-None-Match", `"eskimis"`)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("stale If-None-Match = %d, want 200", rec.Code)
	}
}

func TestAssetHandlerNegotiatesGzip(t *testing.T) {
	a, err := LoadAssets()
	if err != nil {
		t.Fatal(err)
	}
	url := a.URL("htmx.min.js")
	h := a.Handler()

	plain := httptest.NewRecorder()
	h.ServeHTTP(plain, httptest.NewRequest(http.MethodGet, url, nil))
	if plain.Header().Get("Content-Encoding") != "" {
		t.Error("served gzip to a client that did not ask")
	}
	// Vary must be present even without gzip, or a shared cache stores
	// the plain body and serves it to everyone.
	if got := plain.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q on the uncompressed response", got)
	}

	packed := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	h.ServeHTTP(packed, req)
	if got := packed.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q", got)
	}
	if packed.Body.Len() >= plain.Body.Len() {
		t.Errorf("gzip body (%d) is not smaller than plain (%d)", packed.Body.Len(), plain.Body.Len())
	}
	// Content-Length must describe what was actually sent.
	if got, _ := strconv.Atoi(packed.Header().Get("Content-Length")); got != packed.Body.Len() {
		t.Errorf("Content-Length %d, body %d", got, packed.Body.Len())
	}

	zr, err := gzip.NewReader(packed.Body)
	if err != nil {
		t.Fatalf("the gzip body does not decompress: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != plain.Body.String() {
		t.Error("the compressed and plain bodies differ")
	}

	// A client refusing gzip gets the plain body.
	refused := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Accept-Encoding", "gzip;q=0")
	h.ServeHTTP(refused, req)
	if refused.Header().Get("Content-Encoding") != "" {
		t.Error("served gzip to a client that set q=0")
	}
}

func TestAssetHandlerRefusesUnknownPaths(t *testing.T) {
	a, err := LoadAssets()
	if err != nil {
		t.Fatal(err)
	}
	h := a.Handler()
	for _, path := range []string{
		AssetPrefix + "panel.css",   // unhashed: not a real URL
		AssetPrefix + "../go.mod",   // traversal
		AssetPrefix + "VENDOR.md",   // present on disk, not servable
		AssetPrefix + "yok.abc.css", // wrong hash
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "<html") {
			t.Errorf("GET %s answered a subresource request with a full HTML page", path)
		}
	}
}

func TestAssetHandlerAnswersHEADWithoutABody(t *testing.T) {
	a, err := LoadAssets()
	if err != nil {
		t.Fatal(err)
	}
	url := a.URL("panel.css")
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodHead, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Error("HEAD returned a body")
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Error("HEAD did not report a length")
	}
}
