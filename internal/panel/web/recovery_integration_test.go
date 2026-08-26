//go:build integration

package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// recoveryFor gives an account a set of codes and returns them.
func recoveryFor(t *testing.T, store *panel.Store, user panel.User) []string {
	t.Helper()
	codes, err := store.GenerateRecoveryCodes(context.Background(), user.ID, 0)
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	return codes
}

// post sends a form and returns the status and body without following
// the redirect, so a test can assert on where it was sent.
func postNoFollow(t *testing.T, c *http.Client, target string, form url.Values) (*http.Response, string) {
	t.Helper()
	_, page := get(t, c, target)
	form.Set("csrf_token", csrfFrom(t, page))

	prev := c.CheckRedirect
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	defer func() { c.CheckRedirect = prev }()

	resp, err := c.PostForm(target, form)
	if err != nil {
		t.Fatalf("posting %s: %v", target, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, string(body)
}

// TestRecovery_TheOwnerGetsBackInWithoutAnybodyElse is the phase, end to
// end over HTTP: somebody who cannot sign in uses a code and is inside,
// with no operator awake and no email anywhere.
func TestRecovery_TheOwnerGetsBackInWithoutAnybodyElse(t *testing.T) {
	srv, store := setupTestServer(t)
	user := makeUser(t, store, "kurtarma-web", false)
	codes := recoveryFor(t, store, user)

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := newClient(t, server.URL)

	// The sign-in page has to offer the way out, or nobody stuck on it
	// will find this.
	_, login := get(t, client, server.URL+LoginPath)
	if !strings.Contains(login, RecoveryPath) {
		t.Error("the sign-in page does not link to recovery; somebody locked out cannot find it")
	}

	const newPassword = "kurtarilmis-parola-2026"
	resp, _ := postNoFollow(t, client, server.URL+RecoveryPath, url.Values{
		"eposta":             {user.Email},
		"kod":                {panel.FormatRecoveryCode(codes[0])},
		"yeni_parola":        {newPassword},
		"yeni_parola_tekrar": {newPassword},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("recovery answered %d, want a redirect into the panel", resp.StatusCode)
	}

	// Signed in already: they proved who they are with a single-use
	// credential and set a password four seconds ago.
	status, body := get(t, client, server.URL+AccountPath)
	if status != http.StatusOK {
		t.Fatalf("the account page answered %d after recovery; they were not signed in", status)
	}
	if !strings.Contains(body, user.Email) {
		t.Error("the account page does not show the recovered account")
	}

	// And the new password is the one that works now.
	fresh := newClient(t, server.URL)
	if _, err := store.UserByEmail(context.Background(), user.Email); err != nil {
		t.Fatal(err)
	}
	resp, _ = postNoFollow(t, fresh, server.URL+LoginPath, url.Values{
		"eposta": {user.Email}, "parola": {newPassword},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("signing in with the new password answered %d, want a redirect", resp.StatusCode)
	}
}

// TestRecovery_AWrongCodeAndAnUnknownAddressAnswerIdentically.
//
// This page is reachable without signing in, which is what makes it
// worth asking: any difference between the two answers tells anybody on
// the internet which addresses have accounts here.
func TestRecovery_AWrongCodeAndAnUnknownAddressAnswerIdentically(t *testing.T) {
	srv, store := setupTestServer(t)
	user := makeUser(t, store, "kurtarma-ayni", false)
	recoveryFor(t, store, user)

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	const password = "denenen-yeni-parola-11"

	form := func(email, code string) url.Values {
		return url.Values{
			"eposta": {email}, "kod": {code},
			"yeni_parola": {password}, "yeni_parola_tekrar": {password},
		}
	}

	wrongCode, wrongBody := postNoFollow(t, newClient(t, server.URL),
		server.URL+RecoveryPath, form(user.Email, "AAAA-BBBB-CCCC"))
	noAccount, noBody := postNoFollow(t, newClient(t, server.URL),
		server.URL+RecoveryPath, form("kimse@yok-boyle.invalid", "AAAA-BBBB-CCCC"))

	if wrongCode.StatusCode != noAccount.StatusCode {
		t.Errorf("a wrong code answered %d and an unknown address %d; the difference is an oracle",
			wrongCode.StatusCode, noAccount.StatusCode)
	}
	// Compared on the message rather than the whole page: the form
	// echoes the address back, so the bodies differ by design.
	msg := srv.Renderer.Catalogs().Base().T("kurtarma.hata.gecersiz")
	if !strings.Contains(wrongBody, shown(msg)) || !strings.Contains(noBody, shown(msg)) {
		t.Error("the two refusals do not both show the one refusal sentence")
	}
}

// TestRecovery_AMistypedNewPasswordDoesNotSpendTheCode.
//
// The two form-level checks happen before the credential is touched. A
// customer who mistyped their new password twice has not guessed at
// anything, and burning one of their eight escapes for it would be a
// punishment for a typo.
func TestRecovery_AMistypedNewPasswordDoesNotSpendTheCode(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	user := makeUser(t, store, "kurtarma-yazim-web", false)
	codes := recoveryFor(t, store, user)

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := newClient(t, server.URL)

	resp, _ := postNoFollow(t, client, server.URL+RecoveryPath, url.Values{
		"eposta":             {user.Email},
		"kod":                {panel.FormatRecoveryCode(codes[0])},
		"yeni_parola":        {"birinci-uzun-parola-1"},
		"yeni_parola_tekrar": {"ikinci-uzun-parola-2"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("two different new passwords answered %d, want 400", resp.StatusCode)
	}
	if left, err := store.CountRecoveryCodes(ctx, user.ID); err != nil {
		t.Fatal(err)
	} else if left != panel.RecoveryCodeCount {
		t.Errorf("%d codes left after a mistyped password, want %d - the typo spent one",
			left, panel.RecoveryCodeCount)
	}
}

// TestRecovery_AnAccountWithASecondFactorStillHasToSatisfyIt.
//
// The property that keeps a recovery code from being a way around the
// second factor for anybody who finds one. Clearing it is a choice the
// person makes on the form, not something the code does by itself.
func TestRecovery_AnAccountWithASecondFactorStillHasToSatisfyIt(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	user := makeUser(t, store, "kurtarma-2fa-web", false)
	codes := recoveryFor(t, store, user)
	if err := store.SetTOTPSecret(ctx, user.ID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := newClient(t, server.URL)

	const password = "iki-faktor-kalsin-123"
	resp, _ := postNoFollow(t, client, server.URL+RecoveryPath, url.Values{
		"eposta":             {user.Email},
		"kod":                {panel.FormatRecoveryCode(codes[0])},
		"yeni_parola":        {password},
		"yeni_parola_tekrar": {password},
	})
	if got := resp.Header.Get("Location"); got != SecondFactorPath {
		t.Errorf("recovery led to %q, want the second-factor page at %q", got, SecondFactorPath)
	}

	// Not signed in yet: the second factor is still owed.
	if status, _ := get(t, client, server.URL+AccountPath); status == http.StatusOK {
		t.Error("a recovery code let somebody past a second factor they did not ask to clear")
	}

	// And the account still has it.
	after, err := store.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.HasTOTP() {
		t.Error("the second factor was cleared without being asked for")
	}
}
