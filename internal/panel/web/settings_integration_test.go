//go:build integration

// The settings page, and mostly the ways it must refuse.
//
// The page draws controls only where a write would be allowed, and that
// drawing is cosmetic in exactly the way the navigation is. Everything
// below submits a POST directly rather than clicking what was rendered,
// because the question is never "what did the page offer" - it is "what
// does the server do when somebody sends the form the page did not
// draw".
//
// AI-1, in the customer's words: *istemciye güvenme, sadece sunucuya
// güven*. A test that only exercises the rendered controls is a test
// written from the client's side of that rule.
package web

import (
	"context"
	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

const (
	settingsSite = "ayar-testi"
	// A guarded setting and an unguarded one, named rather than
	// discovered: which settings carry legal weight is a decision, and a
	// test that picked "the first guarded one it found" would quietly
	// stop testing the guard if the order changed.
	unguardedKey = panel.KeySiteName
	guardedKey   = panel.KeyPrivacyIPStorage
)

// settingsServerAs signs in a user with one role on settingsSite.
func settingsServerAs(t *testing.T, role panel.Role) (*httptest.Server, *http.Client, *panel.Store) {
	t.Helper()

	srv, store := setupTestServer(t)
	withRealAPI(t, srv)

	user := makeUser(t, store, "ayar-"+string(role), false)
	if err := store.AddMember(context.Background(), settingsSite, user.ID, role, nil); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	t.Cleanup(server.Close)
	return server, signedIn(t, server.URL, user.Email), store
}

// settingsServerAsOperator signs in somebody who may change a guarded
// setting at all.
//
// Superadmin, and that is not a shortcut. Access.operator() reads
// Principal.Superadmin, and in a running deployment that flag is set by
// redeeming a developer link the owner approved - never by a row a
// customer can reach. So this is the test's stand-in for a developer
// session, and the reason the *owner* tests above cannot exercise the
// gate: an owner is refused before a password is ever considered.
//
// Which is the model the customer described: a thing that makes work for
// the geliştirici cannot be protected by a capability, because a müşteri
// can grant themselves capabilities.
func settingsServerAsOperator(t *testing.T) (*httptest.Server, *http.Client, *panel.Store) {
	t.Helper()

	srv, store := setupTestServer(t)
	withRealAPI(t, srv)

	dev := makeUser(t, store, "ayar-operator", true)
	if err := store.AddMember(context.Background(), settingsSite, dev.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	t.Cleanup(server.Close)
	return server, signedIn(t, server.URL, dev.Email), store
}

// restoreGlobal removes a deployment-wide setting row when the test
// ends.
//
// These tests share one database with every other suite, and a global
// row outlives the site the test invented. Measured the hard way: a test
// here wrote logs.retention_days = 21 and
// panel.TestSettings_DefaultsApplyBeforeAnythingIsStored then failed,
// three packages away, asserting the default it no longer had.
//
// Site-scoped rows need no such care - they are keyed by a site id
// nothing else uses.
func restoreGlobal(t *testing.T, store *panel.Store, keys ...panel.Key) {
	t.Helper()
	t.Cleanup(func() {
		for _, key := range keys {
			if _, err := store.Pool().Exec(context.Background(),
				`DELETE FROM panel_settings WHERE key = $1 AND site_id = ''`, string(key)); err != nil {
				t.Errorf("restoring %s: %v", key, err)
			}
		}
	})
}

func settingsURL() string { return MembersPathPrefix + settingsSite + settingsPathSuffix }

// postSetting submits the form the page would have submitted, or the one
// it would not have.
//
// Through the shared post helper, so the CSRF token is a real one taken
// from a real render: a test that forged its own token would be testing
// a handler nobody reaches.
func postSetting(t *testing.T, client *http.Client, base string, form url.Values) (int, string) {
	t.Helper()
	return post(t, client, base+settingsURL(), form)
}

// TestAnOwnerCanChangeAnUnguardedSetting is the happy path, and it is
// here so the refusals below mean something: a suite where everything
// fails proves only that the handler is broken.
func TestAnOwnerCanChangeAnUnguardedSetting(t *testing.T) {
	server, client, store := settingsServerAs(t, panel.RoleOwner)

	status, body := postSetting(t, client, server.URL, url.Values{
		"islem":   {"kaydet"},
		"anahtar": {string(unguardedKey)},
		"deger":   {"Yeni Site Adı"},
	})
	if status != http.StatusOK {
		t.Fatalf("saving answered %d: %s", status, body)
	}

	// Read back from the store, not from the page. A page that echoed
	// what was typed would pass this while having written nothing.
	got, err := store.GetSetting(context.Background(), unguardedKey, settingsSite)
	if err != nil {
		t.Fatal(err)
	}
	if got != "Yeni Site Adı" {
		t.Errorf("the stored value is %v, not what was submitted", got)
	}
}

// TestAViewerSeesEverySettingAndCanChangeNone.
//
// Both halves matter and they pull in opposite directions. A viewer must
// be able to read their own deployment's configuration - hiding it means
// a customer has to ask somebody what their own system is set to - and
// must not be able to change any of it.
func TestAViewerSeesEverySettingAndCanChangeNone(t *testing.T) {
	server, client, store := settingsServerAs(t, panel.RoleViewer)

	status, body := get(t, client, server.URL+settingsURL())
	if status != http.StatusOK {
		t.Fatalf("a viewer got %d from the settings page; it is a page they may read", status)
	}
	// The page is there and it has content, not an empty shell.
	if !strings.Contains(body, "Ayarlar") {
		t.Error("the settings page did not render for a viewer")
	}

	before, err := store.GetSetting(context.Background(), unguardedKey, settingsSite)
	if err != nil {
		t.Fatal(err)
	}

	// Now submit anyway. The viewer's page drew no control; the server
	// has to refuse the request regardless.
	status, _ = postSetting(t, client, server.URL, url.Values{
		"islem":   {"kaydet"},
		"anahtar": {string(unguardedKey)},
		"deger":   {"viewer bunu yazamamalı"},
	})
	if status == http.StatusOK {
		t.Error("a viewer's POST was accepted; the controls are cosmetic, the check is not")
	}

	after, err := store.GetSetting(context.Background(), unguardedKey, settingsSite)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("a viewer changed a setting: %v -> %v", before, after)
	}
}

// TestAGuardedSettingIsRefusedWithoutTheDeveloperPassword.
//
// The setting carries legal weight - it decides whether visitor
// addresses reach the disk in the clear - and an owner holds every
// capability on their own site. Capability is not the thing that stops
// them here, and that is the whole point of the gate.
func TestAGuardedSettingIsRefusedWithoutTheDeveloperPassword(t *testing.T) {
	server, client, store := settingsServerAs(t, panel.RoleOwner)
	restoreGlobal(t, store, panel.KeyPrivacyIPStorage)

	before, err := store.GetSetting(context.Background(), guardedKey, "")
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"parola yok", "yanlış parola"} {
		t.Run(name, func(t *testing.T) {
			form := url.Values{
				"islem":   {"kaydet"},
				"anahtar": {string(guardedKey)},
				"deger":   {"full"},
			}
			if name == "yanlış parola" {
				form.Set(devgate.FormField, "bu-parola-yanlis")
			}
			status, _ := postSetting(t, client, server.URL, form)
			if status == http.StatusOK {
				t.Error("a guarded setting was changed without a correct developer password")
			}
		})
	}

	after, err := store.GetSetting(context.Background(), guardedKey, "")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("a guarded setting changed anyway: %v -> %v", before, after)
	}
}

// TestTheFormCannotChooseWhichActionItIsAuthorizedFor.
//
// The sharp one. The gate mints an authorization for one named action,
// and the name is derived from the setting the handler is about to
// write - never from the request.
//
// If it came from the request, a form could ask for authorization
// against a harmless setting and spend it on privacy.ip_storage. This
// submits extra fields that look exactly like an attempt to do that, and
// requires that they change nothing.
func TestTheFormCannotChooseWhichActionItIsAuthorizedFor(t *testing.T) {
	server, client, store := settingsServerAs(t, panel.RoleOwner)

	before, err := store.GetSetting(context.Background(), guardedKey, "")
	if err != nil {
		t.Fatal(err)
	}

	status, _ := postSetting(t, client, server.URL, url.Values{
		"islem":   {"kaydet"},
		"anahtar": {string(guardedKey)},
		"deger":   {"full"},
		// Field names a request would use if the server read the action
		// or the scope from it. It does not; these must be inert.
		"eylem":           {panel.GateAction(unguardedKey)},
		"action":          {panel.GateAction(unguardedKey)},
		"actions":         {panel.GateAction(unguardedKey)},
		"scope":           {"global"},
		"site":            {""},
		devgate.FormField: {"bu-parola-yanlis"},
	})
	if status == http.StatusOK {
		t.Error("the request talked its way past the gate")
	}

	after, err := store.GetSetting(context.Background(), guardedKey, "")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("a guarded setting changed: %v -> %v", before, after)
	}
}

// TestAnUnknownSettingIsRefusedRatherThanCreated.
//
// A key this build does not define is not a validation failure to
// explain to somebody - it is a request that did not come from a page
// this build rendered. It must not create a row.
func TestAnUnknownSettingIsRefusedRatherThanCreated(t *testing.T) {
	server, client, _ := settingsServerAs(t, panel.RoleOwner)

	status, _ := postSetting(t, client, server.URL, url.Values{
		"islem":   {"kaydet"},
		"anahtar": {"boyle.bir.ayar.yok"},
		"deger":   {"herhangi"},
	})
	if status == http.StatusOK {
		t.Error("an unknown setting key was accepted")
	}
}

// TestAValueOutsideItsBoundsIsRefusedAndSaysWhy.
//
// The bounds live in the definition, so there is no reason to make
// somebody guess them. A message that only says "invalid" is the kind
// that ends in a support channel.
func TestAValueOutsideItsBoundsIsRefusedAndSaysWhy(t *testing.T) {
	server, client, store := settingsServerAs(t, panel.RoleOwner)
	restoreGlobal(t, store, panel.KeyLogArchiveAfterDays)

	status, body := postSetting(t, client, server.URL, url.Values{
		"islem": {"kaydet"},
		// An unguarded bounded int, chosen deliberately. The first
		// version used logs.retention_days, which carries legal weight
		// and is therefore refused at the gate before its bounds are
		// ever checked - so the test passed through the wrong door and
		// asserted the wrong sentence.
		"anahtar": {"logs.archive_after_days"},
		"deger":   {"999999"},
	})
	if status == http.StatusOK {
		t.Fatal("a value far outside its bounds was accepted")
	}
	// The bound, in the panel's own language, built from the definition.
	// Not the validator's sentence: that one is English and internal,
	// and forwarding it to a browser was the first version's mistake.
	if !strings.Contains(body, "3650") {
		t.Errorf("the refusal does not name the bound that was crossed: %s", noticeOf(body))
	}
	// And the internal wording must not be on the page at all.
	if strings.Contains(body, "must be between") || strings.Contains(body, "invalid setting value") {
		t.Errorf("an internal Go error reached the page: %s", firstLines(body))
	}
}

// firstLines trims a page down to something readable in a failure.
func noticeOf(body string) string {
	i := strings.Index(body, "role=")
	if i < 0 {
		return "(hiç duyuru yok)"
	}
	end := i + 300
	if end > len(body) {
		end = len(body)
	}
	return body[i:end]
}

func firstLines(body string) string {
	if i := strings.Index(body, "uyari"); i >= 0 {
		end := i + 400
		if end > len(body) {
			end = len(body)
		}
		return body[i:end]
	}
	if len(body) > 400 {
		return body[:400] + "..."
	}
	return body
}

// TestTheFormCannotWriteIntoAnotherSitesRow.
//
// The cross-tenant one, and the reason the scope is taken from the
// definition rather than from the request.
//
// site.name is per-site, so the write has to land in a row keyed by a
// site. If that key came from the form, an owner of one site could write
// a value into another customer's row - on a machine where three
// customers share one deployment, which is the arrangement B6 exists to
// protect.
//
// The first version of this suite submitted site="" here and learned
// nothing: an empty value falls back to the right site either way. The
// attack is a *populated* one.
func TestTheFormCannotWriteIntoAnotherSitesRow(t *testing.T) {
	server, client, store := settingsServerAs(t, panel.RoleOwner)
	ctx := context.Background()

	const neighbour = "ayar-komsu"
	before, err := store.GetSetting(ctx, unguardedKey, neighbour)
	if err != nil {
		t.Fatal(err)
	}

	status, _ := postSetting(t, client, server.URL, url.Values{
		"islem":   {"kaydet"},
		"anahtar": {string(unguardedKey)},
		"deger":   {"komsunun satirina yazildi"},
		// The field a handler would read if it trusted the form for
		// scope. It must be inert.
		"site":    {neighbour},
		"site_id": {neighbour},
		"scope":   {"site"},
	})
	_ = status // the write may well succeed - against our own site.

	after, err := store.GetSetting(ctx, unguardedKey, neighbour)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("a form wrote into another site's row: %s went %v -> %v", neighbour, before, after)
	}

	// And it landed where it belongs, so this is testing the routing
	// rather than a write that simply failed.
	ours, err := store.GetSetting(ctx, unguardedKey, settingsSite)
	if err != nil {
		t.Fatal(err)
	}
	if ours != "komsunun satirina yazildi" {
		t.Errorf("the value did not reach our own site either: %v", ours)
	}
}

// TestExtraFieldsInTheFormAreInert.
//
// The handler derives what it is about to do from the definition, never
// from the request: the gate action, and the scope. This submits fields
// named the way a request would name them if it could choose either, and
// requires that the write lands where the *key* says.
//
// # What this test used to claim, and why it was wrong
//
// It asserted that the same submission must be *refused*, on the theory
// that a form naming another action could spend an authorization on a
// setting it was not minted for. Written that way it failed against
// correct code, which is how the mistake surfaced: the password was
// right and the key was right, so there was nothing there to refuse -
// the junk fields are simply ignored.
//
// The claim it was reaching for is real, and it is already proven where
// the lock actually is:
// panel.TestGuardedSettings_AnAuthorizationForOneSettingDoesNotWriteAnother.
// Store.SetGuardedSetting checks the authorization against the key it is
// about to write, so a redirected one is refused there even if a handler
// were wrong about it. The handler's own derivation is the second layer,
// not the first, and the comment on saveSetting now says so.
func TestExtraFieldsInTheFormAreInert(t *testing.T) {
	server, client, store := settingsServerAsOperator(t)
	restoreGlobal(t, store, panel.KeyLogRetentionDays, panel.KeyLogArchiveAfterDays)
	ctx := context.Background()

	otherBefore, err := store.GetSetting(ctx, panel.KeyLogArchiveAfterDays, "")
	if err != nil {
		t.Fatal(err)
	}

	status, body := postSetting(t, client, server.URL, url.Values{
		"islem":           {"kaydet"},
		"anahtar":         {string(panel.KeyLogRetentionDays)},
		"deger":           {"21"},
		devgate.FormField: {testDevPassword},
		// Named after a different setting entirely. If any of these
		// reached the routing, the write would land somewhere else.
		"eylem":   {panel.GateAction(panel.KeyLogArchiveAfterDays)},
		"action":  {panel.GateAction(panel.KeyLogArchiveAfterDays)},
		"actions": {panel.GateAction(panel.KeyLogArchiveAfterDays)},
		"scope":   {"site"},
		"site":    {"baska-site"},
	})
	if status != http.StatusOK {
		t.Fatalf("the request was refused: %d %s", status, noticeOf(body))
	}

	got, err := store.GetSetting(ctx, panel.KeyLogRetentionDays, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != 21 {
		t.Errorf("the named setting did not change: %v", got)
	}

	otherAfter, err := store.GetSetting(ctx, panel.KeyLogArchiveAfterDays, "")
	if err != nil {
		t.Fatal(err)
	}
	if otherAfter != otherBefore {
		t.Errorf("the setting the extra fields named changed too: %v -> %v", otherBefore, otherAfter)
	}
}

// TestTheRightPasswordOnTheRightSettingWorks.
//
// So the test above means something. A suite where the gate refuses
// everything proves the gate is shut, not that it is a gate.
func TestTheRightPasswordOnTheRightSettingWorks(t *testing.T) {
	server, client, store := settingsServerAsOperator(t)
	restoreGlobal(t, store, panel.KeyLogRetentionDays)
	ctx := context.Background()

	// logs.retention_days rather than privacy.ip_storage, and the reason
	// is measured: ip_storage=full is refused by a precondition unless
	// the deployment already carries an ip_hash_key, so a test using it
	// would fail for a reason that has nothing to do with the gate.
	// This one is guarded and has no precondition.
	status, body := postSetting(t, client, server.URL, url.Values{
		"islem":           {"kaydet"},
		"anahtar":         {string(panel.KeyLogRetentionDays)},
		"deger":           {"21"},
		devgate.FormField: {testDevPassword},
	})
	if status != http.StatusOK {
		t.Fatalf("the right password on the right setting answered %d: %s", status, noticeOf(body))
	}

	got, err := store.GetSetting(ctx, panel.KeyLogRetentionDays, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != 21 {
		t.Errorf("the guarded setting did not change: %v", got)
	}
}

// TestAPreconditionSaysSoRatherThanBlamingTheValue.
//
// privacy.ip_storage=full is refused on a deployment whose config file
// carries no ip_hash_key. That refusal is correct and has nothing to do
// with the value being admissible - "full" is one of exactly two the
// setting accepts.
//
// The first version of settingErrorText had no branch for it, so the
// refusal fell through to the bounds message and the page said
//
//	Değer şunlardan biri olmalı: full, masked
//
// about a value that is one of them. A message that sends the reader to
// check the thing that is already correct is worse than a vague one:
// they look, find nothing wrong, and stop believing the page.
//
// Needs an operator and the real password, because the precondition is
// only reached once the gate has been passed.
func TestAPreconditionSaysSoRatherThanBlamingTheValue(t *testing.T) {
	server, client, store := settingsServerAsOperator(t)
	restoreGlobal(t, store, panel.KeyPrivacyIPStorage)

	status, body := postSetting(t, client, server.URL, url.Values{
		"islem":           {"kaydet"},
		"anahtar":         {string(panel.KeyPrivacyIPStorage)},
		"deger":           {"full"},
		devgate.FormField: {testDevPassword},
	})
	if status == http.StatusOK {
		t.Fatal("ip_storage=full was accepted with no ip_hash_key configured")
	}
	if strings.Contains(body, "şunlardan biri olmalı") {
		t.Errorf("the refusal blamed the value, which is admissible: %s", noticeOf(body))
	}
	if !strings.Contains(body, "gereken yapılandırma henüz yok") {
		t.Errorf("the refusal does not say what is actually missing: %s", noticeOf(body))
	}
}

// TestTheAboutBlockIsOnThePageForEveryone.
//
// The credits and the licence are drawn for a viewer, which is the
// weakest role there is - and that is the assertion, not an incidental
// detail. A credits block that only the owner can see is not a credit,
// and the licence a deployment runs under is not something a person
// should have to hold a capability to read.
//
// Rendered, not built. aboutFor returning the right struct proves
// nothing about whether the template draws it: this project has shipped
// a correct value that no page displayed before.
func TestTheAboutBlockIsOnThePageForEveryone(t *testing.T) {
	server, client, _ := settingsServerAs(t, panel.RoleViewer)

	status, body := get(t, client, server.URL+settingsURL())
	if status != http.StatusOK {
		t.Fatalf("a viewer got %d from the settings page", status)
	}

	// The team names are built from their own list and drawn under their
	// own heading, so a template edit can drop that whole block while
	// every check above still passes. Read from the list rather than
	// written out again here: a second copy of the names is a second
	// place to forget one.
	wants := []struct{ text, why string }{
		{"Hakkında", "the section heading"},
		{"ekip üyelerine teşekkürler", "the team heading"},
		{RepositoryURL, "where the source is"},
		{LicenceName, "the licence the deployment runs under"},
		{"CrucibleLAB", "the project"},
		{"Fırat Coşkun", "the developer"},
		{"Claude", "the assistant"},
	}
	for _, c := range team {
		wants = append(wants, struct{ text, why string }{c.Name, "on the team list"})
	}

	for _, want := range wants {
		if !strings.Contains(body, want.text) {
			t.Errorf("the settings page does not carry %q (%s)", want.text, want.why)
		}
	}

	// Each badge resolves to an asset the panel serves. A src that
	// 404s is invisible on the page and would never fail anything else.
	for _, c := range contributors {
		if c.Mark == "" {
			continue
		}
		if !strings.Contains(body, ".svg") {
			t.Fatalf("no badge was drawn at all; %s's mark cannot have rendered", c.Name)
		}
	}

	// And the marks are actually fetchable, as the browser will fetch
	// them. The Content-Security-Policy on this page is
	// "img-src 'self'", so a badge served from anywhere else would be
	// refused silently in a real browser and pass every check above.
	assets, err := ui.LoadAssets()
	if err != nil {
		t.Fatalf("loading the embedded assets: %v", err)
	}
	for _, c := range contributors {
		if c.Mark == "" {
			continue
		}
		markURL := assets.URL(c.Mark)
		if markURL == "" {
			t.Errorf("%s's mark %q is not an embedded asset", c.Name, c.Mark)
			continue
		}
		if !strings.HasPrefix(markURL, "/") {
			t.Errorf("%s's mark resolves to %q, which is not same-origin; the page's "+
				"img-src is 'self' and a browser would refuse it", c.Name, markURL)
		}
		status, _ := get(t, client, server.URL+markURL)
		if status != http.StatusOK {
			t.Errorf("fetching %s's mark at %s returned %d", c.Name, markURL, status)
		}
	}
}

// TestNoSettingIsLostInTheRegrouping.
//
// D4c's actual risk, stated as a test.
//
// The page was one flat list and is now seven collapsible sections. The
// dangerous outcome is not an ugly page: it is a setting that no section
// draws, because that setting does not error, does not log, and does not
// look missing - the customer simply concludes it is not there.
//
// Asked against the rendered page rather than against the struct. The
// grouping code can be right while the template drops a branch, and this
// project has shipped a correct value no page displayed before.
func TestNoSettingIsLostInTheRegrouping(t *testing.T) {
	// The operator, so developer settings are in scope too - a viewer
	// would legitimately see fewer, and this test is about the ones that
	// should be drawn going missing, not about the filter working.
	server, client, _ := settingsServerAsOperator(t)

	status, body := get(t, client, server.URL+settingsURL())
	if status != http.StatusOK {
		t.Fatalf("the settings page returned %d", status)
	}

	// What *should* be drawn for this viewer, not everything.
	//
	// The first version of this compared against the whole registry and
	// failed on seven settings - correctly, in the sense that they were
	// absent, and wrongly, because they are Developer settings and
	// developer mode is off. The page was right and the test was
	// asserting something nobody had claimed.
	//
	// Both halves are checked now, which is stronger than the version
	// that passed would have been: the ones that must be there, and the
	// ones whose absence is the filter working rather than a section
	// being dropped.
	defs := panel.AllDefinitions()
	if len(defs) == 0 {
		t.Fatal("the registry reports no settings; this test would pass by looking " +
			"for nothing")
	}

	drawn := func(def panel.Definition) bool {
		// The key is on every editable row as a hidden form field - the
		// one string that is exactly this setting. A read-only row has
		// no form, so its label is the fallback.
		return strings.Contains(body, `value="`+string(def.Key)+`"`) ||
			strings.Contains(body, def.Label)
	}

	var missing, leaked []string
	var checked int
	for _, def := range defs {
		if def.Developer {
			if drawn(def) {
				leaked = append(leaked, string(def.Key))
			}
			continue
		}
		checked++
		if !drawn(def) {
			missing = append(missing, string(def.Key))
		}
	}
	sort.Strings(missing)
	sort.Strings(leaked)

	if checked == 0 {
		t.Fatal("every definition is a developer setting, so the half of this test " +
			"that matters checked nothing")
	}

	for _, key := range missing {
		t.Errorf("%s is in the registry, is not a developer setting, and is not drawn "+
			"anywhere on the page.\n"+
			"Before D4c the page was one list and everything was shown; now a setting "+
			"whose category is wrong, or whose section the template skips, is silently "+
			"absent - and absent is indistinguishable from never having existed", key)
	}
	for _, key := range leaked {
		t.Errorf("%s is a developer setting and is drawn with developer mode off. "+
			"Grouping away is what that flag is for", key)
	}
}

// TestTheSectionsArriveClosedExceptTheOneThatFailed.
//
// Two halves of one requirement, and the second is why the first is safe.
//
// Collapsed by default is the whole point of D4c: the plan's done
// criterion is "varsayılan görünüm kısa". But a refused save renders a
// red banner at the top of the page, and if every section is closed the
// customer is told something went wrong and shown nothing that could
// have gone wrong. So the section holding the refused setting opens.
//
// Without the second half, the first is a regression dressed as a
// feature.
func TestTheSectionsArriveClosedExceptTheOneThatFailed(t *testing.T) {
	server, client, _ := settingsServerAsOperator(t)

	status, body := get(t, client, server.URL+settingsURL())
	if status != http.StatusOK {
		t.Fatalf("the settings page returned %d", status)
	}
	if !strings.Contains(body, "<details") {
		t.Fatal("the page draws no <details>, so it is still one flat list and " +
			"neither half of this test means anything")
	}
	if strings.Contains(body, "<details class=\"ayar-katagori\" open") {
		t.Error("a section arrives open on a plain GET. The default view is meant to " +
			"be short; a page that opens everything is the flat list with extra markup")
	}

	// Now refuse a save, and require the section that holds it to be
	// open.
	//
	// A non-developer setting, deliberately. The first version used a
	// retention day count, which is a developer setting - so with
	// developer mode off it had no row, no section, and nothing to
	// open. The test failed and the page was right.
	//
	// A timezone that is not a timezone is refused by the definition's
	// own Check, with no gate and no capability question in the way.
	status, body = postSetting(t, client, server.URL, url.Values{
		"islem":   {"kaydet"},
		"anahtar": {string(panel.KeyPanelTimezone)},
		"deger":   {"Avrupa/İstanbul"},
	})
	if status == http.StatusOK {
		t.Fatal("the out-of-bounds value was accepted, so there is no refusal to " +
			"render and this test proves nothing")
	}
	if !strings.Contains(body, "<details class=\"ayar-katagori\" open") {
		t.Error("the save was refused and every section is still closed. The customer " +
			"sees a red banner and no row: the cause of the error is behind a heading " +
			"they have not opened and have no reason to suspect")
	}
}
