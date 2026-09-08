//go:build integration

package beacon

import (
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/browsertest"
	"github.com/cruciblelab/crucible-analytic/internal/privacy"
)

// The embedded disclosure, in a real browser, on a page that is not ours.
//
// # Why this cannot be checked from Go
//
// Both halves of the claim are about a browser. "The site puts an anchor
// and the script fills it" is a DOM operation; "no anchor, nothing
// drawn" is a statement about a document after script execution, which
// only a document that ran the script can answer. A Go test can read
// beacon.js and see the code; it cannot see whether the code runs, finds
// the anchor, and produces something a visitor can read.
//
// # The negative is the half worth paying for
//
// A widget that appears on pages nobody asked for is not a small bug in
// an analytics snippet. It is our markup inside somebody's checkout page
// and their layout broken by a vendor. So the second fixture below has
// no anchor, and the assertion is that the document after the script is
// the document before it.
func TestTheEmbeddedDisclosureInARealBrowser(t *testing.T) {
	if os.Getenv("CA_BROWSER_TEST") == "" {
		t.Skip("set CA_BROWSER_TEST=1 to run this; it needs node, playwright and a chromium build")
	}

	const site = "p2-tarayici"
	srv := &Server{
		Sites:     []string{site},
		Sink:      &fakeSink{},
		Visitors:  newTestVisitorIDs(t),
		IPMode:    privacy.IPMasked,
		IPHashKey: []byte("otuz-iki-baytlik-test-anahtari!!"),
	}

	mux := http.NewServeMux()
	mux.Handle("/", srv.Handler())

	// The customer's own privacy page: their markup, their words, one
	// anchor. Nothing here is ours except the script tag, which is what
	// makes this a test of embedding rather than of our own page.
	mux.HandleFunc("/gizlilik", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html lang="tr"><head><title>Gizlilik</title></head>`+
			`<body><h1>Gizlilik Politikamiz</h1><p>Kendi metnimiz.</p>`+
			`<div data-crucible-privacy data-title="Olcum aciklamasi"></div>`+
			`<p id="son">Bizim son paragrafimiz.</p>`+
			`<script src="/_ca/ca.js" data-site=%q></script></body></html>`, site)
	})

	// The same site's ordinary page, which has no anchor. This is every
	// other page on every site that embeds the snippet.
	mux.HandleFunc("/urun", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html lang="tr"><head><title>Urun</title></head>`+
			`<body><h1>Urun</h1><p>Bir sayfa.</p>`+
			`<script src="/_ca/ca.js" data-site=%q></script></body></html>`, site)
	})

	// The same anchor, with the script in <head> and no defer.
	//
	// This is the shape the DOMContentLoaded branch exists for, and it is
	// not hypothetical: a tag manager injects the snippet into the head,
	// and people copy the tag without the defer the README shows. The
	// script runs while the parser is still in <head>, so the anchor it
	// is looking for does not exist yet.
	//
	// Added after a mutation survived. Moving the fill out of the
	// DOMContentLoaded callback and calling it immediately changed
	// nothing in the two fixtures above, because in both of them the
	// script tag sits at the end of the body with the anchor already
	// parsed. The guard was real and the test could not see it.
	mux.HandleFunc("/basliktan", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html lang="tr"><head><title>Baslik</title>`+
			`<script src="/_ca/ca.js" data-site=%q></script></head>`+
			`<body><h1>Baslik</h1><div data-crucible-privacy></div></body></html>`, site)
	})

	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.WriteHeader(http.StatusNoContent)
	})

	// What the browser actually asked for, under a mutex for the reason
	// P1's test records: Chromium fetches page, script and frame on
	// separate connections, and net/http serves them on separate
	// goroutines.
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	defer server.Close()

	shot := filepath.Join(t.TempDir(), "cerceve.png")
	out, err := exec.Command("node", writeDisclosureScript(t), server.URL, shot).Output()
	if err != nil {
		t.Fatalf("browser run failed: %v", err)
	}
	t.Logf("browser transcript:\n%s", out)

	var report struct {
		ConsoleErrors []string `json:"console_errors"`

		// The page that asked for it.
		Frames     int      `json:"frames"`
		FrameURL   string   `json:"frame_url"`
		FrameTitle string   `json:"frame_title"`
		FrameBox   []int    `json:"frame_box"`
		SlotOrder  []string `json:"slot_order"`

		// The page that did not.
		PlainFrames int    `json:"plain_frames"`
		PlainBody   string `json:"plain_body"`

		// Loading the snippet twice must still draw one frame.
		FramesAfterSecondScript int `json:"frames_after_second_script"`

		// The page whose script runs before its own body exists.
		HeadFrames   int    `json:"head_frames"`
		HeadFrameURL string `json:"head_frame_url"`

		Threw string `json:"threw"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("browser report: %v\n%s", err, out)
	}
	if report.Threw != "" {
		t.Fatalf("the browser run threw: %s", report.Threw)
	}
	for _, e := range report.ConsoleErrors {
		t.Errorf("console error: %s", e)
	}

	// ---- the page that asked ----
	if report.Frames != 1 {
		t.Errorf("the anchored page ended with %d frames, want exactly 1", report.Frames)
	}
	if want := server.URL + DefaultPathPrefix + "/privacy.html"; report.FrameURL != want {
		t.Errorf("the frame loaded %q, want %q.\n"+
			"The URL is derived from the script's own src so a site that moved the "+
			"prefix needs no second setting; a wrong one here means every such site "+
			"embeds a 404", report.FrameURL, want)
	}
	if report.FrameTitle != "Olcum aciklamasi" {
		t.Errorf("the frame's accessible name is %q; the site set data-title and a "+
			"frame without a usable name is one a screen reader announces as "+
			"nothing", report.FrameTitle)
	}
	// Drawn, not merely present. A frame with no height is a frame the
	// visitor cannot read, and it would satisfy every assertion above.
	if len(report.FrameBox) != 2 || report.FrameBox[0] < 100 || report.FrameBox[1] < 100 {
		t.Errorf("the frame measures %v; a disclosure this size is not readable",
			report.FrameBox)
	}
	// In the site's own order, where the site put it. A widget that
	// appended itself to the end of the body would pass a count.
	want := []string{"H1", "P", "DIV", "P"}
	if strings.Join(report.SlotOrder, ",") != strings.Join(want, ",") {
		t.Errorf("the page's top-level elements are %v, want %v; the disclosure did "+
			"not stay where the site put it", report.SlotOrder, want)
	}

	// And there is ink in it.
	//
	// Everything above is satisfied by an empty box of the right size:
	// the element exists, it has a URL, it measures 1264 by 544. What a
	// visitor gets out of it is *painted text*, and the only way to ask
	// that question of a frame whose scripts are switched off is to look
	// at the pixels. The frame is sandboxed, so nothing can run inside
	// it to report on itself - which is the point of the sandbox and the
	// reason this assertion is a screenshot.
	if ink := darkPixels(t, shot); ink < 2000 {
		t.Errorf("the frame is drawn but nearly blank: %d dark pixels in %v.\n"+
			"The site embedded a box and the visitor reads nothing", ink, report.FrameBox)
	}

	// The server saw the frame fetch it. Without this the browser could
	// have drawn an empty box from cache or from nothing at all.
	mu.Lock()
	requests := strings.Join(seen, " ")
	mu.Unlock()
	if !strings.Contains(requests, "GET "+DefaultPathPrefix+"/privacy.html") {
		t.Errorf("the server was never asked for the disclosure page.\nSaw: %s", requests)
	}

	// ---- the page that did not ask ----
	if report.PlainFrames != 0 {
		t.Errorf("a page with no anchor ended up with %d frames.\n"+
			"The snippet is on every page of the site; drawing anything on a page "+
			"that did not ask puts our markup inside somebody's checkout",
			report.PlainFrames)
	}
	if body := strings.TrimSpace(report.PlainBody); body != "<h1>Urun</h1><p>Bir sayfa.</p>" {
		t.Errorf("a page with no anchor was modified. Its body is now:\n%s", body)
	}

	// ---- the script in <head>, before the anchor is parsed ----
	if report.HeadFrames != 1 {
		t.Errorf("a page whose script tag is in <head> ended with %d frames.\n"+
			"The script ran before the parser reached the anchor; the fill has to "+
			"wait for the document rather than run where the tag happens to be",
			report.HeadFrames)
	}
	if want := server.URL + DefaultPathPrefix + "/privacy.html"; report.HeadFrameURL != want {
		t.Errorf("the head-loaded page framed %q, want %q", report.HeadFrameURL, want)
	}

	// ---- the same snippet twice ----
	if report.FramesAfterSecondScript != 1 {
		t.Errorf("loading the snippet a second time left %d frames; two copies of a "+
			"script tag is a mistake, not a request for two disclosures",
			report.FramesAfterSecondScript)
	}
}

// darkPixels counts how much of the screenshot is text.
//
// Counted rather than compared against a reference image: a reference
// would have to be regenerated every time the page's wording or a
// browser's font rendering changed, and the question here is not "does
// it look like this" but "is anything there at all".
func darkPixels(t *testing.T, path string) int {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("the browser did not leave a screenshot: %v", err)
	}
	defer f.Close()

	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decoding the screenshot: %v", err)
	}

	dark := 0
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			// Rec. 601 luma, on the 16-bit values image/color returns.
			if (299*int(r)+587*int(g)+114*int(bl))/1000 < 0x8000 {
				dark++
			}
		}
	}
	t.Logf("the frame screenshot is %v with %d dark pixels", b.Size(), dark)
	return dark
}

// TestTheSurfaceCanBeTakenDownWithoutTakingTheOptOutWithIt.
//
// P3's completion criterion, in a browser, because both halves are about
// what a document does.
//
// With the surface off the anchor stays empty: the script asks the JSON
// endpoint first and draws nothing when the answer is 404. Drawing
// anyway would put a browser's own error page inside somebody's privacy
// policy, which is a worse answer than the nothing they asked for.
//
// And the opt-out still works, which is the half that is a promise
// rather than a nicety: *vazgeçme hakkı bizim bir sayfa servis ediyor
// olmamıza bağlanamaz*. The calls are pure localStorage and never touch
// the server, so this is a statement about coupling - the day somebody
// gates the script on the disclosure, this fails.
func TestTheSurfaceCanBeTakenDownWithoutTakingTheOptOutWithIt(t *testing.T) {
	if os.Getenv("CA_BROWSER_TEST") == "" {
		t.Skip("set CA_BROWSER_TEST=1 to run this; it needs node, playwright and a chromium build")
	}

	const site = "p3-tarayici"
	srv := &Server{
		Sites:     []string{site},
		Sink:      &fakeSink{},
		Visitors:  newTestVisitorIDs(t),
		IPMode:    privacy.IPMasked,
		IPHashKey: []byte("otuz-iki-baytlik-test-anahtari!!"),
	}
	srv.SetDisclosure(Disclosure{Enabled: false})

	mux := http.NewServeMux()
	mux.Handle("/", srv.Handler())
	mux.HandleFunc("/gizlilik", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html lang="tr"><head><title>Gizlilik</title></head>`+
			`<body><h1>Gizlilik Politikamiz</h1>`+
			`<div data-crucible-privacy></div>`+
			`<p id="son">Son paragraf.</p>`+
			`<script src="/_ca/ca.js" data-site=%q></script></body></html>`, site)
	})
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	out, err := exec.Command("node", writeSurfaceOffScript(t), server.URL).Output()
	if err != nil {
		t.Fatalf("browser run failed: %v", err)
	}
	t.Logf("browser transcript:\n%s", out)

	var report struct {
		ConsoleErrors []string `json:"console_errors"`
		Frames        int      `json:"frames"`
		AnchorHTML    string   `json:"anchor_html"`
		BodyHTML      string   `json:"body_html"`

		StatusFresh  string `json:"status_fresh"`
		StatusOut    string `json:"status_out"`
		StatusIn     string `json:"status_in"`
		OptOutStored bool   `json:"opt_out_stored"`

		Threw string `json:"threw"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("browser report: %v\n%s", err, out)
	}
	if report.Threw != "" {
		t.Fatalf("the browser run threw: %s", report.Threw)
	}

	// Chromium logs a failed resource load, and the failed resource is
	// the 404 this test is about. Everything else is a defect.
	for _, e := range report.ConsoleErrors {
		if strings.Contains(e, "404") {
			continue
		}
		t.Errorf("console error: %s", e)
	}

	if report.Frames != 0 {
		t.Errorf("the page drew %d frames with the disclosure switched off", report.Frames)
	}
	if strings.TrimSpace(report.AnchorHTML) != "" {
		t.Errorf("the anchor is not empty: %q", report.AnchorHTML)
	}
	if want := "<h1>Gizlilik Politikamiz</h1><div data-crucible-privacy=\"\"></div><p id=\"son\">Son paragraf.</p>"; report.BodyHTML != want {
		t.Errorf("the page was modified.\n got: %s\nwant: %s", report.BodyHTML, want)
	}

	// The opt-out, unaffected.
	if report.StatusFresh != "in" || report.StatusOut != "out" || report.StatusIn != "in" {
		t.Errorf("with the disclosure off the switch reads %q -> %q -> %q, want in -> out -> in",
			report.StatusFresh, report.StatusOut, report.StatusIn)
	}
	if !report.OptOutStored {
		t.Error("optOut() reported that it could not store the choice")
	}
}

func writeSurfaceOffScript(t *testing.T) string {
	t.Helper()

	const script = `
import playwright from '/opt/node22/lib/node_modules/playwright/index.js';
const { chromium } = playwright;

const [base] = process.argv.slice(2);

const browser = await chromium.launch({ executablePath: '/opt/pw-browsers/chromium' });
const report = { console_errors: [] };
const page = await browser.newPage({ viewport: { width: 900, height: 800 } });
page.on('console', (m) => { if (m.type() === 'error') report.console_errors.push(m.text()); });
page.on('pageerror', (e) => report.console_errors.push(String(e)));

try {

await page.goto(base + '/gizlilik');
// Long enough for the fetch and any frame it might have drawn: a test
// that looked before the answer arrived would report "nothing drawn"
// about a page that had not finished asking.
await page.waitForTimeout(800);

report.frames = await page.evaluate(() => document.querySelectorAll('iframe').length);
report.anchor_html = await page.evaluate(() =>
  document.querySelector('[data-crucible-privacy]').innerHTML);
report.body_html = await page.evaluate(() =>
  Array.from(document.body.children)
    .filter((e) => e.tagName !== 'SCRIPT')
    .map((e) => e.outerHTML)
    .join(''));

report.status_fresh = await page.evaluate(() => window.crucible.status());
report.opt_out_stored = await page.evaluate(() => window.crucible.optOut());
report.status_out = await page.evaluate(() => window.crucible.status());
await page.evaluate(() => window.crucible.optIn());
report.status_in = await page.evaluate(() => window.crucible.status());

} catch (e) {
  report.threw = String(e);
}

await browser.close();
process.stdout.write(JSON.stringify(report));
`

	prepared, err := browsertest.Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "surfaceoff.mjs")
	if err := os.WriteFile(path, []byte(prepared), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeDisclosureScript(t *testing.T) string {
	t.Helper()

	const script = `
import playwright from '/opt/node22/lib/node_modules/playwright/index.js';
const { chromium } = playwright;

const [base, shot] = process.argv.slice(2);

const browser = await chromium.launch({ executablePath: '/opt/pw-browsers/chromium' });
const report = { console_errors: [] };

const context = await browser.newContext();
const page = await context.newPage();
page.on('console', (m) => { if (m.type() === 'error') report.console_errors.push(m.text()); });
page.on('pageerror', (e) => report.console_errors.push(String(e)));

try {

// ---- the customer's privacy page ----
await page.goto(base + '/gizlilik');
// The frame is loading="lazy" and the anchor is above the fold, but the
// load still has to finish before its URL is the real one.
await page.waitForSelector('[data-crucible-privacy] iframe');
await page.waitForTimeout(400);

report.frames = await page.evaluate(() => document.querySelectorAll('iframe').length);
report.frame_url = await page.evaluate(() => document.querySelector('iframe').src);
report.frame_title = await page.evaluate(() => document.querySelector('iframe').title);
report.frame_box = await page.evaluate(() => {
  const r = document.querySelector('iframe').getBoundingClientRect();
  return [Math.round(r.width), Math.round(r.height)];
});
report.slot_order = await page.evaluate(() =>
  Array.from(document.body.children).filter((e) => e.tagName !== 'SCRIPT').map((e) => e.tagName));

// A picture of the frame as the visitor sees it. Taken from the parent
// page, which is the only vantage point there is: the frame runs no
// script of its own and cannot be asked what it is showing.
await page.locator('[data-crucible-privacy] iframe').screenshot({ path: shot });

// The same page again with a second copy of the snippet appended, which
// is what a site with two templates ends up with.
await page.evaluate((src) => {
  const s = document.createElement('script');
  s.src = src;
  s.setAttribute('data-site', 'p2-tarayici');
  document.body.appendChild(s);
}, base + '/_ca/ca.js');
await page.waitForTimeout(400);
report.frames_after_second_script =
  await page.evaluate(() => document.querySelectorAll('iframe').length);

// ---- the script in <head>, with no defer ----
await page.goto(base + '/basliktan');
await page.waitForTimeout(400);
report.head_frames = await page.evaluate(() => document.querySelectorAll('iframe').length);
report.head_frame_url = await page.evaluate(() => {
  const f = document.querySelector('iframe');
  return f ? f.src : '';
});

// ---- an ordinary page of the same site ----
await page.goto(base + '/urun');
await page.waitForTimeout(400);
report.plain_frames = await page.evaluate(() => document.querySelectorAll('iframe').length);
report.plain_body = await page.evaluate(() =>
  Array.from(document.body.children)
    .filter((e) => e.tagName !== 'SCRIPT')
    .map((e) => e.outerHTML)
    .join(''));

} catch (e) {
  report.threw = String(e);
}

await browser.close();
process.stdout.write(JSON.stringify(report));
`

	prepared, err := browsertest.Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "disclosure.mjs")
	if err := os.WriteFile(path, []byte(prepared), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
