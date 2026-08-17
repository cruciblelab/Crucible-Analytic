//go:build integration

// The account page against a real database, with the emphasis on the
// two things that are security properties rather than features: that
// changing a password costs the current one, and that an abandoned
// second-factor enrolment leaves the account able to sign in.

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

func TestChangingAPasswordCostsTheCurrentOne(t *testing.T) {
	srv, store := setupTestServer(t)
	user := makeUser(t, store, "parola-degis", false)
	ctx := context.Background()

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := signedIn(t, server.URL, user.Email)
	page := server.URL + AccountPath

	const newPassword = "yepyeni-bir-parola-burada"

	// ---- without the current password, nothing happens ----
	//
	// This is the whole point of the field. A session cookie copied off a
	// shared machine gets somebody the customer's numbers; without this,
	// it also gets them the account permanently.
	status, body := post(t, client, page, url.Values{
		"islem":              {"parola"},
		"mevcut_parola":      {"bu-parola-yanlis-ama-uzun"},
		"yeni_parola":        {newPassword},
		"yeni_parola_tekrar": {newPassword},
	})
	if status != http.StatusBadRequest {
		t.Errorf("a wrong current password answered %d", status)
	}
	if !strings.Contains(body, "Mevcut parolanız doğrulanamadı") {
		t.Errorf("the refusal does not say why: %q", messageOf(body))
	}
	// And the old password still works, which is what "nothing happened"
	// actually means.
	fresh, err := store.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := panel.VerifyPassword(fresh.PasswordHash, testAccountPassword); !ok {
		t.Fatal("the password changed despite the refusal")
	}

	// ---- mismatched repeat ----
	status, body = post(t, client, page, url.Values{
		"islem":              {"parola"},
		"mevcut_parola":      {testAccountPassword},
		"yeni_parola":        {newPassword},
		"yeni_parola_tekrar": {newPassword + "farkli"},
	})
	if status != http.StatusBadRequest || !strings.Contains(body, "iki alanda aynı değil") {
		t.Errorf("a mismatched repeat answered %d: %q", status, messageOf(body))
	}

	// ---- too short ----
	status, body = post(t, client, page, url.Values{
		"islem":              {"parola"},
		"mevcut_parola":      {testAccountPassword},
		"yeni_parola":        {"kisa"},
		"yeni_parola_tekrar": {"kisa"},
	})
	if status != http.StatusBadRequest || !strings.Contains(body, "en az 12") {
		t.Errorf("a short password answered %d: %q", status, messageOf(body))
	}

	// ---- and with everything right, it changes ----
	status, body = post(t, client, page, url.Values{
		"islem":              {"parola"},
		"mevcut_parola":      {testAccountPassword},
		"yeni_parola":        {newPassword},
		"yeni_parola_tekrar": {newPassword},
	})
	if status != http.StatusOK {
		t.Fatalf("the change answered %d: %q", status, messageOf(body))
	}
	changed, err := store.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := panel.VerifyPassword(changed.PasswordHash, newPassword); !ok {
		t.Fatal("the new password does not verify")
	}

	// The new one signs in and the old one does not.
	other := newClient(t, server.URL)
	resp := signIn(t, other, server.URL, user.Email, newPassword)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("the new password answered %d at sign-in", resp.StatusCode)
	}
	stale := newClient(t, server.URL)
	resp = signIn(t, stale, server.URL, user.Email, testAccountPassword)
	resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Error("the old password still signs in")
	}
}

// TestAbandonedTOTPEnrolmentLeavesTheAccountUsable is the reason the
// secret lives in the session.
//
// Writing it to the user row before a code is verified produces the one
// state this panel cannot repair by itself: an account demanding codes
// from an authenticator that never finished scanning. So the test walks
// exactly that path - start, do not confirm - and then signs in.
func TestAbandonedTOTPEnrolmentLeavesTheAccountUsable(t *testing.T) {
	srv, store := setupTestServer(t)
	user := makeUser(t, store, "yarim-2fa", false)
	ctx := context.Background()

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := signedIn(t, server.URL, user.Email)
	page := server.URL + AccountPath

	status, body := post(t, client, page, url.Values{"islem": {"2fa-basla"}})
	if status != http.StatusOK {
		t.Fatalf("starting enrolment answered %d: %q", status, messageOf(body))
	}
	if !strings.Contains(body, TOTPQRPath) {
		t.Error("the page does not offer the QR code")
	}

	// Nothing was written.
	fresh, err := store.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.HasTOTP() {
		t.Fatal("the secret was stored before any code proved the app had it")
	}

	// The QR is served, same-origin, and never cached.
	resp, err := client.Get(server.URL + TOTPQRPath)
	if err != nil {
		t.Fatal(err)
	}
	qr := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the QR endpoint answered %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type = %q", got)
	}
	if !strings.Contains(resp.Header.Get("Cache-Control"), "no-store") {
		t.Errorf("the QR is cacheable: %q", resp.Header.Get("Cache-Control"))
	}
	if !strings.HasPrefix(qr, "\x89PNG") {
		t.Error("the QR endpoint did not return a PNG")
	}

	// Walking away and signing in again must work.
	abandoned := newClient(t, server.URL)
	again := signIn(t, abandoned, server.URL, user.Email, testAccountPassword)
	again.Body.Close()
	if again.StatusCode != http.StatusSeeOther {
		t.Fatalf("signing in after an abandoned enrolment answered %d", again.StatusCode)
	}
	if got := again.Header.Get("Location"); got == SecondFactorPath {
		t.Fatal("an abandoned enrolment left the account asking for codes")
	}

	// And a session with no enrolment in progress gets no image, rather
	// than a blank one that looks like a broken page.
	noQR, err := abandoned.Get(server.URL + TOTPQRPath)
	if err != nil {
		t.Fatal(err)
	}
	noQR.Body.Close()
	if noQR.StatusCode != http.StatusNotFound {
		t.Errorf("the QR endpoint answered %d with nothing being enrolled", noQR.StatusCode)
	}
}

// TestTOTPEnrolmentAndRemoval walks the whole feature: turn it on with a
// real code, prove sign-in now asks for one, then turn it off - which
// costs the password, because it lowers a defence.
func TestTOTPEnrolmentAndRemoval(t *testing.T) {
	srv, store := setupTestServer(t)
	user := makeUser(t, store, "tam-2fa", false)
	ctx := context.Background()

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := signedIn(t, server.URL, user.Email)
	page := server.URL + AccountPath

	status, body := post(t, client, page, url.Values{"islem": {"2fa-basla"}})
	if status != http.StatusOK {
		t.Fatalf("starting enrolment answered %d", status)
	}
	secret := manualKeyFrom(t, body)

	// A wrong code confirms nothing.
	status, body = post(t, client, page, url.Values{
		"islem": {"2fa-onayla"}, "kod": {"000000"},
	})
	if status != http.StatusBadRequest {
		t.Errorf("a wrong confirmation code answered %d", status)
	}
	if fresh, _ := store.UserByID(ctx, user.ID); fresh.HasTOTP() {
		t.Fatal("a wrong code turned two-factor on")
	}

	// The right one does.
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	status, body = post(t, client, page, url.Values{
		"islem": {"2fa-onayla"}, "kod": {code},
	})
	if status != http.StatusOK {
		t.Fatalf("the correct code answered %d: %q", status, messageOf(body))
	}
	fresh, err := store.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.HasTOTP() {
		t.Fatal("two-factor was not turned on")
	}

	// Signing in now asks for a code.
	next := newClient(t, server.URL)
	resp := signIn(t, next, server.URL, user.Email, testAccountPassword)
	resp.Body.Close()
	if got := resp.Header.Get("Location"); got != SecondFactorPath {
		t.Errorf("sign-in went to %q, want the code form", got)
	}

	// ---- removal costs the password ----
	status, body = post(t, client, page, url.Values{
		"islem": {"2fa-kapat"}, "mevcut_parola": {"yanlis-parola-ama-yeterince-uzun"},
	})
	if status != http.StatusBadRequest {
		t.Errorf("removing two-factor without the password answered %d", status)
	}
	if fresh, _ := store.UserByID(ctx, user.ID); !fresh.HasTOTP() {
		t.Fatal("two-factor came off without the password")
	}

	status, body = post(t, client, page, url.Values{
		"islem": {"2fa-kapat"}, "mevcut_parola": {testAccountPassword},
	})
	if status != http.StatusOK {
		t.Fatalf("removing two-factor answered %d: %q", status, messageOf(body))
	}
	if fresh, _ := store.UserByID(ctx, user.ID); fresh.HasTOTP() {
		t.Fatal("two-factor is still on")
	}
}

// TestDeveloperModeNeedsTheCapabilityNotJustTheForm: the toggle is only
// drawn for somebody allowed to use it, and drawing decisions are not
// authorisation.
func TestDeveloperModeNeedsTheCapabilityNotJustTheForm(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "gelistirici-testi"

	viewer := makeUser(t, store, "izleyici-gelistirici", false)
	admin := makeUser(t, store, "yonetici-gelistirici", false)
	if err := store.AddMember(ctx, site, viewer.ID, roleOf("viewer"), nil); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, site, admin.ID, roleOf("admin"), nil); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	page := server.URL + AccountPath

	// The viewer's page does not offer it...
	viewerClient := signedIn(t, server.URL, viewer.Email)
	_, body := get(t, viewerClient, page)
	if strings.Contains(body, "Geliştirici modunu aç") {
		t.Error("the viewer's account page offers developer mode")
	}
	// ...and posting it anyway is refused.
	status, body := post(t, viewerClient, page, url.Values{
		"islem": {"gelistirici"}, "acik": {"1"},
	})
	if status != http.StatusBadRequest {
		t.Errorf("a viewer turning on developer mode answered %d", status)
	}
	if fresh, _ := store.UserByID(ctx, viewer.ID); fresh.DeveloperMode {
		t.Fatal("a viewer acquired developer mode by posting a form")
	}

	// An admin may.
	adminClient := signedIn(t, server.URL, admin.Email)
	status, body = post(t, adminClient, page, url.Values{
		"islem": {"gelistirici"}, "acik": {"1"},
	})
	if status != http.StatusOK {
		t.Fatalf("an admin turning on developer mode answered %d: %q", status, messageOf(body))
	}
	if fresh, _ := store.UserByID(ctx, admin.ID); !fresh.DeveloperMode {
		t.Fatal("developer mode was not turned on")
	}
}

// manualKeyFrom pulls the secret out of the enrolment page, which is
// where a person who cannot scan the QR would read it.
func manualKeyFrom(t *testing.T, body string) string {
	t.Helper()
	const open = `<code class="secret">`
	i := strings.Index(body, open)
	if i < 0 {
		t.Fatalf("no manual key on the enrolment page:\n%s", body)
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, "</code>")
	if j < 0 {
		t.Fatal("the manual key is not closed")
	}
	return rest[:j]
}
