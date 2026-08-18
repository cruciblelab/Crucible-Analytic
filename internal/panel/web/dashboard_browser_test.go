//go:build integration

package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
)

// TestDashboardInABrowser drives the site page in real Chromium.
//
// The Go tests decide what each card says. What a browser adds is what
// this page is: a layout. Six cards that all collapse into one column on
// a laptop, or a value that overflows its card, is a defect no
// ResponseRecorder can see - the bytes are correct and the page is
// unusable.
//
// It also checks the range picker as a *reader* meets it: four links,
// one of them not a link because it is the one you are on, and clicking
// another actually changes the period shown.
func TestDashboardInABrowser(t *testing.T) {
	if os.Getenv("CA_BROWSER_TEST") == "" {
		t.Skip("set CA_BROWSER_TEST=1 to run this; it needs node, playwright and a chromium build")
	}

	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "tarayici-pano"

	base, token := analyticsAPI(t)
	client, err := analytics.New(base, token)
	if err != nil {
		t.Fatal(err)
	}
	srv.Analytics = client
	seedTraffic(t, site, time.Now().Add(-2*time.Hour), 8)

	owner := makeUser(t, store, "tarayici-pano-sahip", false)
	if err := store.AddMember(ctx, site, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	script := writeDashboardScript(t)
	cmd := exec.Command("node", script, server.URL, owner.Email, testAccountPassword, sitePath(site))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("browser run failed: %v", err)
	}
	t.Logf("browser transcript:\n%s", out)

	var report struct {
		CSPViolations []string `json:"csp_violations"`
		ConsoleErrors []string `json:"console_errors"`

		// ReachedFromSiteList is where clicking the site's name in the
		// list actually went. A dashboard nobody can navigate to is a
		// dashboard nobody sees.
		ReachedFromSiteList string `json:"reached_from_site_list"`

		Cards int `json:"cards"`
		// CardsWithNumbers and CardsExplained must add up to Cards: every
		// card either shows a figure or says why it does not, and a card
		// showing neither is a blank box on the page somebody paid for.
		CardsWithNumbers int `json:"cards_with_numbers"`
		CardsExplained   int `json:"cards_explained"`

		// Columns is how many cards share the top row at desktop width.
		// One would mean the grid collapsed.
		Columns int `json:"columns"`
		// Overflowing counts cards whose content is wider than the card.
		Overflowing int `json:"overflowing"`
		// PageScrollsSideways is the whole-page version of the same
		// question.
		PageScrollsSideways bool `json:"page_scrolls_sideways"`

		RangeLinks      int    `json:"range_links"`
		RangeCurrent    int    `json:"range_current"`
		AfterRangeClick string `json:"after_range_click"`
		RangeShown      string `json:"range_shown"`
		RangeShownAfter string `json:"range_shown_after"`

		// MobileColumns is the same grid at a phone width. One column is
		// the right answer there.
		MobileColumns int `json:"mobile_columns"`
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

	if report.ReachedFromSiteList != sitePath(site) {
		t.Errorf("the site's name in the list led to %q, not its dashboard",
			report.ReachedFromSiteList)
	}
	if report.Cards != len(defaultCards) {
		t.Errorf("the page drew %d cards, want %d", report.Cards, len(defaultCards))
	}
	if got := report.CardsWithNumbers + report.CardsExplained; got != report.Cards {
		t.Errorf("%d of %d cards show neither a figure nor a reason; a blank box "+
			"tells the reader nothing at all", report.Cards-got, report.Cards)
	}
	if report.CardsWithNumbers == 0 {
		t.Error("no card carries a number, on a site with real rows behind it")
	}

	if report.Columns < 2 {
		t.Errorf("the cards form %d column(s) at desktop width; the grid collapsed",
			report.Columns)
	}
	if report.MobileColumns != 1 {
		t.Errorf("the cards form %d columns at phone width, want 1", report.MobileColumns)
	}
	if report.Overflowing > 0 {
		t.Errorf("%d card(s) have content wider than the card", report.Overflowing)
	}
	if report.PageScrollsSideways {
		t.Error("the page scrolls horizontally")
	}

	if report.RangeLinks != len(rangeDays)-1 {
		t.Errorf("the picker offers %d links; with %d periods and one of them current, "+
			"it should offer %d", report.RangeLinks, len(rangeDays), len(rangeDays)-1)
	}
	if report.RangeCurrent != 1 {
		t.Errorf("%d periods are marked current; exactly one is", report.RangeCurrent)
	}
	if !strings.Contains(report.AfterRangeClick, "gun=") {
		t.Errorf("clicking a period went to %q, which carries no period",
			report.AfterRangeClick)
	}
	// The dates under the picker have to change with it. A picker that
	// navigates and shows the same period is worse than none.
	if report.RangeShown == "" || report.RangeShown == report.RangeShownAfter {
		t.Errorf("the dates did not change with the period: %q then %q",
			report.RangeShown, report.RangeShownAfter)
	}
}

func writeDashboardScript(t *testing.T) string {
	t.Helper()

	const script = `
import playwright from '/opt/node22/lib/node_modules/playwright/index.js';
const { chromium } = playwright;

const [base, email, password, sitePath] = process.argv.slice(2);

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
const path = () => new URL(page.url()).pathname;

// ---- sign in ----
await page.goto(base + '/giris');
await page.fill('#eposta', email);
await page.fill('#parola', password);
await page.click('main button[type=submit]');
await page.waitForLoadState();
await collectCSP();

// ---- the site list leads to the dashboard ----
await page.click('table.liste a[href="' + sitePath + '"]');
await page.waitForLoadState();
report.reached_from_site_list = path();
await collectCSP();

// ---- the cards ----
report.cards = await page.locator('.kartlar .kart').count();
report.cards_with_numbers = await page.locator('.kartlar .kart .kart-deger').count();
report.cards_explained = await page.locator('.kartlar .kart .kart-bos').count();

// How many cards share the topmost row: distinct offsetTop values tell
// us the grid did not collapse into one column.
const columnsAt = async () => page.evaluate(() => {
  const cards = [...document.querySelectorAll('.kartlar .kart')];
  if (cards.length === 0) return 0;
  const top = Math.min(...cards.map((c) => c.offsetTop));
  return cards.filter((c) => c.offsetTop === top).length;
});
report.columns = await columnsAt();

report.overflowing = await page.evaluate(() =>
  [...document.querySelectorAll('.kartlar .kart')]
    .filter((c) => c.scrollWidth > c.clientWidth + 1).length);
report.page_scrolls_sideways = await page.evaluate(() =>
  document.documentElement.scrollWidth > document.documentElement.clientWidth + 1);

// ---- the period picker ----
report.range_links = await page.locator('.aralik a').count();
report.range_current = await page.locator('.aralik [aria-current="true"]').count();
report.range_shown = (await page.locator('.aralik .ipucu').textContent() ?? '').trim();

await page.locator('.aralik a').first().click();
await page.waitForLoadState();
report.after_range_click = page.url();
report.range_shown_after = (await page.locator('.aralik .ipucu').textContent() ?? '').trim();
await collectCSP();

// ---- and the same grid on a phone ----
await page.setViewportSize({ width: 390, height: 844 });
report.mobile_columns = await columnsAt();
await collectCSP();

console.log(JSON.stringify(report, null, 2));
await browser.close();
`
	dir := t.TempDir()
	name := filepath.Join(dir, "pano.mjs")
	if err := os.WriteFile(name, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}
