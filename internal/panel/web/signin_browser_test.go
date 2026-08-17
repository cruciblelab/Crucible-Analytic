//go:build integration

package web

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"net/http/httptest"

	"github.com/pquerna/otp/totp"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// The customer's door, in a real browser.
//
// The Go tests above prove the handlers. What only Chromium can prove is
// that the pages are usable: that the password field is actually a
// password field, that the QR image loads rather than being refused by
// the Content-Security-Policy, that the code input accepts what a phone
// will paste into it, and that nothing on any of these pages trips the
// policy. Every one of those fails silently from the server's side - the
// response was 200 with correct bytes.
//
//	CA_BROWSER_TEST=1 go test -tags integration ./internal/panel/web/ \
//	    -run TestSignInInABrowser -v
func TestSignInInABrowser(t *testing.T) {
	if os.Getenv("CA_BROWSER_TEST") == "" {
		t.Skip("set CA_BROWSER_TEST=1 to run this; it needs node, playwright and a chromium build")
	}

	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "tarayici-giris"

	user := makeUser(t, store, "tarayici-kullanici", false)
	colleague := makeUser(t, store, "tarayici-meslektas", false)
	if err := store.AddMember(ctx, site, user.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, site, colleague.ID, panel.RoleViewer, nil); err != nil {
		t.Fatal(err)
	}

	// A second account with two-factor already on, so the browser walks
	// the code form with a code it computes itself.
	coded := makeUser(t, store, "tarayici-2fa", false)
	key, err := panel.NewTOTPSecret(coded.Email)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTOTPSecret(ctx, coded.ID, key.Secret()); err != nil {
		t.Fatal(err)
	}
	code, err := totp.GenerateCode(key.Secret(), time.Now())
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	script := writeSignInScript(t)
	cmd := exec.Command("node", script, server.URL,
		user.Email, colleague.Email, coded.Email, testAccountPassword, code, memberPath(site))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("browser run failed: %v", err)
	}
	t.Logf("browser transcript:\n%s", out)

	var report struct {
		CSPViolations []string `json:"csp_violations"`
		ConsoleErrors []string `json:"console_errors"`

		PasswordFieldType string `json:"password_field_type"`
		WrongPasswordMsg  string `json:"wrong_password_msg"`
		AfterSignIn       string `json:"after_sign_in"`
		SiteRowsSeen      int    `json:"site_rows_seen"`

		SecondFactorURL  string `json:"second_factor_url"`
		CodeFieldMode    string `json:"code_field_mode"`
		CodeAutocomplete string `json:"code_autocomplete"`
		AfterCode        string `json:"after_code"`

		QRLoaded      bool `json:"qr_loaded"`
		QRNaturalSize int  `json:"qr_natural_size"`

		MembersHeading  string   `json:"members_heading"`
		MemberRows      int      `json:"member_rows"`
		OwnerRoleValues []string `json:"owner_role_values"`

		ViewerMembersStatus int    `json:"viewer_members_status"`
		ViewerNavHasMembers bool   `json:"viewer_nav_has_members"`
		AfterSignOut        string `json:"after_sign_out"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("browser report was not JSON: %v\n%s", err, out)
	}

	// The policy is the one thing that must be clean on every page.
	if len(report.CSPViolations) > 0 {
		t.Errorf("Content-Security-Policy violations: %v", report.CSPViolations)
	}
	if len(report.ConsoleErrors) > 0 {
		t.Errorf("console errors: %v", report.ConsoleErrors)
	}

	// ---- the sign-in form ----
	if report.PasswordFieldType != "password" {
		t.Errorf("the password field is type %q; it would be shown in plain text",
			report.PasswordFieldType)
	}
	if report.WrongPasswordMsg == "" {
		t.Error("a wrong password produced no visible message")
	}
	if report.AfterSignIn != "/" {
		t.Errorf("signing in landed on %q, want the site list", report.AfterSignIn)
	}
	if report.SiteRowsSeen < 1 {
		t.Error("the site list is empty for an owner with a membership")
	}

	// ---- the second factor ----
	if report.SecondFactorURL != SecondFactorPath {
		t.Errorf("the password step went to %q", report.SecondFactorURL)
	}
	// inputmode is what makes a phone show a numeric keypad, and
	// one-time-code is what makes iOS offer the code from the
	// notification. Both are the difference between a usable form and
	// one people give up on.
	if report.CodeFieldMode != "numeric" {
		t.Errorf("the code field inputmode is %q, want numeric", report.CodeFieldMode)
	}
	if report.CodeAutocomplete != "one-time-code" {
		t.Errorf("the code field autocomplete is %q", report.CodeAutocomplete)
	}
	if report.AfterCode != "/" {
		t.Errorf("a correct code landed on %q", report.AfterCode)
	}

	// ---- the enrolment QR ----
	//
	// This is the assertion the Go tests genuinely cannot make. The
	// image is served from its own endpoint under a policy of
	// img-src 'self' data:, and a refusal would leave a broken image
	// with a 200 in the access log.
	if !report.QRLoaded {
		t.Error("the two-factor QR image did not load in the browser")
	}
	if report.QRNaturalSize < 100 {
		t.Errorf("the QR decoded to %dpx; it is not a real image", report.QRNaturalSize)
	}

	// ---- members, as an owner ----
	if report.MembersHeading == "" {
		t.Error("the member page has no heading")
	}
	if report.MemberRows < 2 {
		t.Errorf("the member table has %d rows, want the owner and the viewer", report.MemberRows)
	}
	// An owner may grant every role. The select is a courtesy - the
	// handler checks again - but a select missing "owner" for an owner
	// would mean nobody could ever hand a site over.
	if len(report.OwnerRoleValues) != 3 {
		t.Errorf("an owner is offered %v, want all three roles", report.OwnerRoleValues)
	}

	// ---- and as a viewer ----
	if report.ViewerMembersStatus != 403 {
		t.Errorf("a viewer got %d on the member page, want 403", report.ViewerMembersStatus)
	}
	if report.ViewerNavHasMembers {
		t.Error("the viewer's navigation offers a page they cannot open")
	}
	if report.AfterSignOut != LoginPath {
		t.Errorf("signing out landed on %q", report.AfterSignOut)
	}
}

func writeSignInScript(t *testing.T) string {
	t.Helper()

	const script = `
import playwright from '/opt/node22/lib/node_modules/playwright/index.js';
const { chromium } = playwright;

const [base, ownerEmail, viewerEmail, codedEmail, password, code, membersPath] =
  process.argv.slice(2);

const browser = await chromium.launch({ executablePath: '/opt/pw-browsers/chromium' });
const report = { csp_violations: [], console_errors: [], owner_role_values: [] };

const page = await browser.newPage();
// Collected only while on pages meant to succeed. One navigation below
// is a deliberate 403 and Chromium logs a failed load for it; asserting
// on that would be asserting that the refusal does not work.
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
const path = () => new URL(page.url()).pathname;

// ---- the sign-in form ----
await page.goto(base + '/giris');
report.password_field_type = await page.getAttribute('#parola', 'type');

// A deliberate 401, so console collection pauses: Chromium logs a
// failed load for it, and asserting on that would be asserting that the
// refusal does not work.
collecting = false;
await page.fill('#eposta', ownerEmail);
await page.fill('#parola', 'kesinlikle-yanlis-bir-parola');
await page.click('button[type=submit]');
await page.waitForLoadState();
collecting = true;
report.wrong_password_msg = (await page.textContent('[role=alert]') ?? '').trim();
await collectCSP();

// ---- and the right one ----
await page.fill('#eposta', ownerEmail);
await page.fill('#parola', password);
await page.click('button[type=submit]');
await page.waitForLoadState();
report.after_sign_in = path();
report.site_rows_seen = await page.locator('table.liste tbody tr').count();
await collectCSP();

// ---- the enrolment QR, on this owner's account page ----
await page.goto(base + '/hesap');
await page.click('input[value="2fa-basla"] + button, form:has(input[value="2fa-basla"]) button');
await page.waitForLoadState();
const qr = page.locator('img.qr');
await qr.waitFor({ state: 'visible', timeout: 5000 });
report.qr_loaded = await qr.evaluate((img) => img.complete && img.naturalWidth > 0);
report.qr_natural_size = await qr.evaluate((img) => img.naturalWidth);
await collectCSP();

// ---- members, as the owner ----
await page.goto(base + membersPath);
report.members_heading = (await page.textContent('h1') ?? '').trim();
report.member_rows = await page.locator('table.liste tbody tr').count();
report.owner_role_values = await page.locator('table.liste tbody tr select').first()
  .evaluate((sel) => Array.from(sel.options).map((o) => o.value));
await collectCSP();

// ---- sign out ----
await page.click('form[action="/cikis"] button, header form button');
await page.waitForLoadState();
report.after_sign_out = path();
await collectCSP();

// ---- the second factor, as the account that has one ----
await page.goto(base + '/giris');
await page.fill('#eposta', codedEmail);
await page.fill('#parola', password);
await page.click('button[type=submit]');
await page.waitForLoadState();
report.second_factor_url = path();
report.code_field_mode = await page.getAttribute('#kod', 'inputmode');
report.code_autocomplete = await page.getAttribute('#kod', 'autocomplete');
await page.fill('#kod', code);
await page.click('button[type=submit]');
await page.waitForLoadState();
report.after_code = path();
await collectCSP();

// ---- and the viewer's refusal ----
//
// A fresh page from browser.newPage() gets its own context, so it
// carries none of the cookies above - which is what makes signing in as
// somebody else here honest rather than a session left half-swapped.
const viewer = await browser.newPage();
await viewer.goto(base + '/giris');
await viewer.fill('#eposta', viewerEmail);
await viewer.fill('#parola', password);
await viewer.click('button[type=submit]');
await viewer.waitForLoadState();
report.viewer_nav_has_members = await viewer.locator('header nav a[href*="/uyeler"]').count() > 0;

collecting = false;
const refused = await viewer.goto(base + membersPath);
report.viewer_members_status = refused.status();

await browser.close();
console.log(JSON.stringify(report));
`
	dir := t.TempDir()
	path := filepath.Join(dir, "signin.mjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
