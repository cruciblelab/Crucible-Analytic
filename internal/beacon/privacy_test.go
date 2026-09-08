package beacon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/privacy"
)

// The disclosure, checked against the thing it discloses.
//
// Every assertion below compares the page or the JSON with a *behaviour*
// of this same process rather than with a sentence typed into the test.
// The stored list is compared with the writer's column list; the token
// claim is compared with what storedPseudonym actually returns for a
// real address; the mode is changed while the server runs and the answer
// is read again. A test that quoted the expected prose would pass on the
// day somebody replaced the page with a beautifully written lie.

// fetchPrivacy performs one GET and returns status and body.
func fetchPrivacy(t *testing.T, s *Server, path string, mutate ...func(*http.Request)) (int, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = "203.0.113.9:41234"
	for _, m := range mutate {
		m(r)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w.Code, w.Body.String()
}

// privacyJSON reads the JSON endpoint and decodes it loosely.
//
// Into a map rather than into privacyResponse: decoding into the type
// that encoded it would agree with itself about field names, and the
// field names are the contract a customer's own page is written against.
func privacyJSON(t *testing.T, s *Server, prefix string) map[string]any {
	t.Helper()
	code, body := fetchPrivacy(t, s, prefix+"/privacy")
	if code != http.StatusOK {
		t.Fatalf("GET %s/privacy = %d\n%s", prefix, code, body)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("the disclosure is not JSON: %v\n%s", err, body)
	}
	return got
}

// newPrivacyServer builds a beacon that serves under a prefix nobody
// would guess.
//
// Not the default prefix on purpose: two routes hard-coded as "/_ca/..."
// would answer every test written against the default and 404 in any
// deployment that set PathPrefix - which is the deployment most likely
// to be running behind somebody else's nginx.
func newPrivacyServer(t *testing.T, mode privacy.IPMode) (*Server, string) {
	t.Helper()
	s := &Server{
		Sites:     []string{"acme"},
		Sink:      &fakeSink{},
		Visitors:  newTestVisitorIDs(t),
		IPMode:    mode,
		IPHashKey: []byte("otuz-iki-baytlik-test-anahtari!!"),
		// A prefix that is not the default, and not a prefix of it.
		PathPrefix: "/olcum",
	}
	return s, "/olcum"
}

// TestBothDisclosureEndpointsAnswerUnderTheConfiguredPrefix.
func TestBothDisclosureEndpointsAnswerUnderTheConfiguredPrefix(t *testing.T) {
	s, prefix := newPrivacyServer(t, privacy.IPMasked)

	for _, path := range []string{prefix + "/privacy", prefix + "/privacy.html"} {
		code, body := fetchPrivacy(t, s, path)
		if code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200\n%s", path, code, body)
		}
		if strings.TrimSpace(body) == "" {
			t.Errorf("GET %s answered 200 with an empty body", path)
		}
	}

	// And nowhere else. A disclosure reachable at the root would work in
	// every test and be unroutable in the deployment this prefix exists
	// for: whatever forwards /olcum/ forwards these, and nothing
	// forwards /privacy.
	for _, path := range []string{"/privacy", "/privacy.html", DefaultPathPrefix + "/privacy"} {
		if code, _ := fetchPrivacy(t, s, path); code == http.StatusOK {
			t.Errorf("GET %s answered 200; this server serves under %s", path, prefix)
		}
	}
}

// TestTheDisclosureEndpointsAnswerOnlyGET.
//
// The two of them read nothing and change nothing, so every other method
// is a surface with no purpose. Asserted rather than assumed: the route
// pattern carries the method, and a pattern written without one would
// hand a POST body to a template.
func TestTheDisclosureEndpointsAnswerOnlyGET(t *testing.T) {
	s, prefix := newPrivacyServer(t, privacy.IPMasked)

	for _, path := range []string{prefix + "/privacy", prefix + "/privacy.html"} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
			r := httptest.NewRequest(method, path, strings.NewReader("{}"))
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, r)
			if w.Code == http.StatusOK {
				t.Errorf("%s %s answered 200", method, path)
			}
		}
	}
}

// TestTheDisclosureListsTheColumnsTheWriterActuallyWrites.
//
// Against the writer's own list, which is the list CopyFrom uses. The
// direction that matters is the missing one: a disclosure naming fewer
// fields than are stored tells a visitor less is collected than is, and
// that is the one mistake here that has a legal shape.
func TestTheDisclosureListsTheColumnsTheWriterActuallyWrites(t *testing.T) {
	s, prefix := newPrivacyServer(t, privacy.IPMasked)

	got := privacyJSON(t, s, prefix)
	listed, ok := got["stored"].([]any)
	if !ok {
		t.Fatalf("stored is %T, want a list", got["stored"])
	}

	said := make(map[string]bool, len(listed))
	for _, v := range listed {
		name, ok := v.(string)
		if !ok {
			t.Fatalf("stored contains a %T", v)
		}
		said[name] = true
	}
	for _, want := range columns {
		if !said[want] {
			t.Errorf("the writer stores %q and the disclosure does not name it", want)
		}
		delete(said, want)
	}
	for extra := range said {
		t.Errorf("the disclosure names %q, which the writer does not store", extra)
	}

	// Sorted, so two deployments differ in the page only where they
	// differ in what they store.
	for i := 1; i < len(listed); i++ {
		if listed[i-1].(string) > listed[i].(string) {
			t.Errorf("stored is not sorted: %q before %q", listed[i-1], listed[i])
		}
	}

	// And the page names them too, since a visitor reads that one.
	_, page := fetchPrivacy(t, s, prefix+"/privacy.html")
	for _, want := range columns {
		if !strings.Contains(page, want) {
			t.Errorf("the page does not name the stored column %q", want)
		}
	}
}

// TestChangingTheModeWhileTheServerRunsChangesWhatVisitorsRead is the
// phase's headline measurement.
//
// One process, one handler, two reads with a SetIPMode between them.
// Nothing is restarted and nothing is rebuilt, because the claim being
// tested is precisely that a customer switching this on the panel does
// not have to restart anything for the disclosure to catch up.
func TestChangingTheModeWhileTheServerRunsChangesWhatVisitorsRead(t *testing.T) {
	s, prefix := newPrivacyServer(t, privacy.IPMasked)

	_, before := fetchPrivacy(t, s, prefix+"/privacy.html")
	maskedJSON := privacyJSON(t, s, prefix)

	s.SetIPMode(privacy.IPFull)

	_, after := fetchPrivacy(t, s, prefix+"/privacy.html")
	fullJSON := privacyJSON(t, s, prefix)

	if before == after {
		t.Fatal("the page reads identically in masked and full mode, so a customer " +
			"who switched to full precision is still showing visitors the masked text")
	}

	if maskedJSON["ip_storage"] != string(privacy.IPMasked) {
		t.Errorf("before the switch the JSON said ip_storage = %v", maskedJSON["ip_storage"])
	}
	if fullJSON["ip_storage"] != string(privacy.IPFull) {
		t.Errorf("after the switch the JSON said ip_storage = %v", fullJSON["ip_storage"])
	}
	if maskedJSON["token_from_whole_address"] != false {
		t.Errorf("masked mode discloses a token: %v", maskedJSON["token_from_whole_address"])
	}
	if fullJSON["token_from_whole_address"] != true {
		t.Errorf("full mode discloses no token: %v", fullJSON["token_from_whole_address"])
	}

	// The page's two branches, named so a failure says which sentence
	// went missing rather than that two strings differ.
	if !strings.Contains(after, "jeton") {
		t.Error("full mode's page does not mention the token it stores")
	}
	if strings.Contains(before, "jeton") {
		t.Error("masked mode's page mentions a token this deployment does not store")
	}

	// And back, because a switch a customer can undo is one the page has
	// to be able to undo. A page that only ever moved in one direction
	// would pass everything above.
	s.SetIPMode(privacy.IPMasked)
	_, again := fetchPrivacy(t, s, prefix+"/privacy.html")
	if again != before {
		t.Error("switching back to masked did not restore the masked text")
	}
}

// TestTheDisclosureAgreesWithWhatTheWriterWouldStore.
//
// The strongest form this check has: the claim on the page is compared
// with the function that fills the column, for a real address, under the
// mode in force at that moment. Two independent readings of one live
// setting - which is the failure mode a second copy of anything creates.
func TestTheDisclosureAgreesWithWhatTheWriterWouldStore(t *testing.T) {
	s, prefix := newPrivacyServer(t, privacy.IPMasked)
	visitor := netip.MustParseAddr("185.23.45.178")

	for _, mode := range []privacy.IPMode{privacy.IPMasked, privacy.IPFull, privacy.IPMasked} {
		s.SetIPMode(mode)

		said := privacyJSON(t, s, prefix)
		token := storedPseudonym(visitor, s.ipMode(), s.IPHashKey)

		if said["token_from_whole_address"] != (len(token) > 0) {
			t.Errorf("in %s mode the disclosure says a token is stored: %v; the writer "+
				"produced %d bytes of one", mode, said["token_from_whole_address"], len(token))
		}

		// The address claim, in both modes, against the same call the
		// row is built from.
		stored := storedAddress(visitor, s.ipMode())
		if stored == visitor {
			t.Errorf("in %s mode the writer stores the visitor's address unchanged, "+
				"and the disclosure says it never does", mode)
		}
	}
}

// TestThePageTellsAVisitorNothingAboutThemselves.
//
// The page reads no request-scoped anything, so there is nothing for it
// to leak - but "there is nothing" is a claim, and this measures it. The
// request below carries a distinctive address, browser string, cookie
// and query, and none of them may come back.
func TestThePageTellsAVisitorNothingAboutThemselves(t *testing.T) {
	s, prefix := newPrivacyServer(t, privacy.IPFull)

	const (
		addr   = "198.51.100.77"
		agent  = "Mozilla/5.0 (BenimTarayicim/9.9)"
		cookie = "kurabiye=cok-gizli-deger"
		query  = "?kim=ali&nereden=veli"
	)
	mark := func(r *http.Request) {
		r.RemoteAddr = addr + ":51234"
		r.Header.Set("User-Agent", agent)
		r.Header.Set("Cookie", cookie)
		r.Header.Set("X-Forwarded-For", "203.0.113.250")
		r.Header.Set("Referer", "https://baska-site.example/gizli-sayfa")
	}

	for _, path := range []string{prefix + "/privacy" + query, prefix + "/privacy.html" + query} {
		_, body := fetchPrivacy(t, s, path, mark)
		for _, secret := range []string{
			addr, "BenimTarayicim", "cok-gizli-deger", "203.0.113.250",
			"gizli-sayfa", "ali", "veli",
		} {
			if strings.Contains(body, secret) {
				t.Errorf("%s echoed %q back to the visitor", path, secret)
			}
		}
	}

	// Nor anything about the deployment's traffic. Asserted as an
	// equality across real traffic rather than by hunting for digits in
	// the body: the page is not allowed to differ, so the comparison is
	// the whole page, and a count added later in any wording fails it.
	// A visitor-facing page that printed a number would be an
	// unauthenticated read of something the panel asks for a password
	// to show.
	_, quiet := fetchPrivacy(t, s, prefix+"/privacy.html")
	for i := 0; i < 3; i++ {
		// To this server's own prefix, not the default one: post()
		// writes to /_ca/event, which this beacon does not serve, and
		// three 404s would leave both readings identical for a reason
		// that has nothing to do with the property.
		r := httptest.NewRequest(http.MethodPost, prefix+"/event",
			strings.NewReader(`{"site":"acme","type":"pageview","url":"/x"}`))
		r.RemoteAddr = "203.0.113.9:41234"
		r.Header.Set("Content-Type", "text/plain")
		r.Header.Set("User-Agent", chromeUA)
		s.Handler().ServeHTTP(httptest.NewRecorder(), r)
	}
	accepted, _, _ := s.Counters()
	if accepted != 3 {
		t.Fatalf("the beacon accepted %d of 3 events, so the comparison below "+
			"would be between two idle servers", accepted)
	}
	_, busy := fetchPrivacy(t, s, prefix+"/privacy.html")
	if busy != quiet {
		t.Error("the page changed after three events were recorded, so it is " +
			"telling an anonymous reader something about this deployment's traffic")
	}
}

// TestThePageNamesTheSiteOnlyWhenThereIsOne.
//
// A visitor on a shared host has a right to know which property this
// describes. A visitor on a beacon serving four sites has no business
// reading the other three names, which are the customer's, not theirs.
func TestThePageNamesTheSiteOnlyWhenThereIsOne(t *testing.T) {
	one, prefix := newPrivacyServer(t, privacy.IPMasked)
	_, page := fetchPrivacy(t, one, prefix+"/privacy.html")
	if !strings.Contains(page, "acme") {
		t.Error("a beacon serving one site does not name it")
	}

	many, prefix := newPrivacyServer(t, privacy.IPMasked)
	many.Sites = []string{"acme", "gizli-musteri", "ucuncu"}
	_, page = fetchPrivacy(t, many, prefix+"/privacy.html")
	for _, site := range many.Sites {
		if strings.Contains(page, site) {
			t.Errorf("a beacon serving %d sites named %q on a public page",
				len(many.Sites), site)
		}
	}
	said := privacyJSON(t, many, prefix)
	if _, ok := said["site"]; ok {
		t.Errorf("the JSON carries site = %v for a beacon serving several", said["site"])
	}
}

// TestTheReasonIdReachesMachinesAndTheSentenceReachesPeople.
//
// The id is for a customer's own page, which switches on it. It is not
// for a visitor, and a page that printed "no_durable_identity" at
// somebody would be a disclosure written for a program.
func TestTheReasonIdReachesMachinesAndTheSentenceReachesPeople(t *testing.T) {
	s, prefix := newPrivacyServer(t, privacy.IPMasked)

	said := privacyJSON(t, s, prefix)
	if said["no_deletion_reason"] != privacy.NoDeletionNoIdentity {
		t.Errorf("the JSON gives the reason as %v", said["no_deletion_reason"])
	}
	if said["deletion_on_request"] != false {
		t.Errorf("the JSON offers deletion on request: %v", said["deletion_on_request"])
	}

	_, page := fetchPrivacy(t, s, prefix+"/privacy.html")
	if strings.Contains(page, privacy.NoDeletionNoIdentity) {
		t.Errorf("the page shows a visitor the raw id %q", privacy.NoDeletionNoIdentity)
	}
	// The sentence itself, by the two things it must say: that there is
	// no deletion, and why.
	if !strings.Contains(page, "Silme talebi") {
		t.Error("the page does not tell a visitor there is no deletion request")
	}

	// The opt-out call, from the constant rather than from prose: a page
	// naming a function beacon.js does not define is instructions that
	// do nothing.
	if !strings.Contains(page, privacy.OptOutCall) {
		t.Errorf("the page does not name %q", privacy.OptOutCall)
	}
	if said["opt_out"] != privacy.OptOutCall {
		t.Errorf("the JSON names %v as the opt-out call", said["opt_out"])
	}
}

// TestTheRotationPeriodIsSaidInWordsWithoutRounding.
//
// The phrase is the one part of this page that turns a number into
// language, so it is the one part that can flatter. "Her 36 saatte"
// rendered as "her gün" would be a disclosure that rounds in the
// direction of sounding better.
func TestTheRotationPeriodIsSaidInWordsWithoutRounding(t *testing.T) {
	cases := []struct {
		period time.Duration
		want   string
	}{
		{24 * time.Hour, "günde"},
		{48 * time.Hour, "her 2 günde"},
		{6 * time.Hour, "her 6 saatte"},
		{time.Hour, "her 1 saatte"},
		{90 * time.Minute, "1h30m0s"},
	}
	for _, tc := range cases {
		got := privacyResponse{Notice: privacy.Notice{IdentifierRotatesEvery: tc.period}}.RotatesHuman()
		if !strings.Contains(got, tc.want) {
			t.Errorf("a %s rotation reads as %q, want something containing %q",
				tc.period, got, tc.want)
		}
	}

	// And the page uses the server's own period rather than a constant.
	s, prefix := newPrivacyServer(t, privacy.IPMasked)
	s.Visitors.SaltPeriod = 6 * time.Hour
	_, page := fetchPrivacy(t, s, prefix+"/privacy.html")
	if !strings.Contains(page, "her 6 saatte") {
		t.Error("the page does not say the rotation period this server is using")
	}
	said := privacyJSON(t, s, prefix)
	if said["identifier_rotates_every_seconds"] != float64(6*time.Hour) {
		t.Errorf("the JSON reports the rotation as %v", said["identifier_rotates_every_seconds"])
	}
}

// TestTheDisclosureIsReadableFromTheCustomersOwnPage.
//
// The JSON exists so a customer can print these facts on their own
// privacy page, which is on their origin and not on this one. Without
// the header that is a fetch every browser blocks, and the endpoint
// would be one only curl can use.
func TestTheDisclosureIsReadableFromTheCustomersOwnPage(t *testing.T) {
	s, prefix := newPrivacyServer(t, privacy.IPMasked)

	r := httptest.NewRequest(http.MethodGet, prefix+"/privacy", nil)
	r.Header.Set("Origin", "https://musteri.example")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q; a customer's page could not read this", got)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q", got)
	}

	// Cached, but not for long: the mode can change while the process
	// runs, and an hour-long cache would keep serving the old answer
	// after a customer changed the setting.
	cache := w.Header().Get("Cache-Control")
	if !strings.Contains(cache, "max-age=") {
		t.Errorf("Cache-Control = %q; every visitor would re-fetch this", cache)
	}
	for _, tooLong := range []string{"max-age=3600", "max-age=86400", "immutable"} {
		if strings.Contains(cache, tooLong) {
			t.Errorf("Cache-Control = %q, which outlives a setting change", cache)
		}
	}
}

// P3: the switch, and the two facts only the operator knows.

// TestTheSurfaceCanBeWithdrawnWhileTheServerRuns.
//
// The claim is a pair, and only the pair is worth anything: a customer
// who switches the disclosure off gets 404 on both endpoints without
// restarting anything, and a customer who switches it back on gets them
// back the same way. A one-way test would pass on a server that had
// simply stopped serving.
func TestTheSurfaceCanBeWithdrawnWhileTheServerRuns(t *testing.T) {
	s, prefix := newPrivacyServer(t, privacy.IPMasked)

	for _, path := range []string{prefix + "/privacy", prefix + "/privacy.html"} {
		if code, _ := fetchPrivacy(t, s, path); code != http.StatusOK {
			t.Fatalf("GET %s = %d before the switch was touched; the default is on", path, code)
		}
	}

	s.SetDisclosure(Disclosure{Enabled: false})

	for _, path := range []string{prefix + "/privacy", prefix + "/privacy.html"} {
		code, body := fetchPrivacy(t, s, path)
		if code != http.StatusNotFound {
			t.Errorf("with the surface off, GET %s = %d, want 404\n%s", path, code, body)
		}
		// And it says nothing about the deployment on the way out. A
		// "disabled by the operator" page would be a disclosure of its
		// own, and one nobody asked for.
		for _, leak := range []string{"acme", "ip_storage", "visitor_id"} {
			if strings.Contains(body, leak) {
				t.Errorf("the 404 for %s mentions %q", path, leak)
			}
		}
	}

	s.SetDisclosure(Disclosure{Enabled: true})

	for _, path := range []string{prefix + "/privacy", prefix + "/privacy.html"} {
		if code, _ := fetchPrivacy(t, s, path); code != http.StatusOK {
			t.Errorf("switching the surface back on left GET %s at %d", path, code)
		}
	}
}

// TestTheClosedSurfaceStillAnswersTheScriptThatAsks.
//
// beacon.js asks the JSON endpoint whether to draw the embedded block,
// from the site's own origin. A 404 without the CORS header is a 404 the
// script cannot read: the browser blocks it, the promise rejects, and
// what should have been an answer becomes a console error on somebody
// else's privacy page.
func TestTheClosedSurfaceStillAnswersTheScriptThatAsks(t *testing.T) {
	s, prefix := newPrivacyServer(t, privacy.IPMasked)
	s.SetDisclosure(Disclosure{Enabled: false})

	r := httptest.NewRequest(http.MethodGet, prefix+"/privacy", nil)
	r.Header.Set("Origin", "https://musteri.example")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("the endpoint answered %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("the 404 carries Access-Control-Allow-Origin %q; the script cannot "+
			"read it, so 'switched off' arrives as an error rather than an answer", got)
	}
	// And it is not cached: the switch is live, so a cached 404 would
	// outlive the decision that produced it.
	if cache := w.Header().Get("Cache-Control"); !strings.Contains(cache, "no-store") {
		t.Errorf("the 404 is cached (%q); switching the surface back on would not "+
			"reach a browser that had already asked", cache)
	}
}

// TestTheEventEndpointIsNotAffectedByTheSwitch.
//
// The switch is about the disclosure, not about the measurement. A
// customer who takes the page down has not asked to stop counting, and a
// switch that stopped both would be one nobody could use for what it is
// for.
func TestTheEventEndpointIsNotAffectedByTheSwitch(t *testing.T) {
	s, prefix := newPrivacyServer(t, privacy.IPMasked)
	s.SetDisclosure(Disclosure{Enabled: false})

	r := httptest.NewRequest(http.MethodPost, prefix+"/event",
		strings.NewReader(`{"site":"acme","type":"pageview","url":"/x"}`))
	r.RemoteAddr = "203.0.113.9:41234"
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("User-Agent", chromeUA)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)

	if w.Code >= 400 {
		t.Errorf("with the disclosure off the event endpoint answered %d", w.Code)
	}
	if accepted, _, _ := s.Counters(); accepted != 1 {
		t.Errorf("the beacon accepted %d events with the disclosure off", accepted)
	}

	// The script is still served too: opting out lives in it, and P3
	// says the opt-out does not depend on us publishing a page.
	if code, body := fetchPrivacy(t, s, prefix+"/ca.js"); code != http.StatusOK ||
		!strings.Contains(body, "optOut") {
		t.Errorf("with the disclosure off the snippet answered %d and %s carry optOut",
			code, map[bool]string{true: "does", false: "does not"}[strings.Contains(body, "optOut")])
	}
}

// TestThePageLinksToTheOperatorsOwnPolicyOnlyWhenThereIsOne.
func TestThePageLinksToTheOperatorsOwnPolicyOnlyWhenThereIsOne(t *testing.T) {
	s, prefix := newPrivacyServer(t, privacy.IPMasked)

	// Nothing set: no link, and no empty link either. An <a href="">
	// reloads the page it is on, which is a worse answer than no link.
	_, page := fetchPrivacy(t, s, prefix+"/privacy.html")
	if strings.Contains(page, `href=""`) || strings.Contains(page, "href=\"#ZgotmplZ\"") {
		t.Errorf("the page carries a link with no target:\n%s", page)
	}
	if strings.Contains(page, "kendi gizlilik metni") {
		t.Error("the page offers the operator's own policy when none is set")
	}
	said := privacyJSON(t, s, prefix)
	if _, ok := said["policy_url"]; ok {
		t.Errorf("the JSON carries policy_url = %v when none is set", said["policy_url"])
	}

	// Set: the link is there, once, pointing where it was told.
	const url = "https://acme.example/gizlilik"
	s.SetDisclosure(Disclosure{Enabled: true, PolicyURL: url})
	_, page = fetchPrivacy(t, s, prefix+"/privacy.html")
	if !strings.Contains(page, `href="`+url+`"`) {
		t.Errorf("the page does not link to %s:\n%s", url, page)
	}
	said = privacyJSON(t, s, prefix)
	if said["policy_url"] != url {
		t.Errorf("the JSON says policy_url = %v", said["policy_url"])
	}
}

// TestAPolicyAddressTheDatabaseShouldNotHaveIsNotRendered.
//
// The panel refuses these on the way in. This is the other end: a row
// written by an older build, restored from a backup, or edited by hand
// reaches a page served to the public, and the page has to decline it
// without anybody to ask.
func TestAPolicyAddressTheDatabaseShouldNotHaveIsNotRendered(t *testing.T) {
	s, prefix := newPrivacyServer(t, privacy.IPMasked)

	for _, bad := range []string{
		"javascript:alert(document.domain)",
		"data:text/html,<script>alert(1)</script>",
		"/gizlilik",
		"https://ali:parola@acme.example/gizlilik",
	} {
		s.SetDisclosure(Disclosure{Enabled: true, PolicyURL: bad, Contact: bad})

		_, page := fetchPrivacy(t, s, prefix+"/privacy.html")
		for _, fragment := range []string{"javascript:", "data:text/html", "parola@", `href="/gizlilik"`} {
			if strings.Contains(page, fragment) {
				t.Errorf("with %q stored, the page contains %q", bad, fragment)
			}
		}
		said := privacyJSON(t, s, prefix)
		if v, ok := said["policy_url"]; ok {
			t.Errorf("with %q stored, the JSON hands a customer's own page %v", bad, v)
		}
		if v, ok := said["contact"]; ok {
			t.Errorf("with %q stored, the JSON hands a customer's own page %v", bad, v)
		}
	}
}

// TestTheContactIsLinkedTheWayItsKindRequires.
func TestTheContactIsLinkedTheWayItsKindRequires(t *testing.T) {
	s, prefix := newPrivacyServer(t, privacy.IPMasked)

	s.SetDisclosure(Disclosure{Enabled: true, Contact: "gizlilik@acme.example"})
	_, page := fetchPrivacy(t, s, prefix+"/privacy.html")
	if !strings.Contains(page, `href="mailto:gizlilik@acme.example"`) {
		t.Errorf("an email contact is not linked as mailto:\n%s", page)
	}

	s.SetDisclosure(Disclosure{Enabled: true, Contact: "https://acme.example/iletisim"})
	_, page = fetchPrivacy(t, s, prefix+"/privacy.html")
	if strings.Contains(page, "mailto:https") {
		t.Error("a form page is linked as an email address")
	}
	if !strings.Contains(page, `href="https://acme.example/iletisim"`) {
		t.Errorf("a form page is not linked:\n%s", page)
	}
}
