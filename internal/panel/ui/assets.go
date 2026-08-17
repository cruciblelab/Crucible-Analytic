package ui

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"sort"
	"strings"
)

//go:embed static
var staticFS embed.FS

// AssetPrefix is where the stylesheet and htmx are served from. It is a
// path segment no page will ever use for anything else, which is what
// lets the handler be mounted with one route and lets the CSP name
// 'self' and nothing more.
const AssetPrefix = "/varlik/"

// assetMaxAge is a year. Safe only because the URL contains a hash of
// the content: a changed file is a changed URL, so nothing cached can
// ever be stale. Without the hash this number would be a bug.
const assetMaxAge = "public, max-age=31536000, immutable"

// contentTypes is a closed set. An extension not listed here is not
// served at all, rather than served as application/octet-stream.
//
// The same rule as everywhere else in this codebase: the type a browser
// is told to treat bytes as is a security decision, so it comes from a
// list somebody wrote, never from the filename.
var contentTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".svg":   "image/svg+xml",
	".ico":   "image/x-icon",
	".png":   "image/png",
	".woff2": "font/woff2",
}

// compressible lists the types worth gzipping. Images and fonts are
// already compressed; spending CPU on them at startup would buy bytes
// that do not exist.
var compressible = map[string]bool{
	".css": true,
	".js":  true,
	".svg": true,
}

type asset struct {
	// name is the logical name templates ask for: "panel.css".
	name string
	// urlPath is what the browser fetches:
	// "/varlik/panel.9f86d081.css".
	urlPath     string
	contentType string
	etag        string
	body        []byte
	gzipped     []byte
}

// Assets is the embedded static file set, indexed both ways: by logical
// name for templates, by URL for the handler.
type Assets struct {
	byName map[string]*asset
	byURL  map[string]*asset
}

// LoadAssets reads the embedded static directory and computes the
// hashed URL for every file. It runs once at startup; if it fails the
// binary does not start, because a panel that cannot serve its own
// stylesheet is not a degraded panel, it is an unreadable one.
func LoadAssets() (*Assets, error) {
	a := &Assets{
		byName: make(map[string]*asset),
		byURL:  make(map[string]*asset),
	}
	err := fs.WalkDir(staticFS, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := strings.TrimPrefix(p, "static/")
		ext := strings.ToLower(path.Ext(name))
		ctype, known := contentTypes[ext]
		if !known {
			// Documentation lives in this directory too (VENDOR.md).
			// Skipping unknown types rather than failing keeps notes
			// next to the thing they describe; serving them would be
			// the mistake, and that is what the closed map prevents.
			return nil
		}
		body, err := staticFS.ReadFile(p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		digest := hex.EncodeToString(sum[:])[:16]
		entry := &asset{
			name:        name,
			urlPath:     AssetPrefix + hashedName(name, digest),
			contentType: ctype,
			etag:        `"` + digest + `"`,
			body:        body,
		}
		if compressible[ext] {
			packed, err := gzipBytes(body)
			if err != nil {
				return fmt.Errorf("ui: gzip %s: %w", name, err)
			}
			// Only keep it if it actually helped. A gzip stream larger
			// than the file it encodes is a real outcome for tiny
			// inputs, and shipping it would make the "optimised" path
			// the slow one.
			if len(packed) < len(body) {
				entry.gzipped = packed
			}
		}
		a.byName[name] = entry
		a.byURL[entry.urlPath] = entry
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ui: load assets: %w", err)
	}
	if len(a.byName) == 0 {
		return nil, fmt.Errorf("ui: no static assets embedded")
	}
	return a, nil
}

// hashedName puts the digest before the extension, so
// "panel.css" becomes "panel.9f86d081.css" and the file keeps an
// extension a browser and a proxy can both recognise.
func hashedName(name, digest string) string {
	ext := path.Ext(name)
	return strings.TrimSuffix(name, ext) + "." + digest + ext
}

func gzipBytes(body []byte) ([]byte, error) {
	var buf bytes.Buffer
	// Best compression, once, at startup. These files never change
	// while the process runs, so the CPU is spent a single time for
	// every request the binary will ever serve.
	w, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(body); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// URL returns the hashed path for a logical asset name. An unknown name
// returns "", which renders as a broken link - visible, unlike an empty
// src that silently loads the page itself.
func (a *Assets) URL(name string) string {
	entry, ok := a.byName[name]
	if !ok {
		return ""
	}
	return entry.urlPath
}

// Names lists the logical asset names, sorted.
func (a *Assets) Names() []string {
	names := make([]string, 0, len(a.byName))
	for name := range a.byName {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Handler serves the embedded assets. Mount it at AssetPrefix.
func (a *Assets) Handler() http.Handler {
	return http.HandlerFunc(a.serve)
}

func (a *Assets) serve(w http.ResponseWriter, r *http.Request) {
	entry, ok := a.byURL[r.URL.Path]
	if !ok {
		// A missing subresource is not a page. Rendering the styled 404
		// here would mean answering a request for a stylesheet with
		// HTML, which the browser would then try to parse as CSS.
		http.Error(w, "bulunamadı", http.StatusNotFound)
		return
	}
	h := w.Header()
	h.Set("Content-Type", entry.contentType)
	h.Set("Cache-Control", assetMaxAge)
	h.Set("ETag", entry.etag)
	h.Set("X-Content-Type-Options", "nosniff")

	if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, entry.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	body := entry.body
	if entry.gzipped != nil {
		// Vary goes on the response whether or not this client asked
		// for gzip: a shared cache that stored the plain body without
		// it would go on to serve that body to everyone.
		h.Set("Vary", "Accept-Encoding")
		if acceptsGzip(r.Header.Get("Accept-Encoding")) {
			h.Set("Content-Encoding", "gzip")
			body = entry.gzipped
		}
	}
	h.Set("Content-Length", fmt.Sprint(len(body)))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// etagMatches handles the list form of If-None-Match and the weak
// prefix, both of which real caches send.
func etagMatches(header, etag string) bool {
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
		if strings.TrimPrefix(candidate, "W/") == etag {
			return true
		}
	}
	return false
}

func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		name, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		if !strings.EqualFold(strings.TrimSpace(name), "gzip") {
			continue
		}
		// "gzip;q=0" means the client is refusing it, which is how a
		// debugging proxy asks for the plain body.
		if strings.Contains(strings.ReplaceAll(params, " ", ""), "q=0") &&
			!strings.Contains(strings.ReplaceAll(params, " ", ""), "q=0.") {
			return false
		}
		return true
	}
	return false
}
