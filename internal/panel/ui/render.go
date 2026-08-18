package ui

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template/parse"
	"time"
)

//go:embed templates
var templateFS embed.FS

const (
	layoutFile  = "templates/layout.html"
	partialGlob = "templates/partials/*.html"
	pagesDir    = "templates/pages"
	pageFileExt = ".html"
)

// Renderer turns a page value into bytes.
//
// One *template.Template per page, each holding its own copy of the
// layout and the partials. Go's template namespace is flat, so a single
// tree would let two pages defining "icerik" silently overwrite each
// other - and the survivor would be whichever file the walker happened
// to read last.
type Renderer struct {
	// pages is indexed by language code and then by page name.
	//
	// One template set per language, because the func map binds t and
	// tf to a specific pack. Go's template namespace is also flat, so a
	// single tree per language would let two pages defining "icerik"
	// silently overwrite each other - and the survivor would be
	// whichever file the walker happened to read last.
	pages   map[string]map[string]*template.Template
	cats    *Catalogs
	assets  *Assets
	log     *slog.Logger
	fallbck map[string][]byte
	// Version is stamped into the footer of every page.
	Version string
	// defaultZone formats for a page whose handler did not set one.
	defaultZone *time.Location

	bufs sync.Pool
}

// New parses the templates, checks every catalog key they name, and
// pre-renders the last-resort error page.
//
// Everything that can fail, fails here - at startup, before a request
// exists. A template that does not parse, or names a key nobody wrote,
// stops the binary rather than producing a broken page under load.
func New(cats *Catalogs, assets *Assets, log *slog.Logger) (*Renderer, error) {
	if cats == nil || assets == nil {
		return nil, fmt.Errorf("ui: renderer needs language packs and assets")
	}
	if log == nil {
		log = slog.Default()
	}
	r := &Renderer{
		pages:       make(map[string]map[string]*template.Template),
		cats:        cats,
		assets:      assets,
		log:         log,
		fallbck:     make(map[string][]byte),
		defaultZone: time.UTC,
	}
	r.bufs.New = func() any { return new(bytes.Buffer) }

	for _, lang := range cats.Languages() {
		pages, trees, err := parsePages(r.funcMap(lang))
		if err != nil {
			return nil, err
		}
		r.pages[lang.Code] = pages
		// Only the base language is checked. A translation resolves
		// every key through the fallback, so checking it would only
		// ever repeat this answer.
		if lang == cats.Base() {
			if err := checkTemplateKeys(trees, lang); err != nil {
				return nil, err
			}
		}
		if err := r.buildFallback(lang); err != nil {
			return nil, err
		}
	}

	// An incomplete translation is reported, not fatal. Refusing to boot
	// would mean one untranslated sentence taking down deployments whose
	// readers do not speak that language at all; the tests are where an
	// incomplete pack is supposed to hurt.
	for code, missing := range cats.Gaps() {
		if len(missing) == 0 {
			continue
		}
		log.Warn("ui: language pack is incomplete, those lines fall back to the base language",
			"language", code, "base", BaseLanguageCode,
			"missing", len(missing), "keys", strings.Join(missing, ", "))
	}
	return r, nil
}

// SetZone makes loc the zone used for pages whose handler did not
// supply a formatter.
func (r *Renderer) SetZone(loc *time.Location) {
	if loc != nil {
		r.defaultZone = loc
	}
}

// Catalogs exposes the loaded language packs to handlers, which need
// them for titles, button labels and language negotiation.
func (r *Renderer) Catalogs() *Catalogs { return r.cats }

// Assets exposes the asset set, for the handler that serves them.
func (r *Renderer) Assets() *Assets { return r.assets }

// Pages lists the page names this renderer knows.
func (r *Renderer) Pages() []string {
	names := make([]string, 0)
	for name := range r.pages[BaseLanguageCode] {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// parsePages builds one template set per page and hands back the parse
// trees alongside them.
//
// The trees are returned rather than re-derived because two callers
// need them and must not disagree: the startup check that refuses
// unknown catalog keys, and the test that refuses catalog keys nothing
// uses. Parsing twice would let those two answer different questions
// about the same templates.
func parsePages(funcs template.FuncMap) (map[string]*template.Template, map[string]*parse.Tree, error) {
	names, err := pageNames()
	if err != nil {
		return nil, nil, err
	}
	pages := make(map[string]*template.Template, len(names))
	trees := make(map[string]*parse.Tree)
	for _, name := range names {
		tmpl, err := template.New(path.Base(layoutFile)).Funcs(funcs).ParseFS(
			templateFS, layoutFile, partialGlob, path.Join(pagesDir, name+pageFileExt))
		if err != nil {
			return nil, nil, fmt.Errorf("ui: parse page %q: %w", name, err)
		}
		for _, t := range tmpl.Templates() {
			if t.Tree != nil {
				trees[name+":"+t.Name()] = t.Tree
			}
		}
		pages[name] = tmpl
	}
	return pages, trees, nil
}

func pageNames() ([]string, error) {
	entries, err := fs.ReadDir(templateFS, pagesDir)
	if err != nil {
		return nil, fmt.Errorf("ui: read pages: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || path.Ext(e.Name()) != pageFileExt {
			continue
		}
		names = append(names, e.Name()[:len(e.Name())-len(pageFileExt)])
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("ui: no page templates embedded")
	}
	sort.Strings(names)
	return names, nil
}

func (r *Renderer) funcMap(lang *Language) template.FuncMap {
	return template.FuncMap{
		"t":     lang.T,
		"tf":    lang.Tf,
		"asset": r.assets.URL,
	}
}

// langOf resolves the language a page should render in, falling back to
// the base rather than to nothing.
func (r *Renderer) langOf(page *Page) *Language {
	if page != nil && page.L != nil {
		if _, known := r.pages[page.L.Code]; known {
			return page.L
		}
	}
	return r.cats.Base()
}

// Render writes a page.
//
// The template is executed into a buffer first and only copied to the
// response once it has succeeded. Executing straight into the
// ResponseWriter would send 200 and half a document before discovering
// a nil field on line forty - the reader gets a page that stops
// mid-sentence, and the status code says it worked.
func (r *Renderer) Render(w http.ResponseWriter, req *http.Request, status int, name string, page *Page) {
	lang := r.langOf(page)
	tmpl, ok := r.pages[lang.Code][name]
	if !ok {
		// A page name is a constant in this codebase, so this is a
		// programming error rather than bad input; it still must not
		// take the process down under load.
		r.log.Error("ui: unknown page template", "page", name, "language", lang.Code)
		r.writeFallback(w, lang, http.StatusInternalServerError)
		return
	}
	if page == nil {
		page = &Page{}
	}
	page.L = lang
	if page.F == nil {
		page.F = NewFormatter(lang, r.defaultZone)
	}
	if page.Version == "" {
		page.Version = r.Version
	}

	buf := r.bufs.Get().(*bytes.Buffer)
	buf.Reset()
	defer func() {
		// Do not hold on to a buffer that one huge page grew; it would
		// keep that memory for the life of the process.
		if buf.Cap() < 512*1024 {
			r.bufs.Put(buf)
		}
	}()

	if err := tmpl.Execute(buf, page); err != nil {
		r.log.Error("ui: render failed", "page", name, "language", lang.Code, "error", err)
		r.writeFallback(w, lang, http.StatusInternalServerError)
		return
	}

	noStore(w)
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Language", lang.Code)
	// The body depends on the header, so a cache that ignored this would
	// hand a Turkish page to an English reader. Pages are no-store
	// anyway; this is here so the two rules cannot drift apart when
	// something downstream decides to cache after all.
	h.Add("Vary", "Accept-Language")
	h.Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	if req != nil && req.Method == http.MethodHead {
		return
	}
	if _, err := buf.WriteTo(w); err != nil {
		// The client went away mid-write. Nothing to do but say so at a
		// level nobody pages on.
		r.log.Debug("ui: write to client failed", "page", name, "error", err)
	}
}

// Error renders the styled error page for a status code.
//
// Every status the panel returns to a browser comes through here, so
// there is exactly one place where a status becomes a sentence, and
// that sentence is written for the person reading it rather than
// derived from the code.
func (r *Renderer) Error(w http.ResponseWriter, req *http.Request, status int) {
	r.errorWith(w, req, status, "", nil)
}

// ErrorIn is Error in a specific language, for handlers that have
// already resolved one.
func (r *Renderer) ErrorIn(w http.ResponseWriter, req *http.Request, status int, lang *Language) {
	r.errorWith(w, req, status, "", lang)
}

// ErrorRef is Error with a reference the reader can quote and the
// operator can grep for.
func (r *Renderer) ErrorRef(w http.ResponseWriter, req *http.Request, status int, reference string) {
	r.errorWith(w, req, status, reference, nil)
}

func (r *Renderer) errorWith(w http.ResponseWriter, req *http.Request, status int, reference string, lang *Language) {
	if status < 400 || status > 599 {
		status = http.StatusInternalServerError
	}
	if lang == nil {
		lang = r.langOf(&Page{L: LanguageFrom(req)})
	}
	titleKey, bodyKey := errorKeys(status)

	if wantsFragment(req) {
		// An htmx swap wants a piece, not a document. Sending the whole
		// layout would drop <html> and <head> into whatever div asked.
		r.writeFragmentError(w, status, lang, lang.T(titleKey), lang.T(bodyKey))
		return
	}

	page := &Page{
		L:       lang,
		Title:   lang.T(titleKey),
		Heading: lang.T(titleKey),
		Data: errorPage{
			Status:    status,
			Body:      lang.T(bodyKey),
			At:        time.Now(),
			Reference: reference,
			BackURL:   "/",
			BackLabel: lang.T("hata.panele_don"),
		},
	}
	r.Render(w, req, status, "hata", page)
}

// errorKeys maps a status to its catalog entries, falling back to the
// 500 wording for anything without its own.
//
// The fallback is the 500 text and not a generic "error" string on
// purpose: an unmapped status reaching a browser is a gap in the
// handler, and the 500 wording is the one that tells the reader the
// fault is ours.
func errorKeys(status int) (titleKey, bodyKey string) {
	prefix := "hata." + strconv.Itoa(status)
	// Only families the catalog actually carries; see messages.tr.toml.
	switch status {
	case http.StatusBadRequest,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusMethodNotAllowed,
		http.StatusRequestEntityTooLarge,
		statusCSRFExpired,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable:
		return prefix + ".baslik", prefix + ".govde"
	}
	return "hata.500.baslik", "hata.500.govde"
}

// statusCSRFExpired is 419, which is not in the RFC but is what this
// panel answers when a form's CSRF token no longer matches. A distinct
// code because the fix is distinct: 403 says "you may not", and this
// says "reload and try again", which is the truth for a page left open
// over lunch.
const statusCSRFExpired = 419

// writeFragmentError renders the same two sentences the full page
// shows. The title goes in too: an htmx swap replaces one region, and
// the reader has no <h1> to fall back on for what went wrong.
func (r *Renderer) writeFragmentError(w http.ResponseWriter, status int, lang *Language, title, body string) {
	var buf bytes.Buffer
	buf.WriteString(`<div class="hata" role="alert"><strong class="uyari-baslik">`)
	template.HTMLEscape(&buf, []byte(title))
	buf.WriteString(`</strong><p>`)
	template.HTMLEscape(&buf, []byte(body))
	buf.WriteString(`</p></div>`)
	noStore(w)
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Language", lang.Code)
	h.Add("Vary", "Accept-Language")
	h.Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// buildFallback renders the 500 page once, at startup, into bytes.
//
// The error path must not depend on the thing that just failed. If
// executing a template is what went wrong, executing another one to say
// so can fail the same way, and the loop that follows is worse than the
// original bug. This copy is produced before any request exists and
// served verbatim afterwards.
func (r *Renderer) buildFallback(lang *Language) error {
	tmpl, ok := r.pages[lang.Code]["hata"]
	if !ok {
		return fmt.Errorf("ui: the error page template is missing")
	}
	page := &Page{
		L:       lang,
		Title:   lang.T("hata.500.baslik"),
		Heading: lang.T("hata.500.baslik"),
		F:       NewFormatter(lang, time.UTC),
		Data: errorPage{
			Status:    http.StatusInternalServerError,
			Body:      lang.T("hata.500.govde"),
			BackURL:   "/",
			BackLabel: lang.T("hata.panele_don"),
		},
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, page); err != nil {
		return fmt.Errorf("ui: pre-render the error page in %s: %w", lang.Code, err)
	}
	r.fallbck[lang.Code] = buf.Bytes()
	return nil
}

func (r *Renderer) writeFallback(w http.ResponseWriter, lang *Language, status int) {
	body, ok := r.fallbck[lang.Code]
	if !ok {
		body = r.fallbck[BaseLanguageCode]
	}
	noStore(w)
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Language", lang.Code)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// NotFound is an http.Handler for routes nothing claims.
func (r *Renderer) NotFound() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.Error(w, req, http.StatusNotFound)
	})
}
