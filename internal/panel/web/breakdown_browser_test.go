//go:build integration

package web

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/browsertest"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
)

// TestBreakdownsInABrowser drives the sections and one detail page in
// real Chromium.
//
// The Go tests decide which rows appear and what a share divides by.
// What a browser adds is the thing these are: six tables. A path longer
// than its column, a table that pushes the page sideways on a phone, a
// pager link that goes nowhere - each is a defect no ResponseRecorder
// can see, because the bytes are correct and the page is unusable.
//
// Long paths are seeded on purpose. A fixture of "/" and "/fiyat" would
// pass a layout that breaks on the first real site.
func TestBreakdownsInABrowser(t *testing.T) {
	if os.Getenv("CA_BROWSER_TEST") == "" {
		t.Skip("set CA_BROWSER_TEST=1 to run this; it needs node, playwright and a chromium build")
	}

	srv, store := setupTestServer(t)
	const site = "tarayici-kirilim"
	withRealAPI(t, srv)

	rows := breakdownFixture()
	// More paths than one *detail page* holds, not merely more than a
	// section shows. The first run of this test seeded past sectionRows
	// and produced a detail page with no pager at all - so the pager was
	// asserted about and never clicked, which is the same as not testing
	// it. Each path is also long enough to test the column rather than
	// the fixture.
	long := "/urunler/kategori/" + strings.Repeat("uzun-yol-parcasi/", 6) + "sayfa"
	for i := range detailRows + 3 {
		rows = append(rows, beaconRow{
			visitor: "uzun", kind: "pageview",
			path:    long + "-" + strconv.Itoa(i),
			device:  "desktop",
			country: "TR",
		})
	}
	seedBeacon(t, site, time.Now().Add(-3*time.Hour), rows)

	server, _, owner := signedInOwner(t, srv, store, site, "tarayici-kirilim-sahip")

	script := writeBreakdownScript(t)
	cmd := exec.Command("node", script,
		server.URL, owner.Email, testAccountPassword,
		sitePath(site), breakdownPath(site, analytics.BreakdownPages))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("browser run failed: %v", err)
	}
	t.Logf("browser transcript:\n%s", out)

	var report struct {
		CSPViolations []string `json:"csp_violations"`
		ConsoleErrors []string `json:"console_errors"`

		Sections int `json:"sections"`
		// SectionsWithRows and SectionsExplained must add up to Sections:
		// every section either has a table or says why it does not.
		SectionsWithRows  int `json:"sections_with_rows"`
		SectionsExplained int `json:"sections_explained"`

		// Overflowing counts sections whose content is wider than the
		// section; PageScrollsSideways is the whole-page version.
		Overflowing         int  `json:"overflowing"`
		PageScrollsSideways bool `json:"page_scrolls_sideways"`

		// MoreLink is where the first "all N" link actually goes.
		MoreLink string `json:"more_link"`
		// ReachedFromMoreLink is where the browser ended up after
		// following it. A link that lands somewhere else is worse than no
		// link.
		ReachedFromMoreLink string `json:"reached_from_more_link"`
		DetailRows          int    `json:"detail_rows"`
		DetailHasPager      bool   `json:"detail_has_pager"`
		AfterNext           string `json:"after_next"`
		BackToSummary       string `json:"back_to_summary"`

		// The named group, styled as the different kind of thing it is.
		NamedRows int `json:"named_rows"`

		// The same tables at a phone width.
		MobileOverflowing         int  `json:"mobile_overflowing"`
		MobilePageScrollsSideways bool `json:"mobile_page_scrolls_sideways"`
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

	if report.Sections != len(defaultBreakdowns) {
		t.Errorf("the page drew %d sections, want %d", report.Sections, len(defaultBreakdowns))
	}
	if got := report.SectionsWithRows + report.SectionsExplained; got != report.Sections {
		t.Errorf("%d of %d sections show neither a table nor a reason",
			report.Sections-got, report.Sections)
	}
	if report.SectionsWithRows == 0 {
		t.Error("no section carries rows, on a site with real beacon events behind it")
	}

	// The layout, at both widths. A table of long paths is exactly what
	// pushes a page sideways, and a phone is where it matters most.
	if report.Overflowing > 0 {
		t.Errorf("%d section(s) have content wider than the section at desktop width",
			report.Overflowing)
	}
	if report.PageScrollsSideways {
		t.Error("the page scrolls horizontally at desktop width")
	}
	if report.MobilePageScrollsSideways {
		t.Error("the page scrolls horizontally at phone width; long paths pushed it out")
	}
	if report.MobileOverflowing > 0 {
		t.Errorf("%d section(s) overflow at phone width", report.MobileOverflowing)
	}

	if report.MoreLink == "" {
		t.Error("no section offers its full list, on a site with more rows than one section shows")
	}
	if report.ReachedFromMoreLink == "" || !strings.Contains(report.ReachedFromMoreLink, "/detay/") {
		t.Errorf("the full-list link led to %q", report.ReachedFromMoreLink)
	}
	if report.DetailRows == 0 {
		t.Error("the detail page drew no rows")
	}
	if report.DetailHasPager && report.AfterNext != "" && !strings.Contains(report.AfterNext, "sayfa=2") {
		t.Errorf("the next-page link went to %q", report.AfterNext)
	}
	if report.BackToSummary == "" || strings.Contains(report.BackToSummary, "/detay/") {
		t.Errorf("the way back from the detail page led to %q", report.BackToSummary)
	}
	if report.NamedRows == 0 {
		t.Error("the never-determined group is not styled as a named row; " +
			"the fixture contains direct visits, so one section must have it")
	}
}

func writeBreakdownScript(t *testing.T) string {
	t.Helper()

	const script = `
import playwright from '/opt/node22/lib/node_modules/playwright/index.js';
const { chromium } = playwright;

const [base, email, password, sitePath, detailPath] = process.argv.slice(2);

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

// Whether anything is wider than the box that is supposed to contain it.
// The tables are allowed to scroll inside .tablo-kaydir; the section
// around them is not.
const overflowing = async () => page.evaluate(() =>
  [...document.querySelectorAll('.kirilim')]
    .filter((s) => s.scrollWidth > s.clientWidth + 1).length);
const sideways = async () => page.evaluate(() =>
  document.documentElement.scrollWidth > document.documentElement.clientWidth + 1);

// ---- sign in ----
await page.goto(base + '/giris');
await page.fill('#eposta', email);
await page.fill('#parola', password);
await page.click('main button[type=submit]');
await page.waitForLoadState();
await collectCSP();

// ---- the site page's sections ----
await page.goto(base + sitePath);
await page.waitForLoadState();
await collectCSP();

report.sections = await page.locator('.kirilim').count();
report.sections_with_rows = await page.locator('.kirilim table.kirilim-tablo').count();
report.sections_explained = await page.locator('.kirilim .kirilim-bos').count();
report.named_rows = await page.locator('.kirilim-tablo th.ad.adsiz').count();
report.overflowing = await overflowing();
report.page_scrolls_sideways = await sideways();

// ---- the same tables on a phone ----
await page.setViewportSize({ width: 390, height: 844 });
report.mobile_overflowing = await overflowing();
report.mobile_page_scrolls_sideways = await sideways();
await page.setViewportSize({ width: 1280, height: 900 });

// ---- following a section to its own page ----
const more = page.locator('.kirilim a.tumu').first();
if (await more.count() > 0) {
  report.more_link = await more.getAttribute('href');
  await more.click();
  await page.waitForLoadState();
  report.reached_from_more_link = new URL(page.url()).pathname;
  await collectCSP();

  report.detail_rows = await page.locator('.kirilim-tablo tbody tr').count();
  const next = page.locator('.sayfalama a[rel=next]');
  report.detail_has_pager = await next.count() > 0;
  if (report.detail_has_pager) {
    await next.click();
    await page.waitForLoadState();
    report.after_next = page.url();
    await collectCSP();
  }
  const back = page.locator('.geri a').first();
  report.back_to_summary = await back.getAttribute('href');
}

// ---- and the detail page reached directly ----
await page.goto(base + detailPath);
await page.waitForLoadState();
await collectCSP();

console.log(JSON.stringify(report, null, 2));
await browser.close();
`
	dir := t.TempDir()
	name := filepath.Join(dir, "kirilim.mjs")
	ready, err := browsertest.Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(ready), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}
