//go:build integration

package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/browsertest"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// TestTheVisibleSetStepInABrowser drives the step a real installer uses.
//
// The Go tests decide what is stored. What a browser adds is the thing
// this step is: a form with a dozen checkboxes per site, filled in by
// somebody sitting with a customer. A box that does not toggle, a label
// that does not select its box, a form that posts the wrong field - none
// of those are visible from a ResponseRecorder, and all of them make the
// step useless while every assertion still passes.
func TestTheVisibleSetStepInABrowser(t *testing.T) {
	if os.Getenv("CA_BROWSER_TEST") == "" {
		t.Skip("set CA_BROWSER_TEST=1 to run this; it needs node, playwright and a chromium build")
	}

	srv, store := setupTestServer(t)
	if err := store.SetSetting(context.Background(), panel.KeyBeaconSites, "",
		[]string{visibilitySite}, nil); err != nil {
		t.Fatal(err)
	}
	setVisible(t, store, visibilitySite, []string{}, []string{})

	// A superadmin, because that is who opens this wizard: an owner is
	// sent to the technical door first.
	dev := makeUser(t, store, "c6-tarayici", true)
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	script := writeVisibilityScript(t)
	cmd := exec.Command("node", script,
		server.URL, dev.Email, testAccountPassword,
		SetupPathPrefix+"gorunum",
		visibilityField(cardFieldPrefix, visibilitySite),
		string(cardBotIPs),
	)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("browser run failed: %v", err)
	}
	t.Logf("browser transcript:\n%s", out)

	var report struct {
		CSPViolations []string `json:"csp_violations"`
		ConsoleErrors []string `json:"console_errors"`

		Boxes   int `json:"boxes"`
		Ticked  int `json:"ticked"`
		Legends int `json:"legends"`
		// LabelSelects is whether clicking the label text toggled the box,
		// which is the whole reason the input sits inside the label.
		LabelSelects bool `json:"label_selects"`
		// AfterSave is what remained ticked once the page came back.
		AfterSave           int  `json:"after_save"`
		SavedBoxStillTicked bool `json:"saved_box_still_ticked"`

		PageScrollsSideways bool `json:"page_scrolls_sideways"`

		LandedOn string `json:"landed_on"`
		Heading  string `json:"heading"`
		HTMLHead string `json:"html_head"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("browser report: %v\n%s", err, out)
	}

	if len(report.CSPViolations) > 0 {
		t.Errorf("Content-Security-Policy violations: %v", report.CSPViolations)
	}
	if len(report.ConsoleErrors) > 0 {
		t.Errorf("console errors: %v", report.ConsoleErrors)
	}

	if want := len(cards) + len(breakdownDefs); report.Boxes != want {
		t.Errorf("the step drew %d boxes for one site, want %d", report.Boxes, want)
	}
	if want := len(defaultCards) + len(defaultBreakdowns); report.Ticked != want {
		t.Errorf("%d boxes opened ticked, want the default %d", report.Ticked, want)
	}
	if report.Legends != 2 {
		t.Errorf("the step drew %d groups, want two - cards and breakdowns", report.Legends)
	}
	if !report.LabelSelects {
		t.Error("clicking a label does not toggle its box; the input is not inside the label")
	}
	if report.PageScrollsSideways {
		t.Error("the step scrolls horizontally")
	}

	// One box left ticked, everything else cleared, and the form saved.
	if report.AfterSave != 1 {
		t.Errorf("%d boxes are ticked after saving, want exactly the one that was left",
			report.AfterSave)
	}
	if !report.SavedBoxStillTicked {
		t.Error("the box that was left ticked came back unticked")
	}

	// And the database agrees with the screen.
	if got := storedSet(t, store, panel.KeyVisibleCards, visibilitySite); !slices.Equal(got,
		[]string{string(cardBotIPs)}) {
		t.Errorf("stored cards = %v, want only the box the browser left ticked", got)
	}
	if got := storedSet(t, store, panel.KeyVisibleBreakdowns, visibilitySite); !slices.Equal(got,
		[]string{ViewNone}) {
		t.Errorf("stored breakdowns = %v, want the reserved none", got)
	}
}

func writeVisibilityScript(t *testing.T) string {
	t.Helper()

	const script = `
import playwright from '/opt/node22/lib/node_modules/playwright/index.js';
const { chromium } = playwright;

const [base, email, password, stepPath, cardField, keepValue] = process.argv.slice(2);

const browser = await chromium.launch({ executablePath: '/opt/pw-browsers/chromium' });
const report = { csp_violations: [], console_errors: [] };

const page = await browser.newPage({ viewport: { width: 1280, height: 900 } });
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

// Everything below runs inside one try so a failed locator reports what
// the page actually was, instead of a stack trace that says only which
// selector timed out. Diagnosing a browser test through the selector
// alone is guesswork; the page is the evidence.
try {

// ---- sign in ----
await page.goto(base + '/giris');
await page.fill('#eposta', email);
await page.fill('#parola', password);
await page.click('main button[type=submit]');
await page.waitForLoadState();
await collectCSP();

// ---- the step ----
await page.goto(base + stepPath);
await page.waitForLoadState();
await collectCSP();
report.landed_on = new URL(page.url()).pathname;
report.heading = (await page.locator('h1').first().textContent().catch(() => '')) ?? '';

const boxes = page.locator('.gorunum input[type=checkbox]');
report.boxes = await boxes.count();
report.ticked = await page.locator('.gorunum input[type=checkbox]:checked').count();
report.legends = await page.locator('.gorunum legend').count();
report.page_scrolls_sideways = await page.evaluate(() =>
  document.documentElement.scrollWidth > document.documentElement.clientWidth + 1);

// Clicking the label text has to toggle the box. The input sits inside
// the label for exactly this, and a stray tag would break it silently.
const keep = page.locator('input[name="' + cardField + '"][value="' + keepValue + '"]');
if (await keep.count() === 0) {
  report.console_errors.push('the step drew no box named ' + cardField + '=' + keepValue);
  report.html_head = (await page.content()).slice(0, 1200);
  console.log(JSON.stringify(report, null, 2));
  await browser.close();
  process.exit(0);
}
const before = await keep.isChecked();
await keep.locator('xpath=ancestor::label[1]').click();
report.label_selects = (await keep.isChecked()) !== before;

// ---- leave one box ticked and save ----
const n = await boxes.count();
for (let i = 0; i < n; i++) {
  const box = boxes.nth(i);
  if (await box.isChecked()) await box.uncheck();
}
await keep.check();
// Scoped to the step's own form. The chrome's sign-out is also a
// form with a submit button and it comes first in the document, so a
// bare 'form button[type=submit]' clicks Çıkış - which is exactly what
// the first run of this test did, landing on /giris with nothing saved.
await page.click('main form button[type=submit]');
await page.waitForLoadState();
await collectCSP();

report.after_save = await page.locator('.gorunum input[type=checkbox]:checked').count();
report.saved_box_still_ticked = await keep.isChecked();

} catch (e) {
  report.console_errors.push('script: ' + String(e).split('\n')[0]);
  report.landed_on = new URL(page.url()).pathname;
  report.html_head = (await page.content()).slice(0, 1500);
}

console.log(JSON.stringify(report, null, 2));
await browser.close();
`
	dir := t.TempDir()
	name := filepath.Join(dir, "gorunum.mjs")
	ready, err := browsertest.Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(ready), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}
