//go:build integration

package web

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/browsertest"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// TestTheOperatorsCodeWorksInABrowser is B3h in real Chromium, by clicks:
// the operator picks the member from the list, types the developer
// password and presses the button; the member, in another browser, finds
// the way from the sign-in page, uses the code without their phone, and
// ends up on the account page that tells them they have no codes left.
//
// The Go tests post the form's fields. What a browser adds is that the
// page's own select, password field and button produce that post, that
// the codes page shows the code where a person can read it, and that the
// sign-in page's link and the recovery form's checkbox are the ones the
// member can actually reach.
func TestTheOperatorsCodeWorksInABrowser(t *testing.T) {
	if os.Getenv("CA_BROWSER_TEST") == "" {
		t.Skip("set CA_BROWSER_TEST=1 to run this; it needs node, playwright and a chromium build")
	}

	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "kurtarma-tarayici"
	owner := makeUser(t, store, "kt-sahip", false)
	member := makeUser(t, store, "kt-uye", false)
	for _, m := range []struct {
		u    panel.User
		role panel.Role
	}{{owner, panel.RoleOwner}, {member, panel.RoleViewer}} {
		if err := store.AddMember(ctx, site, m.u.ID, m.role, panel.Grant{}); err != nil {
			t.Fatal(err)
		}
	}
	recoveryFor(t, store, member)
	enrolTOTP(t, store, member)
	running := httptest.NewServer(srv.Handler())
	t.Cleanup(running.Close)
	server := running.URL
	token, req := requestAccess(t, store, "kurtarma-tarayici")
	if err := store.ApproveDevAccess(ctx, req.ID, owner); err != nil {
		t.Fatal(err)
	}

	base := srv.Renderer.Catalogs().Base()
	script := writeRecoveryIssueScript(t)
	cmd := exec.Command("node", script, server, DevAccessPathPrefix+token, memberPath(site),
		member.Email, testDevPassword, LoginPath, RecoveryPath, base.T("kurtarma.kalan_yok"),
		os.Getenv("CA_SHOT_DIR"))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("browser run failed: %v", err)
	}
	t.Logf("browser transcript:\n%s", out)

	var report struct {
		CSPViolations []string `json:"csp_violations"`
		ConsoleErrors []string `json:"console_errors"`
		// Offered is whether the member list drew the section's form.
		Offered bool `json:"offered"`
		// Preselected is the list's value before anybody chose.
		Preselected string `json:"preselected"`
		// Codes are the codes the page showed after the click.
		Codes []string `json:"codes"`
		// RecoveryLink is whether the sign-in page led to the form.
		RecoveryLink bool `json:"recovery_link"`
		// Landed is where the member ended up after the recovery form.
		Landed string `json:"landed"`
		// ToldNoCodes is whether that page said they have none left.
		ToldNoCodes bool `json:"told_no_codes"`
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
	if !report.Offered {
		t.Fatal("the operator's member list drew no recovery form")
	}
	if report.Preselected != "" {
		t.Errorf("the list opened with %q chosen; nobody should be until somebody is", report.Preselected)
	}
	if len(report.Codes) != 1 {
		t.Fatalf("the codes page showed %d codes, want one", len(report.Codes))
	}
	if !report.RecoveryLink {
		t.Error("the sign-in page does not lead to the recovery form")
	}
	if report.Landed != server+AccountPath {
		t.Errorf("the member landed on %q, want the account page", report.Landed)
	}
	if !report.ToldNoCodes {
		t.Error("the account page does not tell the member they have no codes left")
	}
	after, err := store.UserByID(ctx, member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.HasTOTP() {
		t.Error("the second factor is still on after the member ticked that they cannot reach the phone")
	}
}

func writeRecoveryIssueScript(t *testing.T) string {
	t.Helper()

	const script = `
import playwright from '/opt/node22/lib/node_modules/playwright/index.js';
const { chromium } = playwright;

const [base, devLink, membersPath, memberEmail, devPassword, loginPath, recoveryPath, noCodes, shotDir] =
  process.argv.slice(2);

const browser = await chromium.launch({ executablePath: '/opt/pw-browsers/chromium' });
const report = { csp_violations: [], console_errors: [] };

const open = async () => {
  const context = await browser.newContext({ viewport: { width: 1100, height: 900 }, deviceScaleFactor: 1 });
  const page = await context.newPage();
  page.on('console', (m) => { if (m.type() === 'error') report.console_errors.push(m.text()); });
  page.on('pageerror', (e) => report.console_errors.push(String(e)));
  await page.addInitScript(() => {
    window.__csp = [];
    document.addEventListener('securitypolicyviolation', (e) => {
      window.__csp.push(e.violatedDirective + ' ' + e.blockedURI);
    });
  });
  return page;
};
const collectCSP = async (page) => {
  for (const v of await page.evaluate(() => window.__csp ?? [])) report.csp_violations.push(v);
};
const shot = async (page, name) => {
  if (shotDir) await page.screenshot({ path: shotDir + '/' + name, fullPage: true });
};

// The operator.
const operator = await open();
await operator.goto(base + devLink);
await operator.goto(base + membersPath);
await operator.waitForLoadState();
await collectCSP(operator);
const form = operator.locator('form:has(input[name="islem"][value="kurtarma-yenile"])');
report.offered = (await form.count()) === 1;
if (report.offered) {
  const select = form.locator('select[name="kullanici"]');
  report.preselected = await select.inputValue();
  await select.selectOption({ label: memberEmail });
  await form.locator('input[type="password"]').fill(devPassword);
  await shot(operator, 'kurtarma-uyeler.png');
  await Promise.all([operator.waitForLoadState(), form.locator('button[type="submit"]').click()]);
  await operator.waitForLoadState();
  await collectCSP(operator);
  report.codes = await operator.locator('ul.kodlar code').allTextContents();
  await shot(operator, 'kurtarma-kod.png');
}

// The member, in a browser of their own.
if ((report.codes ?? []).length === 1) {
  const member = await open();
  await member.goto(base + loginPath);
  const link = member.locator('a[href="' + recoveryPath + '"]');
  report.recovery_link = (await link.count()) > 0;
  if (report.recovery_link) {
    await Promise.all([member.waitForLoadState(), link.first().click()]);
  } else {
    await member.goto(base + recoveryPath);
  }
  await member.waitForLoadState();
  await member.fill('#eposta', memberEmail);
  await member.fill('#kod', report.codes[0]);
  await member.fill('#yeni_parola', 'tarayicida-yeni-parola-2026');
  await member.fill('#yeni_parola_tekrar', 'tarayicida-yeni-parola-2026');
  await member.check('#ikinci_faktor');
  await Promise.all([member.waitForURL((u) => !u.pathname.startsWith(recoveryPath), { timeout: 15000 }).catch(() => null),
    member.click('main button[type=submit]')]);
  await member.waitForLoadState();
  await collectCSP(member);
  report.landed = member.url();
  report.told_no_codes = (await member.locator('main').innerText()).includes(noCodes);
  await shot(member, 'kurtarma-hesap.png');
}

console.log(JSON.stringify(report, null, 2));
await browser.close();
`
	dir := t.TempDir()
	name := filepath.Join(dir, "kurtarma-ver.mjs")
	ready, err := browsertest.Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(ready), 0o600); err != nil {
		t.Fatal(err)
	}
	return name
}
