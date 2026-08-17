//go:build integration

package web

import (
	"context"
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

	cats, err := ui.LoadCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	assets, err := ui.LoadAssets()
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := ui.New(cats, assets, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	renderer.Version = "tarayici-testi"

	srv := &Server{
		Renderer: renderer,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Zone:     time.UTC,
		Language: "tr",
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	// A second mount with no configured language, so the browser's own
	// preference decides. Two servers rather than one because the first
	// set must stay Turkish regardless of what locale this container's
	// Chromium happens to default to - a test whose language depends on
	// the machine it runs on is a test that will one day be wrong for a
	// reason nobody can reproduce.
	browserChooses := &Server{
		Renderer: renderer,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Zone:     time.UTC,
	}
	openServer := httptest.NewServer(browserChooses.Handler())
	defer openServer.Close()

	script := writeBrowserScript(t)
	cmd := exec.Command("node", script, server.URL, assets.URL("panel.css"), openServer.URL)
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

		EnglishLang    string `json:"english_lang"`
		EnglishDir     string `json:"english_dir"`
		EnglishBody    string `json:"english_body"`
		EnglishHeading string `json:"english_heading"`
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

	// A second browser, with a second language, against the same server.
	// The Go tests prove the strings are picked; only a real browser
	// proves the document a reader receives actually declares the
	// language it is written in - which is what a screen reader and an
	// automatic translator both act on.
	if report.EnglishLang != "en" {
		t.Errorf("an English browser received a document declaring lang=%q", report.EnglishLang)
	}
	if report.EnglishDir != "ltr" {
		t.Errorf("dir = %q", report.EnglishDir)
	}
	if !strings.Contains(report.EnglishHeading, "Sign in") {
		t.Errorf("the English heading reads %q", report.EnglishHeading)
	}
	if !strings.Contains(report.EnglishBody, "Sign in to continue") {
		t.Errorf("the English page reads %q", report.EnglishBody)
	}
}

func writeBrowserScript(t *testing.T) string {
	t.Helper()

	const script = `
import playwright from '/opt/node22/lib/node_modules/playwright/index.js';
const { chromium } = playwright;

const base = process.argv[2];
const cssPath = process.argv[3];
const openBase = process.argv[4];
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

// ---- a second language, negotiated by a second browser ----
const english = await browser.newContext({ locale: 'en-GB' });
const enPage = await english.newPage();
await enPage.goto(openBase + '/', { waitUntil: 'load' });
report.english_lang = await enPage.locator('html').getAttribute('lang');
report.english_dir = await enPage.locator('html').getAttribute('dir');
report.english_heading = (await enPage.locator('h1').innerText()).trim();
report.english_body = (await enPage.locator('main').innerText()).trim();

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

// A real browser walking the setup wizard, from the first-run page to
// the final check.
//
// The Go tests prove the flow answers correctly. What only a browser
// proves is that the pages are *operable*: that the forms submit, that
// the progress list is navigable, that the password field is a password
// field, and that nothing the wizard renders trips the panel's
// Content-Security-Policy - which it silently would, since the wizard
// is the first part of the panel with real forms in it.
//
//	CA_BROWSER_TEST=1 go test -tags integration ./internal/panel/web/ \
//	    -run TestSetupWizardInABrowser -v
func TestSetupWizardInABrowser(t *testing.T) {
	if os.Getenv("CA_BROWSER_TEST") == "" {
		t.Skip("set CA_BROWSER_TEST=1 to run this; it needs node, playwright and a chromium build")
	}

	srv, store := setupTestServer(t)
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	token, req, err := store.RequestDevAccess(context.Background(), testReasonPrefix+"tarayici", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !req.AutoApproved {
		t.Skip("this database already has accounts, so the link needs an owner's approval")
	}

	script := writeWizardScript(t)
	cmd := exec.Command("node", script, server.URL, token)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("browser run failed: %v", err)
	}
	t.Logf("browser transcript:\n%s", out)

	var report struct {
		CSPViolations []string `json:"csp_violations"`
		ConsoleErrors []string `json:"console_errors"`

		FirstRunHeading string `json:"first_run_heading"`
		FirstRunCommand string `json:"first_run_command"`
		BlockedHeading  string `json:"blocked_heading"`

		AfterRedeem  string   `json:"after_redeem"`
		WarningShown bool     `json:"warning_shown"`
		StepTitles   []string `json:"step_titles"`
		StepsVisited []string `json:"steps_visited"`

		SavedMessage string `json:"saved_message"`
		SitesAfter   string `json:"sites_after"`

		PasswordFieldType string `json:"password_field_type"`
		ChecksBefore      int    `json:"checks_before"`
		ChecksAfter       int    `json:"checks_after"`
		ManualRows        int    `json:"manual_rows"`
	}
	last := strings.TrimSpace(string(out))
	if i := strings.LastIndex(last, "\n"); i >= 0 {
		last = last[i+1:]
	}
	if err := json.Unmarshal([]byte(last), &report); err != nil {
		t.Fatalf("could not read the browser's report %q: %v", last, err)
	}

	if len(report.CSPViolations) > 0 {
		t.Errorf("the wizard trips the CSP: %v", report.CSPViolations)
	}
	if len(report.ConsoleErrors) > 0 {
		t.Errorf("the console is not clean: %v", report.ConsoleErrors)
	}

	// The door, before the link is used.
	if !strings.Contains(report.BlockedHeading, "Kurulum bekleniyor") {
		t.Errorf("the wizard was not refused without a session: %q", report.BlockedHeading)
	}
	if !strings.Contains(report.FirstRunCommand, "-dev-link") {
		t.Errorf("the refusal page does not print the command: %q", report.FirstRunCommand)
	}
	if report.FirstRunHeading == "" {
		t.Error("the front page rendered no heading")
	}

	// The link opens the wizard at its first step.
	if !strings.HasSuffix(report.AfterRedeem, SetupPathPrefix+"baslangic") {
		t.Errorf("redeeming landed on %q", report.AfterRedeem)
	}
	if !report.WarningShown {
		t.Error("the technical-wizard warning is not on the page")
	}
	if len(report.StepTitles) != len(wizardSteps) {
		t.Errorf("the progress list shows %d steps, want %d: %v",
			len(report.StepTitles), len(wizardSteps), report.StepTitles)
	}
	for i, visited := range report.StepsVisited {
		if i < len(wizardSteps) && !strings.HasSuffix(visited, wizardSteps[i].ID) {
			t.Errorf("step %d was %q, want %s", i, visited, wizardSteps[i].ID)
		}
	}

	// A real form submission writes a real setting.
	if !strings.Contains(report.SavedMessage, "Kaydedildi") {
		t.Errorf("saving the sites reported %q", report.SavedMessage)
	}
	// The value coming back on a freshly loaded page *is* the proof it
	// reached the database: the field is filled from GetSetting, on the
	// server, after the redirect.
	//
	// Deliberately not re-read here with a second query. This database is
	// shared with the panel package's suite, which runs in parallel and
	// writes the same global settings table, so a read from this side
	// after the browser subprocess exits would be asserting on a value
	// another package is entitled to change. TestSetupFlow makes the
	// direct database assertion, in the same goroutine as the write.
	if !strings.Contains(report.SitesAfter, "tarayici-sitesi") {
		t.Errorf("the saved sites did not come back from the store: %q", report.SitesAfter)
	}

	// The password field must be a password field: this page is filled
	// in over somebody's shoulder at a rack more often than anywhere
	// else in the panel.
	if report.PasswordFieldType != "password" {
		t.Errorf("the developer password field is type %q", report.PasswordFieldType)
	}

	// The final check runs on the button, not on page load.
	if report.ChecksBefore != 0 {
		t.Errorf("%d check rows appeared before anybody pressed the button", report.ChecksBefore)
	}
	if report.ChecksAfter == 0 {
		t.Error("pressing the button produced no check rows")
	}
	if report.ManualRows == 0 {
		t.Error("the manual-step list is empty")
	}
}

func writeWizardScript(t *testing.T) string {
	t.Helper()

	const script = `
import playwright from '/opt/node22/lib/node_modules/playwright/index.js';
const { chromium } = playwright;

const base = process.argv[2];
const token = process.argv[3];

const browser = await chromium.launch({ executablePath: '/opt/pw-browsers/chromium' });
const report = { csp_violations: [], console_errors: [], step_titles: [], steps_visited: [] };

const page = await browser.newPage();
// Console output is collected only while on pages that are supposed to
// succeed. Two navigations below are deliberate refusals, and Chromium
// logs a failed load for each; asserting on those would mean asserting
// that the refusal does not work.
let collecting = true;
page.on('console', (m) => { if (collecting && m.type() === 'error') report.console_errors.push(m.text()); });
page.on('pageerror', (e) => { if (collecting) report.console_errors.push(String(e)); });
await page.addInitScript(() => {
  window.__csp = [];
  document.addEventListener('securitypolicyviolation', (e) => {
    window.__csp.push(e.violatedDirective + ' ' + e.blockedURI);
  });
});
const collectCSP = async () => {
  for (const v of await page.evaluate(() => window.__csp ?? [])) report.csp_violations.push(v);
};

// ---- the front page ----
//
// Which page this is depends on whether any account exists, and this
// database is shared with another suite that creates them. So the front
// page is only checked for rendering at all; the first-run wording is
// asserted where it can be controlled, in the Go test.
await page.goto(base + '/', { waitUntil: 'load' });
report.first_run_heading = (await page.locator('h1').innerText()).trim();

// ---- the wizard, before the link is used: a deliberate refusal ----
//
// This page does not depend on the account count: with no developer
// session the wizard is shut either way, and it is where the command an
// installer needs is printed.
collecting = false;
await page.goto(base + '/kurulum/baslangic', { waitUntil: 'load' });
report.blocked_heading = (await page.locator('h1').innerText()).trim();
report.first_run_command = (await page.locator('pre code').innerText()).trim();
await collectCSP();
collecting = true;

// ---- redeem ----
await page.goto(base + '/gelistirici/' + token, { waitUntil: 'load' });
report.after_redeem = page.url();
report.warning_shown = (await page.locator('.uyari').count()) > 0;
report.step_titles = await page.locator('.ilerleme li').allInnerTexts();
await collectCSP();

// ---- walk every step by clicking "next" ----
report.steps_visited.push(page.url());
for (;;) {
  const next = page.locator('.kurulum-gezinme a.dugme');
  if (await next.count() === 0) break;
  await next.click();
  await page.waitForLoadState('load');
  report.steps_visited.push(page.url());
  await collectCSP();
}

// ---- fill in the sites form for real ----
//
// Scoped to main. The chrome carries a sign-out form now, and its
// button comes first in the document - a bare button[type=submit]
// selector clicks that one and ends the session instead of saving.
await page.goto(base + '/kurulum/siteler', { waitUntil: 'load' });
await page.fill('#siteler', 'tarayici-sitesi');
await page.click('main button[type="submit"]');
await page.waitForLoadState('load');
report.saved_message = (await page.locator('.bilgi').allInnerTexts()).join(' ').trim();
report.sites_after = await page.inputValue('#siteler');
await collectCSP();

// ---- the retention step's password field ----
await page.goto(base + '/kurulum/saklama', { waitUntil: 'load' });
report.password_field_type = await page.getAttribute('#developer_password', 'type');
await collectCSP();

// ---- the final check runs on the button, not on load ----
await page.goto(base + '/kurulum/kontrol', { waitUntil: 'load' });
report.checks_before = await page.locator('.durum').count();
await page.click('main button[type="submit"]');
await page.waitForLoadState('load');
report.checks_after = await page.locator('.durum').count();
// The manual list is the last table on the page.
const tables = page.locator('table');
report.manual_rows = await tables.nth(await tables.count() - 1).locator('tbody tr').count();
await collectCSP();

await browser.close();
console.log(JSON.stringify(report));
`
	path := filepath.Join(t.TempDir(), "wizard-browser.mjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
