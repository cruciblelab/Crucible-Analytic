package beacon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/asnlookup"
	"github.com/cruciblelab/crucible-analytic/internal/limiter"
	"github.com/cruciblelab/crucible-analytic/internal/privacy"
)

// fakeSink records what the handler produced, so the HTTP layer can be
// exercised without a database.
type fakeSink struct {
	mu   sync.Mutex
	rows []Row
	full bool // when true, every Enqueue reports "no room"
}

func (f *fakeSink) Enqueue(row Row) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.full {
		return false
	}
	f.rows = append(f.rows, row)
	return true
}

func (f *fakeSink) all() []Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Row(nil), f.rows...)
}

func (f *fakeSink) only(t *testing.T) Row {
	t.Helper()
	rows := f.all()
	if len(rows) != 1 {
		t.Fatalf("sink holds %d rows, want exactly 1", len(rows))
	}
	return rows[0]
}

// fakeResolver returns one fixed answer, so enrichment can be checked
// without loaded range tables.
type fakeResolver struct{ result asnlookup.Result }

func (f fakeResolver) Resolve(netip.Addr) asnlookup.Result { return f.result }

func newTestServer(t *testing.T, sink Sink) *Server {
	t.Helper()
	return &Server{
		Sites:    []string{"acme"},
		Sink:     sink,
		Visitors: newTestVisitorIDs(t),
		Now:      func() time.Time { return time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC) },
		// Full mode, which now means "masked address plus a keyed
		// token". The tests below that care which address was resolved
		// compare its masked form, because no mode stores a raw one.
		IPMode:    privacy.IPFull,
		IPHashKey: []byte("otuz-iki-baytlik-test-anahtari!!"),
	}
}

// post sends body to the event endpoint as the snippet would: a plain
// text/plain POST from a browser.
func post(t *testing.T, s *Server, body string, mutate ...func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, DefaultPathPrefix+"/event", strings.NewReader(body))
	r.RemoteAddr = "203.0.113.9:41234"
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("User-Agent", chromeUA)
	for _, m := range mutate {
		m(r)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func TestServer_AcceptsAPageviewAndBuildsTheRow(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)

	w := post(t, s, `{"site":"acme","type":"pageview","url":"/pricing?utm_source=x","referrer":"https://google.com/search?q=y","title":"Pricing","screen_w":1920,"screen_h":1080,"language":"tr-TR"}`)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %s", w.Code, w.Body)
	}

	row := sink.only(t)
	if row.SiteID != "acme" || row.EventType != TypePageview {
		t.Errorf("SiteID/EventType = %q/%q", row.SiteID, row.EventType)
	}
	if row.Path != "/pricing" || row.Query != "utm_source=x" {
		t.Errorf("Path/Query = %q/%q", row.Path, row.Query)
	}
	if row.ReferrerHost != "google.com" {
		t.Errorf("ReferrerHost = %q", row.ReferrerHost)
	}
	if row.Title != "Pricing" || row.Language != "tr-TR" || row.ScreenW != 1920 || row.ScreenH != 1080 {
		t.Errorf("client-supplied fields wrong: %+v", row)
	}
	// Derived server-side from the User-Agent header, never sent.
	if row.Browser != "Chrome" || row.OS != "Windows" || row.Device != DeviceDesktop {
		t.Errorf("user agent classification = %q/%q/%q", row.Browser, row.OS, row.Device)
	}
	if row.IP != netip.MustParseAddr("203.0.113.0") {
		t.Errorf("IP = %v, want the masked network - no mode stores a raw address", row.IP)
	}
	if len(row.IPHash) == 0 {
		t.Error("full mode wrote no token, so two visitors in one /24 are indistinguishable")
	}
	if row.VisitorID == "" {
		t.Error("VisitorID is empty")
	}
	if !row.Time.Equal(time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)) {
		t.Errorf("Time = %v, want the injected clock", row.Time)
	}
}

func TestServer_AcceptsANamedEvent(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)

	if w := post(t, s, `{"site":"acme","type":"event","name":"signup","url":"/register"}`); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %s", w.Code, w.Body)
	}
	row := sink.only(t)
	if row.EventType != TypeEvent || row.EventName != "signup" {
		t.Errorf("EventType/EventName = %q/%q", row.EventType, row.EventName)
	}
}

// The snippet is public and its data-site attribute is a claim anyone
// can copy, so the allowlist is the only thing stopping arbitrary
// callers writing rows under someone else's site.
func TestServer_RejectsAnUnknownSite(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)

	w := post(t, s, `{"site":"someone-elses-site","type":"pageview","url":"/"}`)
	if w.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", w.Code)
	}
	if rows := sink.all(); len(rows) != 0 {
		t.Errorf("a row was stored for an unauthorized site: %+v", rows)
	}
}

func TestServer_RejectsBadPayloads(t *testing.T) {
	cases := map[string]string{
		"not json":        `{{{`,
		"empty body":      ``,
		"unknown type":    `{"site":"acme","type":"click","url":"/"}`,
		"event no name":   `{"site":"acme","type":"event","url":"/"}`,
		"no site":         `{"type":"pageview","url":"/"}`,
		"json not object": `"a string"`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			sink := &fakeSink{}
			w := post(t, newTestServer(t, sink), body)
			if w.Code != http.StatusBadRequest && w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want a 4xx", w.Code)
			}
			if rows := sink.all(); len(rows) != 0 {
				t.Errorf("a row was stored for %q", body)
			}
		})
	}
}

func TestServer_RejectsAnOversizedBody(t *testing.T) {
	sink := &fakeSink{}
	// Valid JSON, just far too much of it.
	body := `{"site":"acme","type":"pageview","url":"/","title":"` + strings.Repeat("x", maxBodyBytes*2) + `"}`

	w := post(t, newTestServer(t, sink), body)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if rows := sink.all(); len(rows) != 0 {
		t.Error("an oversized body produced a row")
	}
}

func TestServer_UnknownFieldsAreIgnoredNotRejected(t *testing.T) {
	// The snippet is cached in browsers for an hour, so a rollout that
	// adds a field always has a window of new-script/old-server. Strict
	// decoding would turn that window into total event loss.
	sink := &fakeSink{}
	w := post(t, newTestServer(t, sink), `{"site":"acme","type":"pageview","url":"/","some_future_field":123}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %s", w.Code, w.Body)
	}
	if row := sink.only(t); row.Path != "/" {
		t.Errorf("Path = %q", row.Path)
	}
}

func TestServer_SameClientGetsAStableVisitorID(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)

	post(t, s, `{"site":"acme","type":"pageview","url":"/one"}`)
	post(t, s, `{"site":"acme","type":"pageview","url":"/two"}`)

	rows := sink.all()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].VisitorID != rows[1].VisitorID {
		t.Errorf("one visitor produced two IDs: %q and %q", rows[0].VisitorID, rows[1].VisitorID)
	}
}

func TestServer_UsesTheForwardedAddressOnlyFromATrustedProxy(t *testing.T) {
	behindProxy := func(r *http.Request) {
		r.RemoteAddr = "127.0.0.1:5555"
		r.Header.Set("X-Forwarded-For", "198.51.100.23")
	}

	trusting := newTestServer(t, &fakeSink{})
	trusting.ClientIP = ClientIPResolver{TrustedProxies: mustPrefixes(t, "127.0.0.1/32")}
	trusting.Sink = &fakeSink{}
	post(t, trusting, `{"site":"acme","type":"pageview","url":"/"}`, behindProxy)
	if got := trusting.Sink.(*fakeSink).only(t).IP; got != netip.MustParseAddr("198.51.100.0") {
		t.Errorf("with a trusted proxy: IP = %v, want the masked 198.51.100.0", got)
	}

	suspicious := newTestServer(t, &fakeSink{})
	post(t, suspicious, `{"site":"acme","type":"pageview","url":"/"}`, behindProxy)
	if got := suspicious.Sink.(*fakeSink).only(t).IP; got != netip.MustParseAddr("127.0.0.0") {
		t.Errorf("with no trusted proxies: IP = %v, want the masked peer 127.0.0.0", got)
	}
}

func TestServer_EnrichesWithCountryAndASNWhenAResolverIsSet(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)
	s.Resolver = fakeResolver{result: asnlookup.Result{Country: "TR", ASN: 9121, ASNName: "TURKTELEKOM", Found: true}}

	post(t, s, `{"site":"acme","type":"pageview","url":"/"}`)

	row := sink.only(t)
	if row.Country != "TR" || row.ASN != 9121 || row.ASNOrg != "TURKTELEKOM" {
		t.Errorf("geo enrichment = %q/%d/%q", row.Country, row.ASN, row.ASNOrg)
	}
}

func TestServer_WithoutAResolverGeoStaysEmpty(t *testing.T) {
	sink := &fakeSink{}
	post(t, newTestServer(t, sink), `{"site":"acme","type":"pageview","url":"/"}`)

	row := sink.only(t)
	if row.Country != "" || row.ASN != 0 || row.ASNOrg != "" {
		t.Errorf("geo fields should be the not-resolved zero values, got %q/%d/%q", row.Country, row.ASN, row.ASNOrg)
	}
}

// A browser cannot usefully act on "your pageview was dropped", and a
// retry would only add load to a process that just said it has none to
// spare - so a full buffer is still a 204, counted rather than reported.
func TestServer_AFullSinkIsStillA204ButCounted(t *testing.T) {
	sink := &fakeSink{full: true}
	s := newTestServer(t, sink)

	if w := post(t, s, `{"site":"acme","type":"pageview","url":"/"}`); w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
	accepted, dropped, _ := s.Counters()
	if accepted != 0 || dropped != 1 {
		t.Errorf("counters = accepted %d, dropped %d; want 0 and 1", accepted, dropped)
	}
}

func TestServer_CountsRejections(t *testing.T) {
	s := newTestServer(t, &fakeSink{})
	post(t, s, `{"site":"nope","type":"pageview","url":"/"}`)
	post(t, s, `{{{`)

	if _, _, rejected := s.Counters(); rejected != 2 {
		t.Errorf("rejected = %d, want 2", rejected)
	}
}

func TestServer_ServesTheScriptWithARevalidatableETag(t *testing.T) {
	s := newTestServer(t, &fakeSink{})

	r := httptest.NewRequest(http.MethodGet, DefaultPathPrefix+"/ca.js", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q", ct)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag; every repeat visitor would re-download the script")
	}
	if !strings.Contains(w.Body.String(), "crucible") {
		t.Error("served script does not look like beacon.js")
	}

	// A browser revalidating with the tag it already has.
	r2 := httptest.NewRequest(http.MethodGet, DefaultPathPrefix+"/ca.js", nil)
	r2.Header.Set("If-None-Match", etag)
	w2 := httptest.NewRecorder()
	s.Handler().ServeHTTP(w2, r2)
	if w2.Code != http.StatusNotModified {
		t.Errorf("revalidation status = %d, want 304", w2.Code)
	}
	if w2.Body.Len() != 0 {
		t.Error("a 304 carried a body")
	}
}

func TestServer_RejectsWrongMethods(t *testing.T) {
	s := newTestServer(t, &fakeSink{})
	cases := []struct{ method, path string }{
		{http.MethodGet, DefaultPathPrefix + "/event"},
		{http.MethodPost, DefaultPathPrefix + "/ca.js"},
		{http.MethodDelete, DefaultPathPrefix + "/event"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s -> %d, want 405", tc.method, tc.path, w.Code)
		}
	}
}

func TestServer_CORSDefaultsToAllowingEveryOrigin(t *testing.T) {
	s := newTestServer(t, &fakeSink{})
	w := post(t, s, `{"site":"acme","type":"pageview","url":"/"}`, func(r *http.Request) {
		r.Header.Set("Origin", "https://acme.example")
	})
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

func TestServer_CORSCanBeNarrowed(t *testing.T) {
	s := newTestServer(t, &fakeSink{})
	s.AllowedOrigins = []string{"https://acme.example"}

	allowed := post(t, s, `{"site":"acme","type":"pageview","url":"/"}`, func(r *http.Request) {
		r.Header.Set("Origin", "https://acme.example")
	})
	if got := allowed.Header().Get("Access-Control-Allow-Origin"); got != "https://acme.example" {
		t.Errorf("allowed origin echoed as %q", got)
	}
	if vary := allowed.Header().Get("Vary"); !strings.Contains(vary, "Origin") {
		t.Error("no Vary: Origin; a shared cache could hand one origin's allow header to another")
	}

	denied := post(t, s, `{"site":"acme","type":"pageview","url":"/"}`, func(r *http.Request) {
		r.Header.Set("Origin", "https://elsewhere.example")
	})
	if got := denied.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("a non-allowlisted origin got %q", got)
	}
}

func TestServer_AnswersPreflight(t *testing.T) {
	s := newTestServer(t, &fakeSink{})
	r := httptest.NewRequest(http.MethodOptions, DefaultPathPrefix+"/event", nil)
	r.Header.Set("Origin", "https://acme.example")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", w.Code)
	}
	if !strings.Contains(w.Header().Get("Access-Control-Allow-Methods"), "POST") {
		t.Errorf("Allow-Methods = %q", w.Header().Get("Access-Control-Allow-Methods"))
	}
}

func TestServer_LimiterFailClosedRejects(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)
	s.Limiter = limiter.New(limiter.Config{MaxRequestsPerSecond: 1, Policy: limiter.PolicyFailClosed})

	var lastCode int
	for range 20 {
		lastCode = post(t, s, `{"site":"acme","type":"pageview","url":"/"}`).Code
	}
	if lastCode != http.StatusTooManyRequests {
		t.Errorf("status after exceeding the rate limit = %d, want 429", lastCode)
	}
	if _, _, rejected := s.Counters(); rejected == 0 {
		t.Error("no rejections were counted")
	}
}

// fail_open means "never be the reason the site breaks" for the
// collector, which fronts one. The beacon fronts nothing, so the
// equivalent is to accept the request and drop the event.
func TestServer_LimiterFailOpenAcceptsButDrops(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)
	s.Limiter = limiter.New(limiter.Config{MaxRequestsPerSecond: 1, Policy: limiter.PolicyFailOpen})

	for range 20 {
		if code := post(t, s, `{"site":"acme","type":"pageview","url":"/"}`).Code; code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 throughout under fail_open", code)
		}
	}
	accepted, dropped, _ := s.Counters()
	if dropped == 0 {
		t.Error("nothing was shed despite exceeding the rate limit")
	}
	if accepted == 0 {
		t.Error("nothing was accepted at all")
	}
}

func TestServer_HealthzIsOpenAndDataless(t *testing.T) {
	s := newTestServer(t, &fakeSink{})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("healthz body is not JSON: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("body = %v", body)
	}
}

func TestServer_HonorsACustomPathPrefix(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)
	s.PathPrefix = "stats/" // no leading slash, trailing one: both normalized

	r := httptest.NewRequest(http.MethodPost, "/stats/event", strings.NewReader(`{"site":"acme","type":"pageview","url":"/"}`))
	r.RemoteAddr = "203.0.113.9:41234"
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body %s", w.Code, w.Body)
	}
	if len(sink.all()) != 1 {
		t.Error("no row was stored under the custom prefix")
	}
}

// --- IP storage mode (A7) ---

// The default is what ends up in production, because it is the value
// nobody looks at. A Server built without an IPMode - by a test, or by a
// future caller that forgets the field - must mask.
func TestServer_MasksTheAddressByDefault(t *testing.T) {
	sink := &fakeSink{}
	s := &Server{
		Sites:    []string{"acme"},
		Sink:     sink,
		Visitors: newTestVisitorIDs(t),
		// No IPMode set at all.
	}

	post(t, s, `{"site":"acme","type":"pageview","url":"/"}`)

	if got := sink.only(t).IP; got != netip.MustParseAddr("203.0.113.0") {
		t.Errorf("IP = %v, want the masked 203.0.113.0; an unset mode stored the whole address", got)
	}
}

// Masking happens after the visitor id is derived and after country and
// ASN are resolved. Doing it first would degrade both, and nothing in
// the output would say why - the numbers would simply be different.
func TestServer_MasksAfterDerivingTheVisitorIDAndResolvingGeography(t *testing.T) {
	masked := &fakeSink{}
	maskedServer := newTestServer(t, masked)
	maskedServer.IPMode = privacy.IPMasked
	maskedServer.Resolver = fakeResolver{result: asnlookup.Result{Country: "TR", ASN: 9121, ASNName: "TURKTELEKOM", Found: true}}

	full := &fakeSink{}
	fullServer := newTestServer(t, full)
	fullServer.Resolver = fakeResolver{result: asnlookup.Result{Country: "TR", ASN: 9121, ASNName: "TURKTELEKOM", Found: true}}
	_ = full

	post(t, maskedServer, `{"site":"acme","type":"pageview","url":"/"}`)
	post(t, fullServer, `{"site":"acme","type":"pageview","url":"/"}`)

	maskedRow, fullRow := masked.only(t), full.only(t)

	if maskedRow.IP != netip.MustParseAddr("203.0.113.0") {
		t.Errorf("masked IP = %v, want 203.0.113.0", maskedRow.IP)
	}
	if fullRow.IP != netip.MustParseAddr("203.0.113.0") {
		t.Errorf("full IP = %v, want the masked 203.0.113.0", fullRow.IP)
	}
	// What separates the two modes is the token, not the address.
	if len(maskedRow.IPHash) != 0 {
		t.Error("masked mode wrote a token, which would need a key it should not need")
	}
	if len(fullRow.IPHash) == 0 {
		t.Error("full mode wrote no token")
	}

	// The two servers share a visitor-id salt only if they were built
	// from the same VisitorIDs, which they are not - so compare what can
	// be compared: masking must not have emptied either field.
	if maskedRow.VisitorID == "" {
		t.Error("masking left the visitor id empty; it was derived from the masked address")
	}
	if maskedRow.Country != "TR" || maskedRow.ASN != 9121 {
		t.Errorf("masking cost the geography: country %q, asn %d", maskedRow.Country, maskedRow.ASN)
	}
}

// Two visitors in one /24 must reach the same stored address - that is
// the cost of masking, and the crossover views have to be able to say
// so. Asserted rather than assumed.
func TestServer_MaskingCollapsesA24(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)
	s.IPMode = privacy.IPMasked

	post(t, s, `{"site":"acme","type":"pageview","url":"/"}`, func(r *http.Request) {
		r.RemoteAddr = "203.0.113.9:41234"
	})
	post(t, s, `{"site":"acme","type":"pageview","url":"/"}`, func(r *http.Request) {
		r.RemoteAddr = "203.0.113.200:41234"
	})

	rows := sink.all()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].IP != rows[1].IP {
		t.Errorf("two addresses in one /24 stored as %v and %v", rows[0].IP, rows[1].IP)
	}

	// And they are still counted as two visitors, because the id was
	// derived before masking. Losing this is the failure the ordering
	// exists to prevent.
	if rows[0].VisitorID == rows[1].VisitorID {
		t.Error("two visitors in one /24 collapsed to a single visitor id; masking ran before the id was derived")
	}
}

func TestServer_SetIPModeTakesEffectOnTheNextEvent(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink) // starts on full

	post(t, s, `{"site":"acme","type":"pageview","url":"/one"}`)
	s.SetIPMode(privacy.IPMasked)
	post(t, s, `{"site":"acme","type":"pageview","url":"/two"}`)

	rows := sink.all()
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	// The address is masked either side of the switch. What changes is
	// the token, which is the only thing the mode decides.
	for i, row := range rows {
		if row.IP != netip.MustParseAddr("203.0.113.0") {
			t.Errorf("row %d IP = %v, want the masked address in both modes", i, row.IP)
		}
	}
	if len(rows[0].IPHash) == 0 {
		t.Error("the event before the switch carried no token, but full mode was in force")
	}
	if len(rows[1].IPHash) != 0 {
		t.Error("the event after the switch still carried a token, so the change did not take effect")
	}
}
