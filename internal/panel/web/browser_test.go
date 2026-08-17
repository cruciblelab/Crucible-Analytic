//go:build integration

package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// A real browser loading the real page, from the real handler tree.
//
// Everything below is a claim the Go tests cannot make. httptest.
// Recorder cannot tell whether Chromium refused the stylesheet under
// the Content-Security-Policy, whether htmx actually started, whether
// the Turkish text decoded, or whether the second load of an asset came
// from the cache. Each of those fails silently in a way that looks
// perfectly healthy from the server's side - the response was 200, the
// bytes were correct, and the page is unreadable.
//
//	CA_BROWSER_TEST=1 go test -tags integration ./internal/panel/web/ \
//	    -run TestPanelInABrowser -v
func TestPanelInABrowser(t *testing.T) {
	if os.Getenv("CA_BROWSER_TEST") == "" {
		t.Skip("set CA_BROWSER_TEST=1 to run this; it needs node, playwright and a chromium build")
	}

	cat, err := ui.LoadCatalog()
	if err != nil {
		t.Fatal(err)
	}
	assets, err := ui.LoadAssets()
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := ui.New(cat, assets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	renderer.Version = "tarayici-testi"

	srv := &Server{
		Renderer: renderer,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Zone:     time.UTC,
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	script := writeBrowserScript(t)
	cmd := exec.Command("node", script, server.URL, assets.URL("panel.css"))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("browser run failed: %v", err)
	}
	t.Logf("browser transcript:\n%s", out)

	var report struct {
		Title            string   `json:"title"`
		Lang             string   `json:"lang"`
		BodyBackground   string   `json:"body_background"`
		StylesheetLoaded bool     `json:"stylesheet_loaded"`
		HtmxLoaded       bool     `json:"htmx_loaded"`
		HtmxVersion      string   `json:"htmx_version"`
		CSPViolations    []string `json:"csp_violations"`
		ConsoleErrors    []string `json:"console_errors"`
		FailedRequests   []string `json:"failed_requests"`
		ExternalHosts    []string `json:"external_hosts"`
		SkipLinkText     string   `json:"skip_link_text"`
		SkipLinkFocused  bool     `json:"skip_link_focused"`
		FooterText       string   `json:"footer_text"`
		NotFoundStatus   int      `json:"not_found_status"`
		NotFoundHeading  string   `json:"not_found_heading"`
		NotFoundHasLink  bool     `json:"not_found_has_link"`
		AssetStatusFirst int      `json:"asset_status_first"`
		AssetStatusCond  int      `json:"asset_status_conditional"`
		AssetEncoding    string   `json:"asset_encoding"`
		DarkBackground   string   `json:"dark_background"`
	}
	last := strings.TrimSpace(string(out))
	if i := strings.LastIndex(last, "\n"); i >= 0 {
		last = last[i+1:]
	}
	if err := json.Unmarshal([]byte(last), &report); err != nil {
		t.Fatalf("could not read the browser's report %q: %v", last, err)
	}

	// The policy is the point of the whole stack choice. One violation
	// means a browser refused something the page needs, and the usual
	// "fix" is to weaken the policy for every page.
	if len(report.CSPViolations) > 0 {
		t.Errorf("the browser reported CSP violations: %v", report.CSPViolations)
	}
	if len(report.ConsoleErrors) > 0 {
		t.Errorf("the console is not clean: %v", report.ConsoleErrors)
	}
	if len(report.FailedRequests) > 0 {
		t.Errorf("some subresources did not load: %v", report.FailedRequests)
	}
	// The deployment promise: one binary, no third party.
	if len(report.ExternalHosts) > 0 {
		t.Errorf("the page fetched from other hosts: %v", report.ExternalHosts)
	}

	if !report.StylesheetLoaded {
		t.Error("the stylesheet did not apply; the page rendered unstyled")
	}
	// A default page has a transparent or white body. Ours does not,
	// which is the cheapest proof the CSS was parsed and not merely
	// downloaded.
	if report.BodyBackground == "" || report.BodyBackground == "rgba(0, 0, 0, 0)" {
		t.Errorf("body background is %q; the stylesheet was fetched but not applied", report.BodyBackground)
	}
	if report.DarkBackground == report.BodyBackground {
		t.Errorf("the dark scheme renders the same background (%q) as the light one", report.DarkBackground)
	}

	if !report.HtmxLoaded {
		t.Error("htmx did not start; every interactive page after this one depends on it")
	}
	if !strings.HasPrefix(report.HtmxVersion, "2.") {
		t.Errorf("htmx reports version %q, want the vendored 2.x", report.HtmxVersion)
	}

	if report.Lang != "tr" {
		t.Errorf("<html lang> is %q; a screen reader would read Turkish with the wrong voice", report.Lang)
	}
	if !strings.Contains(report.Title, "Crucible Analytic") {
		t.Errorf("title = %q", report.Title)
	}
	// Turkish letters outside ASCII are the ones a charset mistake
	// mangles, and "İçeriğe" carries three of them.
	if !strings.Contains(report.SkipLinkText, "İçeriğe atla") {
		t.Errorf("the skip link reads %q; the page is not decoding as UTF-8", report.SkipLinkText)
	}
	if !report.SkipLinkFocused {
		t.Error("the skip link cannot be reached with the keyboard")
	}
	if !strings.Contains(report.FooterText, "tarayici-testi") {
		t.Errorf("the footer does not carry the build version: %q", report.FooterText)
	}

	if report.NotFoundStatus != 404 {
		t.Errorf("an unknown path answered %d", report.NotFoundStatus)
	}
	if !strings.Contains(report.NotFoundHeading, "Sayfa bulunamadı") {
		t.Errorf("the 404 heading reads %q", report.NotFoundHeading)
	}
	if !report.NotFoundHasLink {
		t.Error("the 404 page leaves the reader with no way back")
	}

	if report.AssetStatusFirst != 200 {
		t.Errorf("the first asset request answered %d", report.AssetStatusFirst)
	}
	if report.AssetStatusCond != 304 {
		t.Errorf("a conditional asset request answered %d, want 304", report.AssetStatusCond)
	}
	if report.AssetEncoding != "gzip" {
		t.Errorf("the browser received the stylesheet as %q, want gzip", report.AssetEncoding)
	}
}

func writeBrowserScript(t *testing.T) string {
	t.Helper()

	const script = `
import playwright from '/opt/node22/lib/node_modules/playwright/index.js';
const { chromium } = playwright;

const base = process.argv[2];
const cssPath = process.argv[3];
const origin = new URL(base).host;

const browser = await chromium.launch({ executablePath: '/opt/pw-browsers/chromium' });
const report = {
  csp_violations: [], console_errors: [], failed_requests: [], external_hosts: [],
};

const page = await browser.newPage();

// Console output is only collected while the page under test is one
// that is supposed to succeed. The 404 navigation later on is itself a
// failed load, and Chromium logs it - asserting on that would mean
// asserting the error page does not work.
let collecting = true;
page.on('console', (m) => { if (collecting && m.type() === 'error') report.console_errors.push(m.text()); });
page.on('pageerror', (e) => { if (collecting) report.console_errors.push(String(e)); });
page.on('requestfailed', (r) => report.failed_requests.push(r.url() + ' ' + (r.failure()?.errorText ?? '')));
page.on('request', (r) => {
  const host = new URL(r.url()).host;
  if (host && host !== origin) report.external_hosts.push(r.url());
});

// A CSP refusal fires this event in the page. Collecting it is the only
// way to see, from outside, that the browser dropped something.
await page.addInitScript(() => {
  window.__cspViolations = [];
  document.addEventListener('securitypolicyviolation', (e) => {
    window.__cspViolations.push(e.violatedDirective + ' ' + e.blockedURI);
  });
});

await page.goto(base + '/', { waitUntil: 'load' });

report.title = await page.title();
report.lang = await page.locator('html').getAttribute('lang');
report.body_background = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
// The skip link is positioned far off-screen by the stylesheet; if the
// CSS did not apply it sits at 0.
report.stylesheet_loaded = await page.evaluate(() =>
  document.querySelector('.atla').getBoundingClientRect().left < -1000);
report.htmx_loaded = await page.evaluate(() => typeof window.htmx === 'object');
report.htmx_version = await page.evaluate(() => window.htmx?.version ?? '');
report.skip_link_text = (await page.locator('.atla').innerText()).trim();
report.footer_text = (await page.locator('footer').innerText()).trim();

// Tab once from the top: the skip link must be the first stop, because
// somebody on a keyboard should not have to walk the whole header.
await page.keyboard.press('Tab');
report.skip_link_focused = await page.evaluate(() =>
  document.activeElement?.classList.contains('atla') === true);

report.csp_violations = await page.evaluate(() => window.__cspViolations ?? []);

// ---- an unknown path ----
collecting = false;
const notFound = await page.goto(base + '/boyle-bir-sayfa-yok', { waitUntil: 'load' });
report.not_found_status = notFound.status();
report.not_found_heading = (await page.locator('h1').innerText()).trim();
report.not_found_has_link = (await page.locator('main a').count()) > 0;
for (const v of await page.evaluate(() => window.__cspViolations ?? [])) report.csp_violations.push(v);

// ---- the asset, twice ----
const first = await page.request.get(base + cssPath);
report.asset_status_first = first.status();
report.asset_encoding = (await page.evaluate(async (u) => {
  const r = await fetch(u, { headers: { 'Accept-Encoding': 'gzip' } });
  return r.headers.get('content-encoding') ?? '';
}, base + cssPath)) || (first.headers()['content-encoding'] ?? '');
const etag = first.headers()['etag'];
const second = await page.request.get(base + cssPath, { headers: { 'If-None-Match': etag } });
report.asset_status_conditional = second.status();

// ---- the dark scheme is a real second palette, not the same colours ----
const dark = await browser.newPage({ colorScheme: 'dark' });
await dark.goto(base + '/', { waitUntil: 'load' });
report.dark_background = await dark.evaluate(() => getComputedStyle(document.body).backgroundColor);

await browser.close();
console.log(JSON.stringify(report));
`
	path := filepath.Join(t.TempDir(), "panel-browser.mjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
