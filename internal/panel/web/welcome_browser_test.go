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

	"github.com/cruciblelab/crucible-analytic/internal/browsertest"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// The customer's first ten minutes, in a real browser.
//
// The Go tests prove the handlers and the store. What only Chromium can
// prove is that this reads as a product: that the invitation page shows
// the address it is for, that the wizard's steps advance, that the
// snippet is selectable text rather than an image of one, and that the
// technical door's warning is a warning rather than a link somebody
// clicks through without reading.
//
//	CA_BROWSER_TEST=1 go test -tags integration ./internal/panel/web/ \
//	    -run TestOwnerWizardInABrowser -v
func TestOwnerWizardInABrowser(t *testing.T) {
	if os.Getenv("CA_BROWSER_TEST") == "" {
		t.Skip("set CA_BROWSER_TEST=1 to run this; it needs node, playwright and a chromium build")
	}

	srv, store := setupTestServer(t)
	srv.BeaconURL = "https://olcum.example.test"
	ctx := context.Background()
	const site = "tarayici-hosgeldiniz"
	const email = "sahip@tarayici-hg.invalid"

	if err := store.SetSetting(ctx, panel.KeyBeaconSites, "", []string{site}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool := store.Pool()
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM panel_audit_log WHERE actor_label = $1`, email)
		_, _ = pool.Exec(bg, `DELETE FROM panel_settings WHERE site_id = $1`, site)
		_, _ = pool.Exec(bg, `DELETE FROM panel_site_members WHERE site_id = $1`, site)
		_, _ = pool.Exec(bg, `DELETE FROM panel_owner_claims WHERE email = $1`, email)
		_, _ = pool.Exec(bg, `DELETE FROM panel_users WHERE email = $1`, email)
		_, _ = pool.Exec(bg, `DELETE FROM panel_settings WHERE key = $1`,
			string(panel.KeyPanelTimezone))
	})

	token, claim, err := store.CreateOwnerClaim(ctx, email, "Tarayıcı Sahibi",
		panel.Principal{Label: "test"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = claim

	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	script := writeWelcomeScript(t)
	cmd := exec.Command("node", script, server.URL, ClaimPathPrefix+token, email, site)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("browser run failed: %v", err)
	}
	t.Logf("browser transcript:\n%s", out)

	var report struct {
		CSPViolations []string `json:"csp_violations"`
		ConsoleErrors []string `json:"console_errors"`

		ClaimHeading      string   `json:"claim_heading"`
		ClaimShowsEmail   bool     `json:"claim_shows_email"`
		PasswordFieldTyp  string   `json:"password_field_type"`
		AfterClaim        string   `json:"after_claim"`
		RecoveryCodes     []string `json:"recovery_codes"`
		RecoveryWarnsOnce bool     `json:"recovery_warns_once"`

		StepTitles   []string `json:"step_titles"`
		StepsVisited []string `json:"steps_visited"`
		SavedName    string   `json:"saved_name"`
		SavedZone    string   `json:"saved_zone"`
		Snippet      string   `json:"snippet"`
		TeamHasLink  bool     `json:"team_has_link"`

		DoorWarning   string `json:"door_warning"`
		WizardBefore  string `json:"wizard_before"`
		WizardAfter   int    `json:"wizard_after"`
		FooterHasZone bool   `json:"footer_has_zone"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("browser report was not JSON: %v\n%s", err, out)
	}

	if len(report.CSPViolations) > 0 {
		t.Errorf("Content-Security-Policy violations: %v", report.CSPViolations)
	}
	if len(report.ConsoleErrors) > 0 {
		t.Errorf("console errors: %v", report.ConsoleErrors)
	}

	// ---- the invitation ----
	if !report.ClaimShowsEmail {
		t.Error("the invitation page does not say which address the account is for")
	}
	if report.PasswordFieldTyp != "password" {
		t.Errorf("the password field is type %q", report.PasswordFieldTyp)
	}
	// Claiming now lands on the recovery codes rather than the wizard.
	// The wizard is one deliberate click further on, which the script
	// takes - so the assertions below about the steps still hold.
	if len(report.RecoveryCodes) != panel.RecoveryCodeCount {
		t.Errorf("the page after claiming showed %d recovery codes, want %d",
			len(report.RecoveryCodes), panel.RecoveryCodeCount)
	}
	for _, code := range report.RecoveryCodes {
		if panel.NormalizeRecoveryCode(code) == "" {
			t.Errorf("a rendered recovery code normalises to nothing: %q", code)
		}
	}
	if !report.RecoveryWarnsOnce {
		t.Error("the recovery-code page does not warn that the codes are shown once")
	}
	if report.AfterClaim != ClaimPathPrefix+token {
		t.Errorf("claiming landed on %q, want the codes page at the claim URL", report.AfterClaim)
	}

	// ---- the wizard ----
	if len(report.StepTitles) != len(welcomeSteps) {
		t.Errorf("the progress list shows %d steps, want %d: %v",
			len(report.StepTitles), len(welcomeSteps), report.StepTitles)
	}
	for _, title := range report.StepTitles {
		// The marker the renderer writes for a key with no translation.
		if strings.Contains(title, "⟦") {
			t.Errorf("a step title rendered as an untranslated marker: %q", title)
		}
	}
	if len(report.StepsVisited) < len(welcomeSteps) {
		t.Errorf("the browser reached %d steps, want all %d: %v",
			len(report.StepsVisited), len(welcomeSteps), report.StepsVisited)
	}
	if report.SavedName != "Kahve Dükkânı" {
		t.Errorf("the site name did not survive the round trip: %q", report.SavedName)
	}
	if report.SavedZone != "Europe/Istanbul" {
		t.Errorf("the time zone did not survive the round trip: %q", report.SavedZone)
	}
	// The setting has to reach the rendered page, or storing it was
	// pointless. The footer names the zone every page renders in.
	if !report.FooterHasZone {
		t.Error("the chosen time zone does not reach the page footer")
	}

	// ---- the snippet ----
	//
	// Selectable text, and the real site id: a snippet with the wrong
	// id is pasted onto a live website and silently measures nothing.
	if !strings.Contains(report.Snippet, site) {
		t.Errorf("the snippet does not name the site: %q", report.Snippet)
	}
	if !strings.Contains(report.Snippet, "olcum.example.test") {
		t.Errorf("the snippet does not point at the beacon: %q", report.Snippet)
	}
	if !report.TeamHasLink {
		t.Error("the last step offers no way to reach the members page")
	}

	// ---- the technical door ----
	if !strings.Contains(report.DoorWarning, "geliştiriciniz") {
		t.Errorf("the door does not carry the warning: %q", report.DoorWarning)
	}
	if report.WizardBefore != TechnicalDoorPath {
		t.Errorf("an unwarned owner reached %q instead of the door", report.WizardBefore)
	}
	if report.WizardAfter != 200 {
		t.Errorf("the technical wizard answered %d after the owner confirmed", report.WizardAfter)
	}
}

func writeWelcomeScript(t *testing.T) string {
	t.Helper()

	const script = `
import playwright from '/opt/node22/lib/node_modules/playwright/index.js';
const { chromium } = playwright;

const [base, claimPath, email, site] = process.argv.slice(2);

const browser = await chromium.launch({ executablePath: '/opt/pw-browsers/chromium' });
const report = { csp_violations: [], console_errors: [], step_titles: [], steps_visited: [] };

const page = await browser.newPage();
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

// ---- the invitation ----
await page.goto(base + claimPath);
report.claim_heading = (await page.textContent('h1') ?? '').trim();
report.claim_shows_email = (await page.textContent('main') ?? '').includes(email);
report.password_field_type = await page.getAttribute('#parola', 'type');
await collectCSP();

await page.fill('#parola', 'tarayici-sahip-parolasi');
await page.fill('#parola_tekrar', 'tarayici-sahip-parolasi');
await page.click('main button[type=submit]');
await page.waitForLoadState();

// ---- the recovery codes ----
//
// The page a new owner sees before anything else, and the only time
// these exist in readable form. Read here rather than trusted: this is
// the moment the customer either saves them or does not, and if the page
// showed nothing the account would have no way back into it.
report.after_claim = path();
report.recovery_codes = (await page.locator('main code.secret').allInnerTexts())
  .map((s) => s.trim()).filter((s) => s.length > 0);
report.recovery_warns_once = (await page.textContent('main') ?? '')
  .includes('bir daha gösterilmeyecek');
await collectCSP();

// Continuing is a deliberate click, never automatic: the whole point of
// the page is that somebody reads it before moving on.
await page.click('main a.dugme');
await page.waitForLoadState();
report.step_titles = await page.locator('nav.ilerleme li').allInnerTexts()
  .then((xs) => xs.map((s) => s.trim()));
await collectCSP();

// ---- step 1: the site's name ----
report.steps_visited.push(path());
await page.fill('input[name="ad:' + site + '"]', 'Kahve Dükkânı');
await page.click('main button[type=submit]');
await page.waitForLoadState();
report.saved_name = await page.inputValue('input[name="ad:' + site + '"]');
await collectCSP();

// ---- step 2: the time zone ----
// The "next" link in the wizard's own footer. The progress list's
// future steps are spans, not links - deliberately, so nobody skips a
// step by clicking ahead - so it is not a navigation control.
await page.click('.kurulum-gezinme a.dugme');
await page.waitForLoadState();
report.steps_visited.push(path());
await page.fill('#saat_dilimi', 'Europe/Istanbul');
await page.click('main button[type=submit]');
await page.waitForLoadState();
report.saved_zone = await page.inputValue('#saat_dilimi');
report.footer_has_zone = (await page.textContent('footer') ?? '').includes('Europe/Istanbul');
await collectCSP();

// ---- step 3: the snippet ----
await page.click('.kurulum-gezinme a.dugme');
await page.waitForLoadState();
report.steps_visited.push(path());
report.snippet = (await page.textContent('main pre code') ?? '').trim();
await collectCSP();

// ---- step 4: the team ----
await page.click('.kurulum-gezinme a.dugme');
await page.waitForLoadState();
report.steps_visited.push(path());
report.team_has_link = await page.locator('main a[href*="/uyeler"]').count() > 0;
await collectCSP();

// ---- the technical door ----
await page.goto(base + '/kurulum/baslangic');
await page.waitForLoadState();
report.wizard_before = path();
report.door_warning = (await page.textContent('.kilit, .uyari') ?? '').trim();
await collectCSP();

await page.click('main button[type=submit]');
await page.waitForLoadState();
const after = await page.goto(base + '/kurulum/baslangic');
report.wizard_after = after.status();
await collectCSP();

await browser.close();
console.log(JSON.stringify(report));
`
	dir := t.TempDir()
	path := filepath.Join(dir, "welcome.mjs")
	ready, err := browsertest.Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(ready), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
