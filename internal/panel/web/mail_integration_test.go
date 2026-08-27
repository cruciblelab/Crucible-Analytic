//go:build integration

// The outgoing mail page, against a real database and a real SMTP
// server.
//
// The unit tests decide how a diagnosis becomes a sentence. What only a
// live run can prove is the sequence: an owner types an account, it is
// stored encrypted, the test button has an actual conversation with an
// actual server, and what the page then says matches what happened on
// the wire.

package web

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/mail"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/sealed"
)

const mailSite = "posta-testi"

// mailServer sets up a panel with a working encryption key and an owner
// signed in.
func mailServer(t *testing.T) (*httptest.Server, *http.Client, *panel.Store, panel.User) {
	t.Helper()

	srv, store := setupTestServer(t)

	var raw [sealed.KeySize]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	key, err := sealed.ParseKey(hex.EncodeToString(raw[:]))
	if err != nil {
		t.Fatal(err)
	}
	srv.SecretKey = key

	ctx := context.Background()
	owner := makeUser(t, store, "posta-sahip", false)
	if err := store.AddMember(ctx, mailSite, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	t.Cleanup(func() {
		_, _ = store.Pool().Exec(context.Background(), `DELETE FROM panel_smtp`)
	})

	server := httptest.NewServer(srv.Handler())
	t.Cleanup(server.Close)
	return server, signedIn(t, server.URL, owner.Email), store, owner
}

// saveAccount fills in the form the way somebody would.
func saveAccount(t *testing.T, c *http.Client, base string, form url.Values) (int, string) {
	t.Helper()
	_, page := get(t, c, base+MailPath)
	token := csrfFrom(t, page)
	form.Set("islem", "kaydet")
	return postWithToken(t, c, base+MailPath, token, form)
}

// TestTheOwnerConfiguresMailAndTheKeyNeverLeaves is the whole flow.
func TestTheOwnerConfiguresMailAndTheKeyNeverLeaves(t *testing.T) {
	server, client, store, _ := mailServer(t)
	ctx := context.Background()

	// A fresh deployment says so, and offers the defaults rather than a
	// blank port box.
	status, body := get(t, client, server.URL+MailPath)
	if status != http.StatusOK {
		t.Fatalf("the mail page answered %d", status)
	}
	if !strings.Contains(body, `value="587"`) {
		t.Error("the port box does not default to 587")
	}

	const password = "TahminEdilemezBirSifre-8812"
	status, body = saveAccount(t, client, server.URL, url.Values{
		"sunucu":       {"smtp.ornek.com"},
		"port":         {"587"},
		"sifreleme":    {"starttls"},
		"kullanici":    {"panel@ornek.com"},
		"sifre":        {password},
		"gonderen":     {"panel@ornek.com"},
		"gonderen_ad":  {"Crucible Analytic"},
		"acik":         {"1"},
		"deneme_adres": {""},
	})
	if status != http.StatusOK {
		t.Fatalf("saving answered %d:\n%s", status, body)
	}

	// The password must not come back to the browser, in any form. This
	// is the assertion the whole two-read split in internal/panel exists
	// for, checked where it actually matters: in the bytes sent.
	if strings.Contains(body, password) {
		t.Fatal("the saved password appears in the page")
	}
	// Nor on a fresh render.
	_, body = get(t, client, server.URL+MailPath)
	if strings.Contains(body, password) {
		t.Fatal("the saved password appears when the page is loaded again")
	}
	// The page does say that one is stored, which is the only thing
	// about it a reader needs.
	if !strings.Contains(body, "Kayıtlı bir parola var") {
		t.Error("the page does not say a password is stored")
	}

	// And it is in the database encrypted.
	var stored string
	if err := store.Pool().QueryRow(ctx,
		`SELECT password_sealed FROM panel_smtp WHERE id = 1`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "" || strings.Contains(stored, password) {
		t.Fatalf("password_sealed = %q", stored)
	}
}

// TestTheTestButtonHasARealConversation points the panel at a real SMTP
// server and checks the page reports what actually happened.
//
// The fake server lives in internal/mail's tests, so this one runs a
// small one of its own - deliberately, because what is under test here
// is the panel's reporting rather than the SMTP client, and a server
// that behaves differently from that package's is a better test of the
// wiring than a shared one.
func TestTheTestButtonHasARealConversation(t *testing.T) {
	server, client, store, _ := mailServer(t)
	ctx := context.Background()

	smtpAddr := startPlainSMTP(t)
	host, portText, _ := strings.Cut(smtpAddr, ":")

	status, body := saveAccount(t, client, server.URL, url.Values{
		"sunucu":    {host},
		"port":      {portText},
		"sifreleme": {"starttls"},
		"gonderen":  {"panel@ornek.com"},
		"acik":      {"1"},
	})
	if status != http.StatusOK {
		t.Fatalf("saving answered %d:\n%s", status, body)
	}

	_, page := get(t, client, server.URL+MailPath)
	token := csrfFrom(t, page)
	status, body = postWithToken(t, client, server.URL+MailPath, token, url.Values{
		"islem":     {"dogrula"},
		"sunucu":    {host},
		"port":      {portText},
		"sifreleme": {"starttls"},
		"gonderen":  {"panel@ornek.com"},
		"acik":      {"1"},
	})

	// The server offers no STARTTLS and this account has no credentials,
	// which is the anonymous-relay case: allowed, and reported as
	// working rather than as a failure.
	if status != http.StatusOK {
		t.Fatalf("the connection test answered %d:\n%s", status, body)
	}
	if !strings.Contains(body, "Bağlantı kuruldu") {
		t.Errorf("the page does not report a successful connection:\n%s", body)
	}

	// And the outcome is in the row, which is what answers "why did
	// nobody get the invitation" three weeks later.
	var ok bool
	var diagnosis string
	if err := store.Pool().QueryRow(ctx,
		`SELECT verified_ok, verified_diagnosis FROM panel_smtp WHERE id = 1`).Scan(&ok, &diagnosis); err != nil {
		t.Fatal(err)
	}
	if !ok || diagnosis != "" {
		t.Errorf("verified_ok = %v, diagnosis = %q", ok, diagnosis)
	}
}

// A server that is not there produces the diagnosis for it, in the
// reader's language, with no mention of a password.
func TestAnUnreachableServerIsNamedAsOne(t *testing.T) {
	server, client, store, _ := mailServer(t)
	ctx := context.Background()

	// A port nothing is listening on: bound, learned, and given back.
	closed := closedPort(t)

	status, body := saveAccount(t, client, server.URL, url.Values{
		"sunucu":    {"127.0.0.1"},
		"port":      {strconv.Itoa(closed)},
		"sifreleme": {"starttls"},
		"gonderen":  {"panel@ornek.com"},
		"acik":      {"1"},
	})
	if status != http.StatusOK {
		t.Fatalf("saving answered %d:\n%s", status, body)
	}

	_, page := get(t, client, server.URL+MailPath)
	token := csrfFrom(t, page)
	status, body = postWithToken(t, client, server.URL+MailPath, token, url.Values{
		"islem":     {"dogrula"},
		"sunucu":    {"127.0.0.1"},
		"port":      {strconv.Itoa(closed)},
		"sifreleme": {"starttls"},
		"gonderen":  {"panel@ornek.com"},
		"acik":      {"1"},
	})
	if status != http.StatusBadRequest {
		t.Errorf("a failed test answered %d, want 400", status)
	}
	if !strings.Contains(body, "Sunucuya hiç bağlanılamadı") {
		t.Errorf("the page does not name the failure:\n%s", body)
	}
	// The advice under it is the useful half - the ports a hosting
	// provider blocks are the usual cause.
	if !strings.Contains(body, "giden 25 portunu") {
		t.Error("the page gives the diagnosis without the advice")
	}

	var diagnosis string
	if err := store.Pool().QueryRow(ctx,
		`SELECT verified_diagnosis FROM panel_smtp WHERE id = 1`).Scan(&diagnosis); err != nil {
		t.Fatal(err)
	}
	if diagnosis != string(mail.DiagUnreachable) {
		t.Errorf("stored diagnosis = %q, want %q", diagnosis, mail.DiagUnreachable)
	}
}

// TestOnlyAnOwnerReachesTheMailPage, and a developer is refused by kind.
//
// The second half is the one that matters. Whoever controls the outgoing
// mail server receives every password-reset link the panel sends, so a
// developer who could point mail at a host they control could become any
// user on the deployment. A redeemed developer link carries Superadmin,
// so the ownership question alone would have said yes.
func TestOnlyAnOwnerReachesTheMailPage(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()

	owner := makeUser(t, store, "posta-yetki-sahip", false)
	if err := store.AddMember(ctx, mailSite, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	// An admin on the same site: entitled to a great deal, and not to
	// this.
	admin := makeUser(t, store, "posta-yetki-yonetici", false)
	if err := store.AddMember(ctx, mailSite, admin.ID, panel.RoleAdmin, nil); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	t.Cleanup(func() {
		_, _ = store.Pool().Exec(context.Background(), `DELETE FROM panel_smtp`)
	})

	if status, _ := get(t, signedIn(t, server.URL, owner.Email), server.URL+MailPath); status != http.StatusOK {
		t.Errorf("the owner got %d from the mail page, want 200", status)
	}
	if status, _ := get(t, signedIn(t, server.URL, admin.Email), server.URL+MailPath); status != http.StatusForbidden {
		t.Errorf("an admin got %d from the mail page, want 403", status)
	}

	// A developer session, obtained the only way one can be: an approved
	// link, redeemed.
	liveToken, liveReq := requestAccess(t, store, "posta-yetki")
	if err := store.ApproveDevAccess(ctx, liveReq.ID, owner); err != nil {
		t.Fatal(err)
	}
	dev := newClient(t, server.URL)
	if status, _ := get(t, dev, server.URL+DevAccessPathPrefix+liveToken); status != http.StatusSeeOther {
		t.Fatalf("redeeming the developer link answered %d", status)
	}

	if status, _ := get(t, dev, server.URL+MailPath); status != http.StatusForbidden {
		t.Errorf("a developer got %d from the mail page, want 403", status)
	}

	// And the POST is refused too, carrying a token the developer really
	// holds - taken from the setup wizard, which is their page. A guard
	// on the GET alone would be a guard on the page rather than on the
	// action.
	_, wizard := get(t, dev, server.URL+SetupPathPrefix+wizardSteps[0].ID)
	token := csrfFrom(t, wizard)
	status, _ := postWithToken(t, dev, server.URL+MailPath, token, url.Values{
		"islem":     {"kaydet"},
		"sunucu":    {"smtp.saldirgan.com"},
		"port":      {"587"},
		"sifreleme": {"starttls"},
		"gonderen":  {"panel@saldirgan.com"},
		"acik":      {"1"},
	})
	if status != http.StatusForbidden {
		t.Errorf("a developer saving a mail account answered %d, want 403", status)
	}

	// The proof that matters is not the status code: nothing was stored.
	var n int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM panel_smtp`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("a developer's rejected save left a mail account behind")
	}
}

// Editing the sender name must not silently erase the password. The form
// shows no password, so an empty box is the ordinary case.
func TestEditingWithoutRetypingThePasswordKeepsIt(t *testing.T) {
	server, client, store, _ := mailServer(t)
	ctx := context.Background()

	base := url.Values{
		"sunucu":    {"smtp.ornek.com"},
		"port":      {"587"},
		"sifreleme": {"starttls"},
		"kullanici": {"panel@ornek.com"},
		"gonderen":  {"panel@ornek.com"},
		"acik":      {"1"},
	}

	first := url.Values{}
	for k, v := range base {
		first[k] = v
	}
	first.Set("sifre", "ilk-sifre-9931")
	if status, body := saveAccount(t, client, server.URL, first); status != http.StatusOK {
		t.Fatalf("saving answered %d:\n%s", status, body)
	}

	var before string
	if err := store.Pool().QueryRow(ctx,
		`SELECT password_sealed FROM panel_smtp WHERE id = 1`).Scan(&before); err != nil {
		t.Fatal(err)
	}

	second := url.Values{}
	for k, v := range base {
		second[k] = v
	}
	second.Set("gonderen_ad", "Yeni Ad")
	second.Set("sifre", "")
	if status, body := saveAccount(t, client, server.URL, second); status != http.StatusOK {
		t.Fatalf("the second save answered %d:\n%s", status, body)
	}

	var after string
	var fromName string
	if err := store.Pool().QueryRow(ctx,
		`SELECT password_sealed, from_name FROM panel_smtp WHERE id = 1`).Scan(&after, &fromName); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Error("saving with an empty password box changed the stored password")
	}
	if fromName != "Yeni Ad" {
		t.Errorf("from_name = %q; the edit did not take", fromName)
	}
}

// With no key configured the page still renders and says what to do,
// rather than disappearing or accepting a password it cannot protect.
func TestWithoutAKeyThePageExplainsItself(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	// srv.SecretKey deliberately left zero.

	owner := makeUser(t, store, "posta-anahtarsiz", false)
	if err := store.AddMember(ctx, mailSite, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	t.Cleanup(func() {
		_, _ = store.Pool().Exec(context.Background(), `DELETE FROM panel_smtp`)
	})

	client := signedIn(t, server.URL, owner.Email)
	status, body := get(t, client, server.URL+MailPath)
	if status != http.StatusOK {
		t.Fatalf("the mail page answered %d with no key", status)
	}
	if !strings.Contains(body, "Şifreleme anahtarı tanımlı değil") {
		t.Error("the page does not say the key is missing")
	}
	if !strings.Contains(body, "openssl rand -hex 32") {
		t.Error("the page says the key is missing without saying how to make one")
	}

	// And a password is refused rather than stored in the clear.
	token := csrfFrom(t, body)
	status, body = postWithToken(t, client, server.URL+MailPath, token, url.Values{
		"islem":     {"kaydet"},
		"sunucu":    {"smtp.ornek.com"},
		"port":      {"587"},
		"sifreleme": {"starttls"},
		"kullanici": {"panel@ornek.com"},
		"sifre":     {"korunamayacak-sifre"},
		"gonderen":  {"panel@ornek.com"},
		"acik":      {"1"},
	})
	if status != http.StatusBadRequest {
		t.Errorf("saving a password with no key answered %d, want 400", status)
	}
	if strings.Contains(body, "korunamayacak-sifre") {
		t.Error("the refused password came back in the page")
	}

	var n int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM panel_smtp`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("a password was stored despite there being no key to protect it")
	}
}

// startPlainSMTP runs a minimal SMTP server for the duration of the
// test and returns its address.
//
// Deliberately the simplest thing that speaks the protocol: it greets,
// answers EHLO with no extensions, and accepts the three commands a send
// needs. No STARTTLS and no AUTH, which makes it the anonymous-relay
// case - the one shape of unencrypted account this product allows,
// because there is no password to expose.
func startPlainSMTP(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				r := bufio.NewReader(conn)
				write := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }

				write("220 test.local ESMTP")
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					verb, _, _ := strings.Cut(strings.TrimSpace(line), " ")
					switch strings.ToUpper(verb) {
					case "EHLO", "HELO":
						write("250 test.local")
					case "MAIL", "RCPT":
						write("250 ok")
					case "DATA":
						write("354 send it")
						for {
							l, err := r.ReadString('\n')
							if err != nil {
								return
							}
							if l == ".\r\n" || l == ".\n" {
								break
							}
						}
						write("250 accepted")
					case "QUIT":
						write("221 bye")
						return
					default:
						write("500 unknown")
					}
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// closedPort returns a port nothing is listening on: bound to learn the
// number, then given back.
//
// A hardcoded high port would be a test that fails on whichever machine
// happens to be using it, which is the kind of flake nobody can
// reproduce.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}
