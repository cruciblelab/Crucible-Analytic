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
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
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
				form.Set("gelistirici_parolasi", "bu-parola-yanlis")
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
		"eylem":                {panel.GateAction(unguardedKey)},
		"action":               {panel.GateAction(unguardedKey)},
		"actions":              {panel.GateAction(unguardedKey)},
		"scope":                {"global"},
		"site":                 {""},
		"gelistirici_parolasi": {"bu-parola-yanlis"},
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
	server, client, _ := settingsServerAs(t, panel.RoleOwner)

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
