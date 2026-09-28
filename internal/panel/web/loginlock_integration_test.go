//go:build integration

// The sign-in budget and the owner's way of lifting a lock (catalogue
// #25), through the handlers a person actually uses.

package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// enrolTOTP gives an account a second factor and returns its secret.
func enrolTOTP(t *testing.T, store *panel.Store, user panel.User) string {
	t.Helper()
	key, err := panel.NewTOTPSecret(user.Email)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetTOTPSecret(context.Background(), user.ID, key.Secret()); err != nil {
		t.Fatal(err)
	}
	return key.Secret()
}

// wrongCode is a code that is certainly not valid now: none of the three
// the server accepts at this moment.
func wrongCode(t *testing.T, secret string) string {
	t.Helper()
	valid := map[string]bool{}
	for _, off := range []time.Duration{-30 * time.Second, 0, 30 * time.Second} {
		c, err := totp.GenerateCode(secret, time.Now().Add(off))
		if err != nil {
			t.Fatal(err)
		}
		valid[c] = true
	}
	for i := 0; ; i++ {
		if c := fmt.Sprintf("%06d", i); !valid[c] {
			return c
		}
	}
}

func submitCode(t *testing.T, c *http.Client, base, code string) *http.Response {
	t.Helper()
	_, body := get(t, c, base+SecondFactorPath)
	resp, err := c.PostForm(base+SecondFactorPath, url.Values{
		"csrf_token": {csrfFrom(t, body)},
		"kod":        {code},
	})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func failuresOf(t *testing.T, email string) int {
	t.Helper()
	var n int
	if err := testdb.Admin(t).QueryRow(context.Background(),
		`SELECT count(*) FROM panel_login_attempts WHERE email = $1 AND NOT success`,
		panel.NormalizeEmail(email)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestTheSecondFactorIsCountedUntilTheSignInCompletes is the defect found
// on the way to #25, as a test.
//
// With the password in hand, an attacker could type seven wrong codes,
// sign in with the password again - which reset the account's failures -
// and type seven more: seventy codes checked in 1.6 seconds against a
// limit of eight per fifteen minutes (measured 2026-09-28, the probe this
// test replaces).
//
// Four codes a round here, not the probe's seven: one fewer than the
// budget, so every return to the password step happens with the account
// still open - the only order in which a reset there hands anything out.
// Seven was right for a budget of eight; against five it locks the
// account inside the first round, and this test went on passing with the
// reset put back (the mutation round caught it through another test).
// Ten rounds of four now get five codes checked in all, and then both
// doors refuse.
func TestTheSecondFactorIsCountedUntilTheSignInCompletes(t *testing.T) {
	srv, store := setupTestServer(t)
	user := makeUser(t, store, "totp-butce", false)
	secret := enrolTOTP(t, store, user)
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	checked, rounds := 0, 0
	for range 10 {
		client := newClient(t, server.URL)
		resp := signIn(t, client, server.URL, user.Email, testAccountPassword)
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			break // the password form has started refusing too
		}
		rounds++
		for range 4 {
			wrong := submitCode(t, client, server.URL, wrongCode(t, secret))
			wrong.Body.Close()
			if wrong.StatusCode == http.StatusUnauthorized {
				checked++
			}
		}
	}
	if checked != 5 {
		t.Errorf("%d wrong codes were checked over %d password rounds, want 5 - the budget, "+
			"not reset by a right password", checked, rounds)
	}
	// Two: the first round spends four, the second is let in with one left
	// and spends it, and the third is refused before the password is read.
	if rounds != 2 {
		t.Errorf("the password was accepted in %d rounds, want 2; once the budget is spent "+
			"the form refuses before it looks at the password", rounds)
	}

	// Locked means locked for the right code too: the check comes first.
	client := newClient(t, server.URL)
	if resp := signIn(t, client, server.URL, user.Email, testAccountPassword); resp.StatusCode != http.StatusTooManyRequests {
		resp.Body.Close()
		t.Errorf("the right password answered %d on a locked account, want 429", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
}

// TestACompletedSignInResetsTheBudget is the other half: somebody who
// fumbles and then gets in does not start their next visit with the
// fumbles still counted - for the password alone, and for a password and
// a code.
func TestACompletedSignInResetsTheBudget(t *testing.T) {
	srv, store := setupTestServer(t)
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	plain := makeUser(t, store, "butce-duz", false)
	client := newClient(t, server.URL)
	for range 4 {
		signIn(t, client, server.URL, plain.Email, "yanlis-parola-yeterince-uzun").Body.Close()
	}
	if n := failuresOf(t, plain.Email); n != 4 {
		t.Fatalf("%d failures recorded, want 4", n)
	}
	if resp := signIn(t, client, server.URL, plain.Email, testAccountPassword); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the right password answered %d", resp.StatusCode)
	}
	if n := failuresOf(t, plain.Email); n != 0 {
		t.Errorf("%d failures left after a completed sign-in, want 0", n)
	}

	coded := makeUser(t, store, "butce-kodlu", false)
	secret := enrolTOTP(t, store, coded)
	client = newClient(t, server.URL)
	for range 2 {
		signIn(t, client, server.URL, coded.Email, "yanlis-parola-yeterince-uzun").Body.Close()
	}
	signIn(t, client, server.URL, coded.Email, testAccountPassword).Body.Close()
	submitCode(t, client, server.URL, wrongCode(t, secret)).Body.Close()
	// Half-way: the right password did not clear anything.
	if n := failuresOf(t, coded.Email); n != 3 {
		t.Fatalf("%d failures after two wrong passwords, a right one and a wrong code, want 3", n)
	}
	code, err := totp.GenerateCode(secret, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if resp := submitCode(t, client, server.URL, code); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("the right code answered %d", resp.StatusCode)
	}
	if n := failuresOf(t, coded.Email); n != 0 {
		t.Errorf("%d failures left after a completed sign-in with a code, want 0", n)
	}
}

// TestAnOwnerLiftsALockFromTheMembersPage: a member locks themselves out
// through the real form, the owner sees it on the member list, presses
// the button, and the member gets in.
func TestAnOwnerLiftsALockFromTheMembersPage(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "kisit-testi"
	owner := makeUser(t, store, "kisit-sahip", false)
	member := makeUser(t, store, "kisit-uye", false)
	if err := store.AddMember(ctx, site, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, site, member.ID, panel.RoleViewer, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	page := server.URL + memberPath(site)
	ownerClient := signedIn(t, server.URL, owner.Email)

	lockOut := func() {
		t.Helper()
		c := newClient(t, server.URL)
		for range 5 {
			signIn(t, c, server.URL, member.Email, "yanlis-parola-yeterince-uzun").Body.Close()
		}
		resp := signIn(t, c, server.URL, member.Email, testAccountPassword)
		resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("five wrong passwords did not lock the member (%d)", resp.StatusCode)
		}
	}
	liftForm := `name="islem" value="kisit-kaldir"`
	memberField := `name="kullanici" value="` + strconv.FormatInt(member.ID, 10) + `"`

	// Nothing to show before there is a lock.
	_, body := get(t, ownerClient, page)
	if strings.Contains(body, liftForm) || strings.Contains(body, "giriş kısıtlı") {
		t.Fatal("the page shows a lock nobody has")
	}

	lockOut()
	_, body = get(t, ownerClient, page)
	row := rowOf(t, body, member.Email)
	if !strings.Contains(row, "giriş kısıtlı") || !strings.Contains(row, liftForm) {
		t.Fatalf("the member's row shows no lock and no button:\n%s", row)
	}
	// The lift form itself names the member - asked of that form, because
	// the row's remove form carries the same field and would answer for it.
	lift := formOf(t, row, "kisit-kaldir")
	if !strings.Contains(lift, memberField) {
		t.Fatalf("the lift form does not name the member:\n%s", lift)
	}
	if !strings.Contains(row, "dakika içinde kendiliğinden kalkar") {
		t.Errorf("the row does not say when the lock lifts by itself:\n%s", row)
	}
	if ownRow := rowOf(t, body, owner.Email); strings.Contains(ownRow, liftForm) {
		t.Error("the owner's own row carries a lift button")
	}

	// Submitted with the form's own fields, as a browser would. A test
	// that typed the member's id itself would pass whatever the form
	// carried.
	status, body := post(t, ownerClient, page, fieldsOf(lift))
	if status != http.StatusOK || !strings.Contains(body, "giriş kısıtı kaldırıldı") {
		t.Fatalf("lifting answered %d: %q", status, noticeOf(body))
	}
	if strings.Contains(rowOf(t, body, member.Email), "giriş kısıtlı") {
		t.Error("the page still shows the lock it just lifted")
	}
	if resp := signIn(t, newClient(t, server.URL), server.URL, member.Email, testAccountPassword); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("the member could not sign in after the lift (%d)", resp.StatusCode)
	}

	// Locked again inside the window: shown, not offered, and said why.
	lockOut()
	_, body = get(t, ownerClient, page)
	row = rowOf(t, body, member.Email)
	if !strings.Contains(row, "giriş kısıtlı") || strings.Contains(row, liftForm) {
		t.Errorf("a second lock in one window should be shown without a button:\n%s", row)
	}
	if !strings.Contains(row, "15 dakikada bir kez") {
		t.Errorf("the row does not say why there is no button:\n%s", row)
	}
	// And a retyped form meets the store's refusal, in a sentence.
	status, body = post(t, ownerClient, page, url.Values{
		"islem": {"kisit-kaldir"}, "kullanici": {strconv.FormatInt(member.ID, 10)},
	})
	if status != http.StatusBadRequest || !strings.Contains(body, "bir kez kaldırılabilir") {
		t.Errorf("a second lift answered %d: %q", status, noticeOf(body))
	}

	// The owner's own account, locked by somebody else's guesses: shown,
	// with what it means, and no button - they are signed in, and lifting
	// it would hand whoever is guessing a fresh budget.
	c := newClient(t, server.URL)
	for range 5 {
		signIn(t, c, server.URL, owner.Email, "yanlis-parola-yeterince-uzun").Body.Close()
	}
	_, body = get(t, ownerClient, page)
	own := rowOf(t, body, owner.Email)
	if !strings.Contains(own, "giriş kısıtlı") || strings.Contains(own, liftForm) {
		t.Errorf("the owner's own lock should be shown without a button:\n%s", own)
	}
	if !strings.Contains(own, "Siz değilseniz parolanızı değiştirin") {
		t.Errorf("the owner's own row does not say what a lock on it means:\n%s", own)
	}
}

// TestTheMembersPageSaysWhenALockLifts: the minutes on the row are the
// database's answer for this lock, not the window restated. Failures ten
// to six minutes old lift in five minutes; a page that printed the window
// would say fifteen.
func TestTheMembersPageSaysWhenALockLifts(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "kisit-dakika-testi"
	owner := makeUser(t, store, "kisit-d-sahip", false)
	member := makeUser(t, store, "kisit-d-uye", false)
	if err := store.AddMember(ctx, site, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, site, member.ID, panel.RoleViewer, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	admin := testdb.Admin(t)
	for _, minutes := range []int{10, 9, 8, 7, 6} {
		if _, err := admin.Exec(ctx, `
			INSERT INTO panel_login_attempts (email, ip, success, at)
			VALUES ($1, '203.0.113.42', false, now() - make_interval(mins => $2))`,
			member.Email, minutes); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	_, body := get(t, signedIn(t, server.URL, owner.Email), server.URL+memberPath(site))
	row := rowOf(t, body, member.Email)
	// Anchored at the start of the sentence: "15 dakika içinde" contains
	// "5 dakika içinde", and the window restated is the answer this test
	// exists to refuse.
	if !strings.Contains(row, `kisit-notu">5 dakika içinde kendiliğinden kalkar.`) {
		t.Errorf("the row does not say five minutes:\n%s", row)
	}
}

// TestAnAdminNeitherSeesNorLiftsALock is the refused half: an
// administrator of the same site sees no badge and no button, and the
// form posted anyway is refused with the lock still in place.
func TestAnAdminNeitherSeesNorLiftsALock(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "kisit-yonetici-testi"
	owner := makeUser(t, store, "kisit-y-sahip", false)
	admin := makeUser(t, store, "kisit-y-yonetici", false)
	member := makeUser(t, store, "kisit-y-uye", false)
	for _, m := range []struct {
		u    panel.User
		role panel.Role
	}{{owner, panel.RoleOwner}, {admin, panel.RoleAdmin}, {member, panel.RoleViewer}} {
		if err := store.AddMember(ctx, site, m.u.ID, m.role, panel.Grant{}); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	page := server.URL + memberPath(site)

	c := newClient(t, server.URL)
	for range 5 {
		signIn(t, c, server.URL, member.Email, "yanlis-parola-yeterince-uzun").Body.Close()
	}

	adminClient := signedIn(t, server.URL, admin.Email)
	_, body := get(t, adminClient, page)
	if strings.Contains(body, "giriş kısıtlı") || strings.Contains(body, `value="kisit-kaldir"`) {
		t.Error("an administrator is shown a lock they may not lift")
	}
	// The owner is, which is what makes the absence above a rule rather
	// than the page having stopped drawing locks.
	_, ownerBody := get(t, signedIn(t, server.URL, owner.Email), page)
	if !strings.Contains(rowOf(t, ownerBody, member.Email), `value="kisit-kaldir"`) {
		t.Fatal("the owner is not shown the lock either; the check above proves nothing")
	}

	status, body := post(t, adminClient, page, url.Values{
		"islem": {"kisit-kaldir"}, "kullanici": {strconv.FormatInt(member.ID, 10)},
	})
	if status != http.StatusBadRequest || !strings.Contains(body, "yalnız bu sitenin sahibi") {
		t.Errorf("an administrator's lift answered %d: %q", status, noticeOf(body))
	}
	if n := failuresOf(t, member.Email); n != 5 {
		t.Errorf("%d failures after a refused lift, want the 5 still counted", n)
	}
}

// TestTheSecondFactorPageLeadsToRecovery: the page used to tell somebody
// without their phone to ask the site's owner to reset their second
// factor, which no page in the panel does. It links to the recovery form,
// where a recovery code does.
func TestTheSecondFactorPageLeadsToRecovery(t *testing.T) {
	srv, store := setupTestServer(t)
	user := makeUser(t, store, "kod-kurtarma", false)
	enrolTOTP(t, store, user)
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	client := newClient(t, server.URL)
	signIn(t, client, server.URL, user.Email, testAccountPassword).Body.Close()
	status, body := get(t, client, server.URL+SecondFactorPath)
	if status != http.StatusOK {
		t.Fatalf("the code form answered %d", status)
	}
	if !strings.Contains(body, `href="`+RecoveryPath+`"`) {
		t.Error("the code form has no way to the recovery form")
	}
	if strings.Contains(body, "sahibinden") {
		t.Error("the code form still sends people to the site's owner")
	}
}

// rowOf returns the member-table row that names an address.
//
// By the row's own header cell rather than by the first mention: the
// signed-in person's address is also in the page header, outside any row.
func rowOf(t *testing.T, body, email string) string {
	t.Helper()
	for _, row := range strings.Split(body, "<tr>")[1:] {
		if end := strings.Index(row, "</tr>"); end >= 0 {
			row = row[:end]
		}
		if strings.Contains(row, "<th scope=\"row\">\n"+email) {
			return row
		}
	}
	t.Fatalf("no member row for %s", email)
	return ""
}

// formOf is the form in a row whose islem is the given action.
func formOf(t *testing.T, row, action string) string {
	t.Helper()
	for _, part := range strings.Split(row, "<form")[1:] {
		if end := strings.Index(part, "</form>"); end >= 0 {
			part = part[:end]
		}
		if strings.Contains(part, `name="islem" value="`+action+`"`) {
			return part
		}
	}
	t.Fatalf("no %q form in the row:\n%s", action, row)
	return ""
}

// hiddenField is one hidden input as the templates write it.
var hiddenField = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`)

// fieldsOf is what a browser submits from a form of hidden fields.
func fieldsOf(form string) url.Values {
	v := url.Values{}
	for _, m := range hiddenField.FindAllStringSubmatch(form, -1) {
		v.Set(m[1], html.UnescapeString(m[2]))
	}
	return v
}
