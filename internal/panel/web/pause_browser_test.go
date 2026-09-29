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

	"github.com/cruciblelab/crucible-analytic/internal/browsertest"
	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// TestTheOwnerPausesRecordingInABrowser is the pause (PLAN §4, #3) in real
// Chromium, by clicks: the owner signs in, opens the collection section,
// picks six hours in the row's own list and presses its own button; the
// row then says until when, and the site's page says recording is paused
// and names the writer that would not honour it. Then "switch off now",
// the same way: the row is off, the store is empty, and the site's page
// says nothing - the pause lasted seconds, less than any writer's poll, so
// the stretch is not named as one that went unrecorded.
//
// The Go tests post the form's fields by name. What a browser adds is that
// the page's own select and button produce that post - the row has a
// second form beside it, the reset, and a button in the wrong one would
// post a different operation with the same key.
func TestTheOwnerPausesRecordingInABrowser(t *testing.T) {
	if os.Getenv("CA_BROWSER_TEST") == "" {
		t.Skip("set CA_BROWSER_TEST=1 to run this; it needs node, playwright and a chromium build")
	}

	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "duraklat-tarayici"
	clearSiteSettings(t, store, site)
	clearPauseHistory(t, site)
	owner := makeUser(t, store, "dt-sahip", false)
	if err := store.AddMember(ctx, site, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	// After setupTestServer's AccountsLock, as every suite holding both.
	admin := testdb.Admin(t)
	testdb.Lock(t, admin, testdb.HeartbeatLock)
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(),
			`DELETE FROM service_heartbeat WHERE version LIKE 'dt-%'`); err != nil {
			t.Logf("clearing the test heartbeat rows: %v", err)
		}
	})
	started := time.Now().Add(-time.Hour)
	writeBeatDetail(t, store, testdb.Collector, "dt-eski", started,
		map[string]int64{heartbeat.CounterWritten: 1200}, nil, "", heartbeat.TokenKeyUnknown)
	writeBeatDetail(t, store, testdb.Beacon, "dt-yeni", started,
		map[string]int64{heartbeat.CounterAccepted: 900, heartbeat.CounterWritten: 900,
			heartbeat.CounterPaused: 42}, nil, "", heartbeat.TokenKeyUnknown)

	running := httptest.NewServer(srv.Handler())
	t.Cleanup(running.Close)
	base := srv.Renderer.Catalogs().Base()

	script := writePauseScript(t)
	before := time.Now()
	cmd := exec.Command("node", script, running.URL, LoginPath, owner.Email, testAccountPassword,
		settingsPath(site), pauseKey, sitePath(site), HealthPath, os.Getenv("CA_SHOT_DIR"))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("browser run failed: %v", err)
	}
	t.Logf("browser transcript:\n%s", out)

	var report struct {
		CSPViolations []string `json:"csp_violations"`
		ConsoleErrors []string `json:"console_errors"`
		// Options are the row's choices as the page drew them.
		Options []struct {
			Value string `json:"value"`
			Label string `json:"label"`
		} `json:"options"`
		// Before and After are the row's state line either side of the click.
		Before string `json:"before"`
		After  string `json:"after"`
		// Banner is the site page's notice text.
		Banner string `json:"banner"`
		// OffLabel is the zero choice while paused; Resumed the row's
		// state line after choosing it; Notices the site page's notices
		// after that.
		OffLabel string   `json:"off_label"`
		Resumed  string   `json:"resumed"`
		Notices  []string `json:"notices"`
	}
	if err := json.Unmarshal(out[strings.LastIndex(string(out), "\n{")+1:], &report); err != nil {
		t.Fatalf("reading the browser's report: %v", err)
	}

	want := []string{"0 " + base.T("ayarlar.sure.kapat"), "60 1 saat", "360 6 saat", "1440 1 gün", "10080 7 gün"}
	var got []string
	for _, o := range report.Options {
		got = append(got, o.Value+" "+o.Label)
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("the row offers %q, want %q", got, want)
	}
	if report.Before != base.T("ayarlar.sure.kapali_durum") {
		t.Errorf("before the click the row says %q", report.Before)
	}
	// What the pause click stored, read from the audit entry it wrote: by
	// now the script has switched the pause off again, and the setting
	// holds the empty value that did.
	var stored string
	if err := store.Pool().QueryRow(ctx, `SELECT detail->>'to' FROM panel_audit_log
		WHERE site_id = $1 AND target = $2 AND detail->>'to' <> '' ORDER BY id DESC LIMIT 1`,
		site, pauseKey).Scan(&stored); err != nil {
		t.Fatalf("no audit entry for the pause click: %v", err)
	}
	end, err := time.Parse(time.RFC3339, stored)
	if err != nil {
		t.Fatalf("the pause click stored %q", stored)
	}
	if lo, hi := before.Add(6*time.Hour-time.Second), time.Now().Add(6*time.Hour+time.Second); end.Before(lo) || end.After(hi) {
		t.Errorf("the click stored %s; six hours from it is between %s and %s", end, lo, hi)
	}
	if !strings.Contains(report.After, "saatine kadar açık") {
		t.Errorf("after the click the row says %q", report.After)
	}
	if !strings.Contains(report.Banner, "duraklatıldı") ||
		!strings.Contains(report.Banner, "Dikkat: "+base.T("saglik.servis.collector")) {
		t.Errorf("the site's page says %q; want the pause, and the collector named", report.Banner)
	}
	if report.OffLabel != base.T("ayarlar.sure.simdi_kapat") {
		t.Errorf("while paused the zero choice reads %q", report.OffLabel)
	}
	if report.Resumed != base.T("ayarlar.sure.kapali_durum") || pausedUntil(t, store, site) != "" {
		t.Errorf("after switching off the row says %q and the store holds %q",
			report.Resumed, pausedUntil(t, store, site))
	}
	for _, n := range report.Notices {
		if strings.Contains(n, "duraklat") {
			t.Errorf("after a pause of seconds the site's page still says %q", n)
		}
	}
	if len(report.CSPViolations) != 0 {
		t.Errorf("CSP violations: %v", report.CSPViolations)
	}
	if len(report.ConsoleErrors) != 0 {
		t.Errorf("console errors: %v", report.ConsoleErrors)
	}
}

func writePauseScript(t *testing.T) string {
	t.Helper()

	const script = `
import playwright from '/opt/node22/lib/node_modules/playwright/index.js';
const { chromium } = playwright;

const [base, loginPath, email, password, settingsPath, key, sitePath, healthPath, shotDir] =
  process.argv.slice(2);

const browser = await chromium.launch({ executablePath: '/opt/pw-browsers/chromium' });
const report = { csp_violations: [], console_errors: [] };
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
const collectCSP = async () => {
  for (const v of await page.evaluate(() => window.__csp ?? [])) report.csp_violations.push(v);
};
const shot = async (locator, name) => {
  if (shotDir) await locator.screenshot({ path: shotDir + '/' + name });
};

await page.goto(base + loginPath);
await page.fill('#eposta', email);
await page.fill('#parola', password);
await Promise.all([page.waitForNavigation(), page.click('form button[type=submit]')]);
console.log('signed in at ' + new URL(page.url()).pathname);

// The row, found by the key its own form carries, and its section opened
// the way a person opens it: by the summary.
const row = () => page.locator('section.ayar', { has: page.locator('input[name=anahtar][value="' + key + '"]') });
const openRow = async () => {
  const details = page.locator('details', { has: row() });
  if (!(await details.evaluate((d) => d.open))) await details.locator('summary').click();
};

await page.goto(base + settingsPath);
await collectCSP();
await openRow();
report.options = await row().locator('select[name=deger] option').evaluateAll((os) =>
  os.map((o) => ({ value: o.value, label: o.textContent.trim() })));
report.before = (await row().locator('.durum').textContent()).trim();
await shot(row(), 'duraklat-once.png');

// The row's own list and the button in the same form as that list.
const form = row().locator('form', { has: page.locator('select[name=deger]') });
await form.locator('select[name=deger]').selectOption('360');
await Promise.all([page.waitForNavigation(), form.locator('button[type=submit]').click()]);
await collectCSP();
await openRow();
report.after = (await row().locator('.durum').textContent()).trim();
await shot(row(), 'duraklat-sonra.png');

await page.goto(base + sitePath);
await collectCSP();
report.banner = (await page.locator('main > div.uyari', { hasText: 'duraklatıldı' }).first().textContent()).trim();
if (shotDir) await page.screenshot({ path: shotDir + '/duraklat-pano.png' });

// Switch off now: the zero choice, in the same form, the same button.
await page.goto(base + settingsPath);
await openRow();
const again = row().locator('form', { has: page.locator('select[name=deger]') });
report.off_label = (await again.locator('select[name=deger] option[value="0"]').textContent()).trim();
await again.locator('select[name=deger]').selectOption('0');
await Promise.all([page.waitForNavigation(), again.locator('button[type=submit]').click()]);
await collectCSP();
await openRow();
report.resumed = (await row().locator('.durum').textContent()).trim();
await page.goto(base + sitePath);
await collectCSP();
report.notices = await page.locator('main > div[role=note]').evaluateAll((ns) => ns.map((n) => n.textContent.trim()));

if (shotDir) {
  await page.goto(base + healthPath);
  await collectCSP();
  await page.screenshot({ path: shotDir + '/duraklat-saglik.png', fullPage: true });
}

await browser.close();
console.log(JSON.stringify(report));
`
	prepared, err := browsertest.Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "duraklat.mjs")
	if err := os.WriteFile(path, []byte(prepared), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
