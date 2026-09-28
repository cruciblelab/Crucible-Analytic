//go:build integration

package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/browsertest"
)

// TestTheDiagnosticFileDownloadsInABrowser clicks the Health page's
// button in real Chromium and reads what the browser saved.
//
// The Go tests prove the handler's bytes. What a browser adds is whether
// the click is a download at all - a link the browser opened as a page
// would show the owner a wall of JSON instead of handing them a file to
// send - and whether the name it saves under is the one the header sets.
func TestTheDiagnosticFileDownloadsInABrowser(t *testing.T) {
	if os.Getenv("CA_BROWSER_TEST") == "" {
		t.Skip("set CA_BROWSER_TEST=1 to run this; it needs node, playwright and a chromium build")
	}

	srv, store := setupTestServer(t)
	server, _, owner := signedInOwner(t, srv, store, healthSite, "tani-tarayici-sahip")

	script := writeDiagnosticScript(t)
	cmd := exec.Command("node", script, server.URL, owner.Email, testAccountPassword,
		HealthPath, DiagnosticPath, os.Getenv("CA_SHOT_DIR"))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("browser run failed: %v", err)
	}
	t.Logf("browser transcript:\n%s", out)

	var report struct {
		CSPViolations []string `json:"csp_violations"`
		ConsoleErrors []string `json:"console_errors"`
		// Buttons is how many links on the page lead to the file.
		Buttons int `json:"buttons"`
		// Downloaded is whether the click produced a download at all.
		Downloaded bool   `json:"downloaded"`
		Filename   string `json:"filename"`
		// Stayed is the page's address after the click: a download does
		// not navigate.
		Stayed string `json:"stayed"`
		// File is what the browser saved, parsed.
		File struct {
			Format   string          `json:"format"`
			Services json.RawMessage `json:"services"`
			Settings json.RawMessage `json:"settings"`
			Logs     struct {
				Scope string            `json:"scope"`
				Lines []json.RawMessage `json:"lines"`
			} `json:"logs"`
		} `json:"file"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("the browser report is not JSON: %v\n%s", err, out)
	}

	if len(report.CSPViolations) > 0 {
		t.Errorf("CSP violations: %v", report.CSPViolations)
	}
	if len(report.ConsoleErrors) > 0 {
		t.Errorf("console errors: %v", report.ConsoleErrors)
	}
	if report.Buttons != 1 {
		t.Fatalf("the Health page has %d links to the diagnostic file, want 1", report.Buttons)
	}
	if !report.Downloaded {
		t.Fatal("clicking the button produced no download; the browser treated the file as a page")
	}
	if !strings.HasPrefix(report.Filename, "crucible-tani-") || !strings.HasSuffix(report.Filename, ".json") {
		t.Errorf("the browser saved the file as %q", report.Filename)
	}
	if !strings.HasSuffix(report.Stayed, HealthPath) {
		t.Errorf("after the click the page is at %q; a download should leave it where it was", report.Stayed)
	}
	if report.File.Format != diagnosticFormat || len(report.File.Services) == 0 || len(report.File.Settings) == 0 {
		t.Errorf("the saved file is not the diagnostic file: format %q", report.File.Format)
	}
	// The log section is there, scoped to the owner who clicked. Its
	// lines are the integration test's business; here it is whether the
	// file a browser saves carries the section at all.
	if report.File.Logs.Scope != "owned" || report.File.Logs.Lines == nil {
		t.Errorf("the saved file's log section: scope %q, lines %v", report.File.Logs.Scope, report.File.Logs.Lines)
	}
}

func writeDiagnosticScript(t *testing.T) string {
	t.Helper()

	const script = `
import playwright from '/opt/node22/lib/node_modules/playwright/index.js';
import { readFile } from 'node:fs/promises';
const { chromium } = playwright;

const [base, email, password, healthPath, filePath, shotDir] = process.argv.slice(2);

const browser = await chromium.launch({ executablePath: '/opt/pw-browsers/chromium' });
const report = { csp_violations: [], console_errors: [] };

const context = await browser.newContext({ acceptDownloads: true, viewport: { width: 1200, height: 900 } });
const page = await context.newPage();
page.on('console', (m) => { if (m.type() === 'error') report.console_errors.push(m.text()); });
page.on('pageerror', (e) => report.console_errors.push(String(e)));
await page.addInitScript(() => {
  window.__csp = [];
  document.addEventListener('securitypolicyviolation', (e) => {
    window.__csp.push(e.violatedDirective + ' ' + e.blockedURI);
  });
});
const collectCSP = async () => {
  for (const v of await page.evaluate(() => window.__csp ?? [])) report.csp_violations.push(v);
};

await page.goto(base + '/giris');
await page.fill('#eposta', email);
await page.fill('#parola', password);
await page.click('main button[type=submit]');
await page.waitForLoadState();

await page.goto(base + healthPath);
await page.waitForLoadState();
await collectCSP();
if (shotDir) {
  await page.screenshot({ path: shotDir + '/tani-dugmesi.png', clip: { x: 0, y: 0, width: 1200, height: 420 } });
}

const button = page.locator('a[href="' + filePath + '"]');
report.buttons = await button.count();
if (report.buttons === 1) {
  const [download] = await Promise.all([
    page.waitForEvent('download', { timeout: 15000 }).catch(() => null),
    button.click(),
  ]);
  if (download) {
    report.downloaded = true;
    report.filename = download.suggestedFilename();
    report.file = JSON.parse(await readFile(await download.path(), 'utf8'));
  }
  report.stayed = page.url();
}
await collectCSP();

console.log(JSON.stringify(report, null, 2));
await browser.close();
`
	dir := t.TempDir()
	name := filepath.Join(dir, "tani.mjs")
	ready, err := browsertest.Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(ready), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}
