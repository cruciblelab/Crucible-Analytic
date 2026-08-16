//go:build integration

package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/argon2id"
	"github.com/cruciblelab/crucible-analytic/internal/devgate"
)

// A real browser typing a real password into a real form, against a real
// database.
//
// The unit tests prove the gate's logic and the integration tests prove
// the store refuses an unauthorized write. Neither proves the thing a
// person actually experiences: that the prompt appears, that a wrong
// password changes nothing, that the right one does, and - the property
// the whole design turns on - that the next change asks again rather
// than riding on the last answer.
//
// This project has been caught once by a defect that every synthetic
// test passed and only a real run revealed, which is why this exists in
// the form it does.
//
//	CA_BROWSER_TEST=1 go test -tags integration ./internal/panel/ \
//	    -run TestDeveloperPasswordInABrowser -v
func TestDeveloperPasswordInABrowser(t *testing.T) {
	if os.Getenv("CA_BROWSER_TEST") == "" {
		t.Skip("set CA_BROWSER_TEST=1 to run this; it needs node, playwright and a chromium build")
	}

	store := settingsStore(t)
	ctx := context.Background()

	hash, err := argon2id.Hash(testDevPassword)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	gate, err := devgate.New(devgate.Config{PasswordHash: hash}, devgate.Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Audit:  store.GateAudit(),
	})
	if err != nil {
		t.Fatalf("devgate.New: %v", err)
	}

	server := httptest.NewServer(settingsFormHandler(t, store, gate))
	defer server.Close()

	script := writeBrowserScript(t)
	cmd := exec.Command("node", script, server.URL)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("browser run failed: %v", err)
	}
	t.Logf("browser transcript:\n%s", out)

	var report struct {
		PromptShown       bool   `json:"prompt_shown"`
		PromptMentionsWhy bool   `json:"prompt_mentions_why"`
		WrongPassword     string `json:"wrong_password"`
		RightPassword     string `json:"right_password"`
		SecondSaveNoField string `json:"second_save_no_field"`
		ValueAfterWrong   string `json:"value_after_wrong"`
		ValueAfterRight   string `json:"value_after_right"`
		ValueAfterSecond  string `json:"value_after_second"`
	}
	last := strings.TrimSpace(string(out))
	if i := strings.LastIndex(last, "\n"); i >= 0 {
		last = last[i+1:]
	}
	if err := json.Unmarshal([]byte(last), &report); err != nil {
		t.Fatalf("could not read the browser's report %q: %v", last, err)
	}

	if !report.PromptShown {
		t.Error("the page did not put up a developer password prompt for a guarded setting")
	}
	if !report.PromptMentionsWhy {
		t.Error("the prompt did not say why this setting is guarded")
	}

	if !strings.Contains(report.WrongPassword, "yanlış") {
		t.Errorf("a wrong password produced %q, want the refusal message", report.WrongPassword)
	}
	if report.ValueAfterWrong != IPStorageMasked {
		t.Errorf("after the wrong password the setting is %q; the refused write went through", report.ValueAfterWrong)
	}

	if !strings.Contains(report.RightPassword, "kaydedildi") {
		t.Errorf("the correct password produced %q, want a success message", report.RightPassword)
	}
	if report.ValueAfterRight != IPStorageFull {
		t.Errorf("after the correct password the setting is %q, want %q", report.ValueAfterRight, IPStorageFull)
	}

	// The point of the whole design: the second change does not ride on
	// the first one's answer.
	if !strings.Contains(report.SecondSaveNoField, "girilmedi") {
		t.Errorf("a second save with an empty password field produced %q; it should have been asked again", report.SecondSaveNoField)
	}
	if report.ValueAfterSecond != IPStorageFull {
		t.Errorf("the second save changed the value to %q without a password", report.ValueAfterSecond)
	}

	// And every one of those attempts is in the append-only log.
	entries, _, err := store.Audit(ctx, AuditFilter{Limit: 100})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	var granted, refused int
	for _, e := range entries {
		switch e.Action {
		case ActionDevPasswordGranted:
			granted++
		case ActionDevPasswordRefused:
			refused++
		}
	}
	if granted < 1 || refused < 2 {
		t.Errorf("audit holds %d granted and %d refused attempts, want at least 1 and 2", granted, refused)
	}
}

// settingsFormHandler is the smallest honest stand-in for the settings
// page C1 will build: it renders the prompt from PromptFor, verifies with
// GateRequest, and writes with ApplySetting. Every one of those is the
// function the real page will call, which is what makes driving this
// with a browser worth anything.
func settingsFormHandler(t *testing.T, store *Store, gate *devgate.Gate) http.Handler {
	t.Helper()

	page := template.Must(template.New("settings").Parse(`<!doctype html>
<html lang="tr"><head><meta charset="utf-8"><title>Ayarlar</title></head><body>
<h1>Gizlilik ayarları</h1>
<p id="current">Şu anki değer: <b id="value">{{.Value}}</b></p>
{{if .Message}}<p id="message">{{.Message}}</p>{{end}}
<form method="post" action="/">
  <label>IP saklama biçimi
    <select name="value" id="mode">
      <option value="masked">masked</option>
      <option value="full">full</option>
    </select>
  </label>
  <fieldset id="gate">
    <legend>Geliştirici şifresi</legend>
    <p id="notice">{{.Prompt.Notice}}</p>
    <ul id="reasons">{{range .Prompt.Reasons}}<li>{{.Label}}: {{.Reason}}</li>{{end}}</ul>
    <input type="password" name="{{.Prompt.FormField}}" id="devpass">
  </fieldset>
  <button type="submit" id="save">Kaydet</button>
</form>
</body></html>`))

	principal := Principal{Kind: PrincipalUser, Label: "tarayici@example.com", Superadmin: true}

	render := func(w http.ResponseWriter, r *http.Request, message string) {
		value, err := store.GetStringSetting(r.Context(), KeyPrivacyIPStorage, "")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = page.Execute(w, map[string]any{
			"Value":   value,
			"Message": message,
			"Prompt":  PromptFor(gate.Configured(), KeyPrivacyIPStorage),
		})
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			render(w, r, "")
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		value := r.PostForm.Get("value")

		// The action list is the server's own conclusion about what it is
		// about to change, never anything the form sent.
		result := gate.Verify(r.Context(), store.GateRequest(principal, r, KeyPrivacyIPStorage))
		if !result.OK() {
			render(w, r, devgate.Explain(result))
			return
		}

		if err := store.ApplySetting(r.Context(), principal, KeyPrivacyIPStorage, "", value,
			result.For(GateAction(KeyPrivacyIPStorage))); err != nil {
			render(w, r, fmt.Sprintf("Kaydedilemedi: %v", err))
			return
		}
		render(w, r, "Ayar kaydedildi.")
	})
}

func writeBrowserScript(t *testing.T) string {
	t.Helper()

	const script = `
import playwright from '/opt/node22/lib/node_modules/playwright/index.js';
const { chromium } = playwright;

const base = process.argv[2];
const browser = await chromium.launch({ executablePath: '/opt/pw-browsers/chromium' });
const page = await browser.newPage();
const report = {};

const message = () => page.locator('#message').count().then(async (n) =>
  n ? (await page.locator('#message').innerText()).trim() : '');
const value = () => page.locator('#value').innerText();

await page.goto(base, { waitUntil: 'load' });

report.prompt_shown = (await page.locator('#gate').count()) > 0;
const notice = await page.locator('#notice').innerText();
const reasons = await page.locator('#reasons').innerText();
report.prompt_mentions_why = notice.includes('geliştirici') && reasons.includes('KVKK');
console.log('prompt:', notice.slice(0, 80) + '...');

// 1. The wrong password.
await page.selectOption('#mode', 'full');
await page.fill('#devpass', 'yanlis-sifre-1234');
await page.click('#save');
await page.waitForLoadState('load');
report.wrong_password = await message();
report.value_after_wrong = await value();
console.log('wrong password ->', report.wrong_password, '| value now', report.value_after_wrong);

// 2. The right one.
await page.selectOption('#mode', 'full');
await page.fill('#devpass', 'test-gelistirici-sifresi');
await page.click('#save');
await page.waitForLoadState('load');
report.right_password = await message();
report.value_after_right = await value();
console.log('right password ->', report.right_password, '| value now', report.value_after_right);

// 3. A second change, with the password field left empty. If anything
//    remembered the last answer, this would go through.
await page.selectOption('#mode', 'masked');
await page.click('#save');
await page.waitForLoadState('load');
report.second_save_no_field = await message();
report.value_after_second = await value();
console.log('second save, empty field ->', report.second_save_no_field, '| value now', report.value_after_second);

await browser.close();
console.log(JSON.stringify(report));
`
	path := filepath.Join(t.TempDir(), "gate.mjs")
	if err := os.WriteFile(path, []byte(script), 0o600); err != nil {
		t.Fatalf("writing the browser script: %v", err)
	}
	return path
}
