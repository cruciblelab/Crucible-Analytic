//go:build integration

// The customer's door, against a real database.
//
// These cover the sequences a unit test cannot: a password check that
// actually reads a stored argon2id hash, a throttle that actually counts
// rows, a TOTP code that actually has to be a valid one, and a role
// change that actually has to survive the store's transaction.
//
//	docker compose up -d
//	psql "$DSN" -f internal/panel/schema.sql
//	go test -tags integration ./internal/panel/web/ -run TestSignIn -v

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

// testAccountPassword is long enough to pass ValidatePassword and is
// used by every account these tests create.
const testAccountPassword = "bu-bir-test-parolasidir"

// testEmailSuffix marks every account this file creates so the cleanup
// can find them and nothing else. The panel package's suite shares this
// table.
const testEmailSuffix = "@web-giris-testi.invalid"

// makeUser creates an account and returns it, cleaning up afterwards.
func makeUser(t *testing.T, store *panel.Store, local string, superadmin bool) panel.User {
	t.Helper()
	ctx := context.Background()
	hash, err := panel.HashPassword(testAccountPassword)
	if err != nil {
		t.Fatal(err)
	}
	email := local + testEmailSuffix
	user, err := store.CreateUser(ctx, email, local, hash, superadmin)
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", email, err)
	}
	t.Cleanup(func() {
		pool := store.Pool()
		bg := context.Background()
		// Audit rows outlive their actor by design (ON DELETE SET NULL),
		// so they need removing explicitly rather than by cascade.
		_, _ = pool.Exec(bg, `DELETE FROM panel_audit_log WHERE actor_label = $1`, email)
		_, _ = pool.Exec(bg, `DELETE FROM panel_login_attempts WHERE email = $1`, email)
		_, _ = pool.Exec(bg, `DELETE FROM panel_site_members WHERE user_id = $1`, user.ID)
		_, _ = pool.Exec(bg, `DELETE FROM panel_users WHERE id = $1`, user.ID)
	})
	return user
}

// signIn posts the sign-in form and returns the response.
func signIn(t *testing.T, c *http.Client, base, email, password string) *http.Response {
	t.Helper()
	_, body := get(t, c, base+LoginPath)
	token := csrfFrom(t, body)
	resp, err := c.PostForm(base+LoginPath, url.Values{
		"csrf_token": {token},
		"eposta":     {email},
		"parola":     {password},
	})
	if err != nil {
		t.Fatalf("POST %s: %v", LoginPath, err)
	}
	return resp
}

func csrfFrom(t *testing.T, body string) string {
	t.Helper()
	m := csrfPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no CSRF token in the page:\n%s", body)
	}
	return m[1]
}

func TestSignInWithAPassword(t *testing.T) {
	srv, store := setupTestServer(t)
	user := makeUser(t, store, "kullanici", false)

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := newClient(t, server.URL)

	// ---- the wrong password ----
	resp := signIn(t, client, server.URL, user.Email, "yanlis-parola-yeterince-uzun")
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong password answered %d, want 401", resp.StatusCode)
	}

	// ---- an address with no account gets the identical answer ----
	//
	// Same status and the same sentence. Anything that differs here is a
	// way to ask the panel which of a list of addresses has an account,
	// from anywhere on the internet.
	absent := signIn(t, client, server.URL, "yok"+testEmailSuffix, "yanlis-parola-yeterince-uzun")
	absentBody := readBody(t, absent)
	wrongAgain := signIn(t, client, server.URL, user.Email, "hala-yanlis-parola-uzun")
	wrongBody := readBody(t, wrongAgain)
	if absent.StatusCode != wrongAgain.StatusCode {
		t.Errorf("an unknown address answered %d and a wrong password %d; the difference is an oracle",
			absent.StatusCode, wrongAgain.StatusCode)
	}
	if messageOf(absentBody) != messageOf(wrongBody) {
		t.Errorf("the two failures say different things:\n  unknown: %q\n  wrong:   %q",
			messageOf(absentBody), messageOf(wrongBody))
	}

	// ---- the right password ----
	ok := signIn(t, client, server.URL, user.Email, testAccountPassword)
	ok.Body.Close()
	if ok.StatusCode != http.StatusSeeOther {
		t.Fatalf("a correct password answered %d, want 303", ok.StatusCode)
	}
	if got := ok.Header.Get("Location"); got != "/" {
		t.Errorf("signing in landed on %q", got)
	}

	// ---- and the session actually works ----
	status, body := get(t, client, server.URL+AccountPath)
	if status != http.StatusOK {
		t.Fatalf("the account page answered %d after signing in", status)
	}
	if !strings.Contains(body, user.Email) {
		t.Error("the account page does not show the signed-in address")
	}

	// ---- signing out ----
	logout, err := client.PostForm(server.URL+LogoutPath, url.Values{
		"csrf_token": {csrfFrom(t, body)},
	})
	if err != nil {
		t.Fatal(err)
	}
	logout.Body.Close()
	if logout.StatusCode != http.StatusSeeOther {
		t.Errorf("signing out answered %d", logout.StatusCode)
	}
	if status, _ := get(t, client, server.URL+AccountPath); status != http.StatusSeeOther {
		t.Errorf("the account page still answered %d after signing out", status)
	}
}

// TestSignInSendsYouWhereYouWereGoing covers the destination parameter
// end to end, including the fact that a hostile one is dropped rather
// than followed.
func TestSignInSendsYouWhereYouWereGoing(t *testing.T) {
	srv, store := setupTestServer(t)
	user := makeUser(t, store, "yonlendirme", false)

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := newClient(t, server.URL)

	// Asking for a page while signed out redirects to the form, carrying
	// the destination.
	resp, err := client.Get(server.URL + AccountPath)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Location"); got != "/giris?next=%2Fhesap" {
		t.Fatalf("Location = %q", got)
	}

	// Signing in from that form lands on the page that was asked for.
	_, body := get(t, client, server.URL+LoginPath+"?next=%2Fhesap")
	landed, err := client.PostForm(server.URL+LoginPath, url.Values{
		"csrf_token": {csrfFrom(t, body)},
		"eposta":     {user.Email},
		"parola":     {testAccountPassword},
		"next":       {"/hesap"},
	})
	if err != nil {
		t.Fatal(err)
	}
	landed.Body.Close()
	if got := landed.Header.Get("Location"); got != AccountPath {
		t.Errorf("signing in landed on %q, want %q", got, AccountPath)
	}

	// A hostile destination is dropped, not followed. Posted directly,
	// because a browser would never produce this - an attacker would.
	client2 := newClient(t, server.URL)
	_, body2 := get(t, client2, server.URL+LoginPath)
	hostile, err := client2.PostForm(server.URL+LoginPath, url.Values{
		"csrf_token": {csrfFrom(t, body2)},
		"eposta":     {user.Email},
		"parola":     {testAccountPassword},
		"next":       {"//evil.test/"},
	})
	if err != nil {
		t.Fatal(err)
	}
	hostile.Body.Close()
	if got := hostile.Header.Get("Location"); got != "/" {
		t.Errorf("a hostile destination produced Location = %q; it must be dropped", got)
	}
}

// TestSignInIsThrottled proves the counter is reached from the handler,
// which is the part a store test cannot show.
func TestSignInIsThrottled(t *testing.T) {
	srv, store := setupTestServer(t)
	user := makeUser(t, store, "kisitli", false)

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := newClient(t, server.URL)

	// The per-account limit is 8 in a fifteen-minute window.
	var last *http.Response
	for i := 0; i < 12; i++ {
		last = signIn(t, client, server.URL, user.Email, "yanlis-parola-yeterince-uzun")
		if last.StatusCode == http.StatusTooManyRequests {
			break
		}
		last.Body.Close()
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("twelve wrong passwords never produced a 429 (last %d)", last.StatusCode)
	}
	body := readBody(t, last)
	if !strings.Contains(body, "dakika") {
		t.Errorf("the refusal does not say how long to wait:\n%s", messageOf(body))
	}

	// The block holds even for the correct password: a throttle that a
	// correct guess walks through is not a throttle.
	blocked := signIn(t, client, server.URL, user.Email, testAccountPassword)
	blocked.Body.Close()
	if blocked.StatusCode != http.StatusTooManyRequests {
		t.Errorf("the correct password answered %d while throttled", blocked.StatusCode)
	}
}

// TestSecondFactorIsRequiredAndCannotBeSkipped is the important one.
//
// A password check that passes and then lets the browser go straight to
// a page is a two-factor feature that does nothing. So the pending state
// is asserted from both sides: the code form opens, and every other page
// stays shut.
func TestSecondFactorIsRequiredAndCannotBeSkipped(t *testing.T) {
	srv, store := setupTestServer(t)
	user := makeUser(t, store, "ikifaktor", false)
	ctx := context.Background()

	key, err := panel.NewTOTPSecret(user.Email)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTOTPSecret(ctx, user.ID, key.Secret()); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := newClient(t, server.URL)

	// The password alone sends us to the code form, not to a session.
	resp := signIn(t, client, server.URL, user.Email, testAccountPassword)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the password step answered %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != SecondFactorPath {
		t.Fatalf("the password step went to %q, want the code form", got)
	}

	// Half-finished is not signed in. This is the assertion that matters:
	// the pending state must not open anything.
	for _, path := range []string{"/", AccountPath} {
		if status, _ := get(t, client, server.URL+path); status != http.StatusSeeOther {
			t.Errorf("%s answered %d with only a password proved; want a redirect to sign in", path, status)
		}
	}

	// A wrong code is refused.
	_, body := get(t, client, server.URL+SecondFactorPath)
	wrong, err := client.PostForm(server.URL+SecondFactorPath, url.Values{
		"csrf_token": {csrfFrom(t, body)},
		"kod":        {"000000"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wrong.Body.Close()
	if wrong.StatusCode != http.StatusUnauthorized {
		t.Errorf("a wrong code answered %d, want 401", wrong.StatusCode)
	}

	// The right code completes the sign-in.
	code, err := totp.GenerateCode(key.Secret(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, body = get(t, client, server.URL+SecondFactorPath)
	done, err := client.PostForm(server.URL+SecondFactorPath, url.Values{
		"csrf_token": {csrfFrom(t, body)},
		"kod":        {code},
	})
	if err != nil {
		t.Fatal(err)
	}
	done.Body.Close()
	if done.StatusCode != http.StatusSeeOther {
		t.Fatalf("the correct code answered %d", done.StatusCode)
	}
	if status, _ := get(t, client, server.URL+AccountPath); status != http.StatusOK {
		t.Errorf("the account page answered %d after a correct code", status)
	}

	// And the same code cannot be used again. Replay is refused by the
	// store; what this proves is that the handler surfaces it rather than
	// treating a used code as a fresh success.
	client2 := newClient(t, server.URL)
	again := signIn(t, client2, server.URL, user.Email, testAccountPassword)
	again.Body.Close()
	_, body2 := get(t, client2, server.URL+SecondFactorPath)
	replay, err := client2.PostForm(server.URL+SecondFactorPath, url.Values{
		"csrf_token": {csrfFrom(t, body2)},
		"kod":        {code},
	})
	if err != nil {
		t.Fatal(err)
	}
	replayBody := readBody(t, replay)
	if replay.StatusCode != http.StatusUnauthorized {
		t.Errorf("a replayed code answered %d, want 401", replay.StatusCode)
	}
	if !strings.Contains(replayBody, "zaten kullanıldı") {
		t.Errorf("a replayed code did not get its own sentence: %q", messageOf(replayBody))
	}
}

// readBody reads and closes a response.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String()
}

// messageOf extracts the alert paragraph, so a comparison between two
// failures is about the sentence rather than about the whole document.
func messageOf(body string) string {
	const open = `role="alert">`
	i := strings.Index(body, open)
	if i < 0 {
		return ""
	}
	rest := body[i+len(open):]
	j := strings.Index(rest, "<")
	if j < 0 {
		return rest
	}
	return rest[:j]
}
