//go:build integration

// C7.2's second net (B3h) through the pages a person actually uses: the
// operator mints a one-time code from a site's member list, and the
// member gets back in with it.

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// issueAction is the member list's form for a one-time code.
const issueAction = "kurtarma-yenile"

// issuedCode is a code as the codes page shows it.
var issuedCode = regexp.MustCompile(`<code class="secret">([^<]+)</code>`)

// developerOn opens a developer session the only way one can be opened:
// a request, an owner's approval, and the link.
func developerOn(t *testing.T, store *panel.Store, base string, owner panel.User, why string) *http.Client {
	t.Helper()
	token, req := requestAccess(t, store, why)
	if err := store.ApproveDevAccess(context.Background(), req.ID, owner); err != nil {
		t.Fatal(err)
	}
	dev := newClient(t, base)
	if status, _ := get(t, dev, base+DevAccessPathPrefix+token); status != http.StatusSeeOther {
		t.Fatalf("redeeming the developer link answered %d", status)
	}
	return dev
}

// issueForm is what a browser submits from the section's form: its own
// hidden fields, the member chosen in its list, and the password typed
// into the field it draws.
func issueForm(t *testing.T, body, email, password string) url.Values {
	t.Helper()
	form := formOf(t, body, issueAction)
	if !strings.Contains(form, `name="`+devgate.FormField+`"`) {
		t.Fatalf("the form draws no field named %q, the only one the gate reads:\n%s", devgate.FormField, form)
	}
	option := regexp.MustCompile(`<option value="(\d+)">` + regexp.QuoteMeta(email) + `</option>`).
		FindStringSubmatch(form)
	if option == nil {
		t.Fatalf("the form does not offer %s:\n%s", email, form)
	}
	values := fieldsOf(form)
	values.Set("kullanici", option[1])
	values.Set(devgate.FormField, password)
	return values
}

// TestTheOperatorHandsAMemberBackTheirAccount is the whole net, end to
// end: a member who has lost their phone and their codes, a developer
// session, one code, and the member inside with nothing left over.
func TestTheOperatorHandsAMemberBackTheirAccount(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "kurtarma-ver-testi"
	owner := makeUser(t, store, "kv-sahip", false)
	member := makeUser(t, store, "kv-uye", false)
	for _, m := range []struct {
		u    panel.User
		role panel.Role
	}{{owner, panel.RoleOwner}, {member, panel.RoleViewer}} {
		if err := store.AddMember(ctx, site, m.u.ID, m.role, panel.Grant{}); err != nil {
			t.Fatal(err)
		}
	}
	lost := recoveryFor(t, store, member)
	enrolTOTP(t, store, member)
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	page := server.URL + memberPath(site)
	base := srv.Renderer.Catalogs().Base()

	// The owner is not offered it.
	if _, body := get(t, signedIn(t, server.URL, owner.Email), page); strings.Contains(body, `value="`+issueAction+`"`) {
		t.Error("the site's owner is offered the operator's form")
	}

	dev := developerOn(t, store, server.URL, owner, "kurtarma-ver")
	status, body := get(t, dev, page)
	if status != http.StatusOK || !strings.Contains(body, shown(base.T("uyeler.kurtarma.baslik"))) {
		t.Fatalf("the developer's member list (%d) has no recovery section", status)
	}
	// Nobody chosen until somebody is: the first row is the owner, and a
	// press without a choice would end the owner's codes.
	if form := formOf(t, body, issueAction); !strings.Contains(form, `name="kullanici" required>`+"\n"+`<option value="">`) {
		t.Errorf("the list preselects somebody, or lets the form go without a choice:\n%s", form)
	}

	// A wrong password: refused, and the member's codes untouched.
	status, body = post(t, dev, page, issueForm(t, body, member.Email, "bu-parola-yanlis"))
	if status != http.StatusBadRequest || !strings.Contains(body, shown(base.T("ayarlar.hata.parola_yanlis"))) {
		t.Errorf("a wrong password answered %d: %q", status, noticeOf(body))
	}
	if n, err := store.CountRecoveryCodes(ctx, member.ID); err != nil || n != panel.RecoveryCodeCount {
		t.Errorf("%d codes after a refused issue (%v), want the %d the member had", n, err, panel.RecoveryCodeCount)
	}

	_, body = get(t, dev, page)
	status, body = post(t, dev, page, issueForm(t, body, member.Email, testDevPassword))
	if status != http.StatusOK || !strings.Contains(body, shown(base.Tf("kurtarma.verildi.govde", member.Email))) {
		t.Fatalf("issuing answered %d without saying whose code it is: %q", status, noticeOf(body))
	}
	codes := issuedCode.FindAllStringSubmatch(body, -1)
	if len(codes) != 1 {
		t.Fatalf("the page shows %d codes, want one", len(codes))
	}
	// The page is the operator's: pass it on, do not keep it, and back to
	// the list - not "your codes, save them".
	for _, key := range []string{"kurtarma.verildi.uyari", "kurtarma.verildi.nasil", "kurtarma.verildi.donus"} {
		if !strings.Contains(body, shown(base.T(key))) {
			t.Errorf("the codes page does not say %s", key)
		}
	}
	for _, key := range []string{"kurtarma.baslik", "kurtarma.uyari", "kurtarma.kaydettim"} {
		if strings.Contains(body, shown(base.T(key))) {
			t.Errorf("the codes page tells the operator %s, which is the owner's sentence", key)
		}
	}
	if !strings.Contains(body, `href="`+memberPath(site)+`"`) {
		t.Error("the codes page does not lead back to the member list")
	}

	// The member, with the code and without a phone.
	const password = "yeniden-iceride-2026"
	memberClient := newClient(t, server.URL)
	resp, _ := postNoFollow(t, memberClient, server.URL+RecoveryPath, url.Values{
		"eposta": {member.Email}, "kod": {codes[0][1]},
		"yeni_parola": {password}, "yeni_parola_tekrar": {password},
		"ikinci_faktor": {"1"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("recovering with the issued code answered %d", resp.StatusCode)
	}
	// Sent to the page that says they have no codes left and makes new ones.
	if got := resp.Header.Get("Location"); got != AccountPath {
		t.Errorf("recovery with the last code led to %q, want the account page %q", got, AccountPath)
	}
	after, err := store.UserByID(ctx, member.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.HasTOTP() {
		t.Error("the second factor is still on though the member said they cannot reach the phone")
	}
	if n, err := store.CountRecoveryCodes(ctx, member.ID); err != nil || n != 0 {
		t.Errorf("%d codes left after the one-time code (%v), want none", n, err)
	}
	// Where they land says they have none - in both places it talks
	// about them.
	status, body = get(t, memberClient, server.URL+AccountPath)
	if status != http.StatusOK || !strings.Contains(body, shown(base.T("kurtarma.kalan_yok"))) ||
		!strings.Contains(body, shown(base.T("hesap.2fa.kurtarma_yok"))) ||
		strings.Contains(body, shown(base.T("hesap.2fa.kurtarma_var"))) {
		t.Errorf("the account page (%d) does not say, everywhere, that there are no codes left", status)
	}
	// And a code from the lost set does nothing.
	resp, _ = postNoFollow(t, newClient(t, server.URL), server.URL+RecoveryPath, url.Values{
		"eposta": {member.Email}, "kod": {panel.FormatRecoveryCode(lost[1])},
		"yeni_parola": {password + "-2"}, "yeni_parola_tekrar": {password + "-2"},
	})
	if resp.StatusCode == http.StatusSeeOther {
		t.Error("a code from the lost set still works after the issue")
	}
}

// TestTheLastCodeLeadsToTheAccountPageThroughTheSecondFactor: a member who
// keeps their second factor goes through it first, and the account page
// is still where they end up.
func TestTheLastCodeLeadsToTheAccountPageThroughTheSecondFactor(t *testing.T) {
	srv, store := setupTestServer(t)
	user := makeUser(t, store, "son-kod-2fa", false)
	codes, err := store.GenerateRecoveryCodes(context.Background(), user.ID, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Seven of eight spent, as a person would have over time.
	for _, code := range codes[:len(codes)-1] {
		hash, err := panel.HashPassword("ara-parola-yeterince-uzun")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.UseRecoveryCode(context.Background(), user.Email, code, hash, false, netip.MustParseAddr("203.0.113.40")); err != nil {
			t.Fatal(err)
		}
	}
	enrolTOTP(t, store, user)
	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	const password = "son-kodla-iceride-2026"
	resp, _ := postNoFollow(t, newClient(t, server.URL), server.URL+RecoveryPath, url.Values{
		"eposta": {user.Email}, "kod": {panel.FormatRecoveryCode(codes[len(codes)-1])},
		"yeni_parola": {password}, "yeni_parola_tekrar": {password},
	})
	if got, want := resp.Header.Get("Location"), withNext(SecondFactorPath, AccountPath); got != want {
		t.Errorf("recovery with the last code led to %q, want %q", got, want)
	}
}

// TestNobodyButTheOperatorReachesTheGateFromTheMemberList is the refused
// half, and the order it is refused in.
//
// A site's owner posting the form with guesses is refused before the
// password is read. The gate's failure budget is the deployment's, not
// the person's: five wrong answers close it for every form that asks. So
// after five of the owner's guesses the operator's right password must
// still be accepted.
func TestNobodyButTheOperatorReachesTheGateFromTheMemberList(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "kurtarma-ret-testi"
	owner := makeUser(t, store, "kr-sahip", false)
	member := makeUser(t, store, "kr-uye", false)
	for _, m := range []struct {
		u    panel.User
		role panel.Role
	}{{owner, panel.RoleOwner}, {member, panel.RoleViewer}} {
		if err := store.AddMember(ctx, site, m.u.ID, m.role, panel.Grant{}); err != nil {
			t.Fatal(err)
		}
	}
	recoveryFor(t, store, member)
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	page := server.URL + memberPath(site)
	base := srv.Renderer.Catalogs().Base()

	ownerClient := signedIn(t, server.URL, owner.Email)
	for range 5 {
		status, body := post(t, ownerClient, page, url.Values{
			"islem": {issueAction}, "kullanici": {strconv.FormatInt(member.ID, 10)},
			devgate.FormField: {"sahibin-tahmini-yanlis"},
		})
		if status != http.StatusBadRequest || !strings.Contains(body, shown(base.T("uyeler.kurtarma.yetki"))) {
			t.Fatalf("the owner's post answered %d: %q", status, noticeOf(body))
		}
	}
	if n, err := store.CountRecoveryCodes(ctx, member.ID); err != nil || n != panel.RecoveryCodeCount {
		t.Errorf("%d codes after the owner's posts (%v), want %d", n, err, panel.RecoveryCodeCount)
	}

	dev := developerOn(t, store, server.URL, owner, "kurtarma-ret")
	_, body := get(t, dev, page)
	status, body := post(t, dev, page, issueForm(t, body, member.Email, testDevPassword))
	if status != http.StatusOK || len(issuedCode.FindAllString(body, -1)) != 1 {
		t.Errorf("after the owner's five guesses the operator's right password answered %d: %q - "+
			"the guesses reached the gate and spent its budget", status, noticeOf(body))
	}
}

// TestTheSectionSaysSoWhenThereIsNoPassword: a deployment with no developer
// password cannot issue a code, and the operator is told that instead of
// being handed a field that cannot succeed.
func TestTheSectionSaysSoWhenThereIsNoPassword(t *testing.T) {
	srv, store := setupTestServer(t)
	srv.Gate = nil
	ctx := context.Background()
	const site = "kurtarma-kapisiz"
	owner := makeUser(t, store, "kk-sahip", false)
	member := makeUser(t, store, "kk-uye", false)
	for _, m := range []struct {
		u    panel.User
		role panel.Role
	}{{owner, panel.RoleOwner}, {member, panel.RoleViewer}} {
		if err := store.AddMember(ctx, site, m.u.ID, m.role, panel.Grant{}); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	page := server.URL + memberPath(site)
	base := srv.Renderer.Catalogs().Base()

	dev := developerOn(t, store, server.URL, owner, "kurtarma-kapisiz")
	_, body := get(t, dev, page)
	if !strings.Contains(body, shown(base.T("uyeler.kurtarma.kapi_yok"))) || strings.Contains(body, `value="`+issueAction+`"`) {
		t.Error("with no developer password the section should say so and draw no form")
	}
	status, body := post(t, dev, page, url.Values{
		"islem": {issueAction}, "kullanici": {strconv.FormatInt(member.ID, 10)},
		devgate.FormField: {testDevPassword},
	})
	if status != http.StatusBadRequest || !strings.Contains(body, shown(base.T("uyeler.kurtarma.kapi_yok"))) {
		t.Errorf("a post with no gate answered %d: %q", status, noticeOf(body))
	}
}

// TestTheAccountPageSaysWhetherThereAreCodes: the sentence under two-factor
// authentication is drawn from the count, both ways.
//
// Found on B3h's screenshot: the page told somebody with no codes left
// that losing their phone would not matter, one section above the one
// saying they had none. The half with codes is here so that dropping the
// sentence altogether is not a way to pass.
func TestTheAccountPageSaysWhetherThereAreCodes(t *testing.T) {
	srv, store := setupTestServer(t)
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	base := srv.Renderer.Catalogs().Base()
	with := makeUser(t, store, "hesap-kodlu", false)
	recoveryFor(t, store, with)
	without := makeUser(t, store, "hesap-kodsuz", false)

	for _, tc := range []struct {
		user        panel.User
		want, avoid string
	}{
		{with, "hesap.2fa.kurtarma_var", "hesap.2fa.kurtarma_yok"},
		{without, "hesap.2fa.kurtarma_yok", "hesap.2fa.kurtarma_var"},
	} {
		_, body := get(t, signedIn(t, server.URL, tc.user.Email), server.URL+AccountPath)
		if !strings.Contains(body, shown(base.T(tc.want))) || strings.Contains(body, shown(base.T(tc.avoid))) {
			t.Errorf("%s: the two-factor section should say %s and not %s", tc.user.Email, tc.want, tc.avoid)
		}
	}
}
