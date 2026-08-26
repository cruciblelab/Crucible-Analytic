//go:build integration

package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/browsertest"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// TestDevAccessInABrowser drives the approval flow in real Chromium.
//
// The Go tests already prove the decisions. What a browser adds here is
// the part that is invisible from the server's side and matters most on
// this particular page: whether the owner actually **meets** the banner
// while they are somewhere else, whether the link in it goes anywhere,
// and whether the two forms on each pending request are two forms - a
// single form with two submit buttons would make "approve" depend on
// which button the browser chose to send.
//
// It also watches for Content-Security-Policy violations, which is the
// class of defect that has reached this project three times now with
// every HTTP-level test reporting a healthy 200.
func TestDevAccessInABrowser(t *testing.T) {
	if os.Getenv("CA_BROWSER_TEST") == "" {
		t.Skip("set CA_BROWSER_TEST=1 to run this; it needs node, playwright and a chromium build")
	}

	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "tarayici-erisim"

	owner := makeUser(t, store, "tarayici-erisim-sahip", false)
	if err := store.AddMember(ctx, site, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}

	// Two requests: one to approve in the browser, one to deny.
	approveToken, approveReq := requestAccess(t, store, "tarayici-onay")
	denyToken, denyReq := requestAccess(t, store, "tarayici-ret")

	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	script := writeDevAccessScript(t)
	cmd := exec.Command("node", script, server.URL,
		owner.Email, testAccountPassword, approveReq.Reason, denyReq.Reason)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("browser run failed: %v", err)
	}
	t.Logf("browser transcript:\n%s", out)

	var report struct {
		CSPViolations []string `json:"csp_violations"`
		ConsoleErrors []string `json:"console_errors"`

		// BannerOnOtherPage is the banner's title as seen from the
		// account page - somewhere the owner might be working when a
		// request arrives.
		BannerOnOtherPage string `json:"banner_on_other_page"`
		// BannerLandedOn is where the banner's own link actually went.
		BannerLandedOn string `json:"banner_landed_on"`
		// PendingForms counts the forms drawn per pending request. Two
		// is the answer: approve and deny are separate submissions.
		PendingForms int `json:"pending_forms"`
		// UnverifiedNotice is the sentence saying the panel checked
		// nothing about who asked. Empty means an owner is deciding
		// without being told that.
		UnverifiedNotice string `json:"unverified_notice"`

		AfterApprove   string `json:"after_approve"`
		AfterDeny      string `json:"after_deny"`
		BannerAfterAll string `json:"banner_after_all"`
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

	if report.BannerOnOtherPage == "" {
		t.Error("the owner does not meet the banner while working on another page, " +
			"which is the only reason it is in the shared chrome rather than on the landing page")
	}
	if report.BannerLandedOn != DevAccessRequestsPath {
		t.Errorf("the banner's link went to %q, not the page that decides it",
			report.BannerLandedOn)
	}
	if report.UnverifiedNotice == "" {
		t.Error("the page shows the reason without saying the panel verified nothing " +
			"about who wrote it")
	}
	if report.PendingForms != 2 {
		t.Errorf("a pending request draws %d forms; approve and deny must be separate "+
			"submissions, not two buttons whose meaning depends on which one the "+
			"browser sent", report.PendingForms)
	}

	// The decisions the browser made have to be true in the database,
	// not merely on the screen.
	if _, err := store.RedeemDevAccess(ctx, approveToken, netip.Addr{}); err != nil {
		t.Errorf("the link approved in the browser does not open: %v", err)
	}
	if _, err := store.RedeemDevAccess(ctx, denyToken, netip.Addr{}); err == nil {
		t.Error("the link denied in the browser still opens")
	}

	if report.BannerAfterAll != "" {
		t.Errorf("the banner survives both decisions: %q", report.BannerAfterAll)
	}
	if !strings.Contains(report.AfterApprove, "/") || !strings.Contains(report.AfterDeny, "/") {
		t.Errorf("deciding navigated somewhere unexpected: %q then %q",
			report.AfterApprove, report.AfterDeny)
	}
}

func writeDevAccessScript(t *testing.T) string {
	t.Helper()

	const script = `
import playwright from '/opt/node22/lib/node_modules/playwright/index.js';
const { chromium } = playwright;

const [base, ownerEmail, password, approveReason, denyReason] = process.argv.slice(2);

const browser = await chromium.launch({ executablePath: '/opt/pw-browsers/chromium' });
const report = { csp_violations: [], console_errors: [] };

const page = await browser.newPage();
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
await page.fill('#eposta', ownerEmail);
await page.fill('#parola', password);
await page.click('main button[type=submit]');
await page.waitForLoadState();
await collectCSP();

// ---- the banner, met somewhere else entirely ----
await page.goto(base + '/hesap');
const banner = page.locator('.uyari').filter({ has: page.locator('a[href="/erisim"]') }).first();
report.banner_on_other_page = (await banner.locator('.uyari-baslik').textContent() ?? '').trim();
await collectCSP();

// ---- and its link goes where it says ----
await banner.locator('a[href="/erisim"]').click();
await page.waitForLoadState();
report.banner_landed_on = path();
await collectCSP();

report.unverified_notice = (await page.locator('.kilit').first().textContent() ?? '').trim();

// Two forms on one pending card: approve and deny are separate
// submissions on purpose.
const card = page.locator('article.bekleyen').first();
report.pending_forms = await card.locator('form').count();

// ---- approve the first, deny the second, by their reasons ----
const cardFor = (reason) =>
  page.locator('article.bekleyen').filter({ hasText: reason }).first();

await cardFor(approveReason).locator('form:has(input[value="onayla"]) button').click();
await page.waitForLoadState();
report.after_approve = path();
await collectCSP();

await cardFor(denyReason).locator('form:has(input[value="reddet"]) button').click();
await page.waitForLoadState();
report.after_deny = path();
await collectCSP();

// ---- nothing is pending now, so the banner is gone ----
await page.goto(base + '/hesap');
const left = page.locator('.uyari').filter({ has: page.locator('a[href="/erisim"]') });
report.banner_after_all = (await left.count()) === 0
  ? ''
  : (await left.first().textContent() ?? '').trim();
await collectCSP();

console.log(JSON.stringify(report, null, 2));
await browser.close();
`
	dir := t.TempDir()
	name := filepath.Join(dir, "erisim.mjs")
	ready, err := browsertest.Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(ready), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}
