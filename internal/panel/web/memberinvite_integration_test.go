//go:build integration

package web

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// The invitation, from the two pages a person actually meets.
//
// internal/panel proves the store keeps its promises. What a handler
// test adds is the half a store cannot see: whether the link reaches the
// page, whether the form the invitee is shown can change what they were
// invited to, and whether the page that lists invitations lets the right
// person withdraw one.

const inviteWebSite = "web-davet-testi"

// linkPattern finds the invitation link the members page prints.
//
// Read out of the page rather than out of the database, deliberately.
// The token is never stored, so the page is the only place it exists -
// and a test that reached into the store for it would be proving
// something about the store, which is a different file's job.
var linkPattern = regexp.MustCompile(`/katil/[A-Za-z0-9_-]+`)

// jarClient is an unauthenticated client that keeps cookies.
//
// Needed because the join page starts a session for its CSRF token, and
// a client without a jar posts back a token belonging to a session it
// has already forgotten.
func jarClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// inviteWebFixture signs somebody in as the owner of inviteWebSite.
func inviteWebFixture(t *testing.T) (*httptest.Server, *http.Client, *panel.Store, panel.User) {
	t.Helper()
	srv, store := setupTestServer(t)
	ctx := context.Background()

	owner := makeUser(t, store, "web-davet-sahip", false)
	if err := store.AddMember(ctx, inviteWebSite, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = store.Pool().Exec(bg,
			`DELETE FROM panel_member_invites WHERE site_id = $1`, inviteWebSite)
		_, _ = store.Pool().Exec(bg,
			`DELETE FROM panel_site_members WHERE site_id = $1`, inviteWebSite)
	})

	server := httptest.NewServer(srv.Handler())
	t.Cleanup(server.Close)
	return server, signedIn(t, server.URL, owner.Email), store, owner
}

// invite mints one through the page and returns the link it printed.
//
// It also removes the account accepting that invitation would create.
//
// Measured the hard way: without this, the first run creates the account
// through the join page, the test fails on a later assertion, and the
// second run takes the "this address already has an account" branch and
// fails for a reason that has nothing to do with the code. A test that
// drives the product through its own flow has to clean up what the flow
// made, not only what its fixture made.
func invite(t *testing.T, client *http.Client, server *httptest.Server, email, role string) string {
	t.Helper()
	t.Cleanup(func() {
		_, _ = testdb.Admin(t).Exec(context.Background(),
			`DELETE FROM panel_users WHERE email = $1`, email)
	})
	status, body := post(t, client, server.URL+memberPath(inviteWebSite), url.Values{
		"islem": {"ekle"}, "eposta": {email}, "rol": {role},
	})
	if status != http.StatusOK {
		t.Fatalf("inviting %s answered %d: %q", email, status, messageOf(body))
	}
	link := linkPattern.FindString(body)
	if link == "" {
		t.Fatalf("the page printed no invitation link for %s: %q", email, messageOf(body))
	}
	return link
}

// TestTheInvitedPageIsReadable, and shows what is being offered.
//
// The page a stranger meets. It has to say which site and which role
// before they accept, because "viewer" is a word somebody has to
// understand in advance rather than discover afterwards.
//
// Both faces are loaded: the form, and the refusal a dead link gets.
// This is the walk the marker test excuses joinHandler for.
func TestTheInvitedPageIsReadable(t *testing.T) {
	server, client, _, _ := inviteWebFixture(t)
	link := invite(t, client, server, "okunur"+testEmailSuffix, "viewer")

	// Opened by somebody with no session at all, which is who opens it.
	stranger := &http.Client{}
	resp, err := stranger.Get(server.URL + link)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the invitation page answered %d", resp.StatusCode)
	}
	for _, want := range []string{inviteWebSite, "okunur" + testEmailSuffix} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not say %q. Somebody deciding whether to accept "+
				"has to be told what they are accepting", want)
		}
	}
	if strings.Contains(body, `name="rol"`) || strings.Contains(body, `name="site"`) {
		t.Error("the page offers a role or a site field. Neither is the invitee's to choose")
	}
	if markerPattern.MatchString(body) {
		t.Errorf("the invitation page carries a missing-message marker: %q",
			markerPattern.FindString(body))
	}

	// And the other face.
	dead, err := stranger.Get(server.URL + JoinPathPrefix + "boyle-bir-jeton-yok")
	if err != nil {
		t.Fatal(err)
	}
	defer dead.Body.Close()
	deadBody := readBody(t, dead)
	if dead.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown invitation answered %d, want 404", dead.StatusCode)
	}
	if markerPattern.MatchString(deadBody) {
		t.Errorf("the refusal page carries a missing-message marker: %q",
			markerPattern.FindString(deadBody))
	}
}

// TestTheInviteeCannotChooseTheirOwnRole.
//
// The claim the whole design rests on, asked the way an attacker would:
// by sending the field the page does not draw. The invitation is for a
// viewer; the form posts owner.
//
// *İstemciye güvenme, sadece sunucuya güven.*
func TestTheInviteeCannotChooseTheirOwnRole(t *testing.T) {
	server, client, store, _ := inviteWebFixture(t)
	ctx := context.Background()
	link := invite(t, client, server, "kurnaz"+testEmailSuffix, "viewer")

	// A client with a jar but no session: the CSRF token is bound to a
	// session the page itself starts, which is how somebody holding only
	// a link arrives.
	stranger := jarClient(t)
	status, body := post(t, stranger, server.URL+link, url.Values{
		"ad":            {"Kurnaz"},
		"parola":        {testAccountPassword},
		"parola_tekrar": {testAccountPassword},
		// Neither field exists on the page. Both are sent anyway.
		"rol":  {"owner"},
		"site": {"baska-site"},
	})
	if status != http.StatusOK && status != http.StatusSeeOther {
		t.Fatalf("accepting answered %d: %q", status, messageOf(body))
	}

	user, err := store.UserByEmail(ctx, "kurnaz"+testEmailSuffix)
	if err != nil {
		t.Fatalf("the account was not created: %v", err)
	}
	access, err := store.AccessFor(ctx, principalOf(user.ID), inviteWebSite)
	if err != nil {
		t.Fatal(err)
	}
	if access.Role != panel.RoleViewer {
		t.Fatalf("the invitee holds %q after posting rol=owner; the invitation said viewer.\n"+
			"The role has to come from the row, never from the form", access.Role)
	}
	if user.IsSuperadmin {
		t.Error("accepting an invitation produced a superadmin")
	}
	// And nothing was granted on the site the form named.
	other, err := store.AccessFor(ctx, principalOf(user.ID), "baska-site")
	if err != nil {
		t.Fatal(err)
	}
	if other.Member {
		t.Error("the invitee is a member of the site their form named rather than the invitation's")
	}
}

// TestAnInvitationIsListedAndCanBeWithdrawn.
func TestAnInvitationIsListedAndCanBeWithdrawn(t *testing.T) {
	server, client, store, _ := inviteWebFixture(t)
	ctx := context.Background()
	link := invite(t, client, server, "geri"+testEmailSuffix, "viewer")

	page := server.URL + memberPath(inviteWebSite)
	status, body := get(t, client, page)
	if status != http.StatusOK {
		t.Fatalf("the members page answered %d", status)
	}
	if !strings.Contains(body, "geri"+testEmailSuffix) {
		t.Error("the members page does not list the pending invitation")
	}

	invites, err := store.OpenMemberInvites(ctx, inviteWebSite)
	if err != nil || len(invites) != 1 {
		t.Fatalf("expected one open invitation, got %d (%v)", len(invites), err)
	}
	status, body = post(t, client, page, url.Values{
		"islem": {"davet-geri-al"}, "davet": {strconv.FormatInt(invites[0].ID, 10)},
	})
	if status != http.StatusOK {
		t.Fatalf("withdrawing answered %d: %q", status, messageOf(body))
	}

	// Withdrawn means the link stops working, which is the only part of
	// this that matters to the person holding it.
	stranger := &http.Client{}
	resp, err := stranger.Get(server.URL + link)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("a withdrawn invitation still answers %d", resp.StatusCode)
	}
}

// TestAViewerCannotInviteAnybody.
//
// The page is refused to them entirely, which is the existing rule for
// every member operation. Asserted here because an invitation is a way
// of adding a member and would be a way around that rule if it were not.
func TestAViewerCannotInviteAnybody(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()

	owner := makeUser(t, store, "kisit-sahip", false)
	viewer := makeUser(t, store, "kisit-izleyici", false)
	for id, role := range map[int64]panel.Role{owner.ID: panel.RoleOwner, viewer.ID: panel.RoleViewer} {
		if err := store.AddMember(ctx, inviteWebSite, id, role, panel.Grant{}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = store.Pool().Exec(bg, `DELETE FROM panel_member_invites WHERE site_id = $1`, inviteWebSite)
		_, _ = store.Pool().Exec(bg, `DELETE FROM panel_site_members WHERE site_id = $1`, inviteWebSite)
	})

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := signedIn(t, server.URL, viewer.Email)

	// The token comes from a page this account may open, because the one
	// under attack answers 403 and carries no form. A CSRF token belongs
	// to the session rather than to the page, so this is the request a
	// permission check actually has to survive - see postWithToken.
	_, home := get(t, client, server.URL+"/")
	status, _ := postWithToken(t, client, server.URL+memberPath(inviteWebSite),
		csrfFrom(t, home), url.Values{
			"islem": {"ekle"}, "eposta": {"olmaz" + testEmailSuffix}, "rol": {"viewer"},
		})
	if status != http.StatusForbidden {
		t.Errorf("a viewer inviting somebody answered %d, want 403", status)
	}
	open, err := store.OpenMemberInvites(ctx, inviteWebSite)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("a viewer's request minted %d invitation(s)", len(open))
	}
}

// TestOneSiteCannotWithdrawAnothersInvitation.
//
// The isolation rule this project states in the customer's own words:
// *başka birinin başka birinin verilerine müdehale etmesini
// engellemeliyiz.* The invitation id is a number in a form, so the only
// thing standing between one site's owner and another site's invitation
// is that the site comes from the authorised access rather than from the
// request.
func TestOneSiteCannotWithdrawAnothersInvitation(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const other = "web-davet-baska"

	mine := makeUser(t, store, "yalitim-benim", false)
	theirs := makeUser(t, store, "yalitim-onlar", false)
	if err := store.AddMember(ctx, inviteWebSite, mine.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, other, theirs.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bg := context.Background()
		for _, site := range []string{inviteWebSite, other} {
			_, _ = store.Pool().Exec(bg, `DELETE FROM panel_member_invites WHERE site_id = $1`, site)
			_, _ = store.Pool().Exec(bg, `DELETE FROM panel_site_members WHERE site_id = $1`, site)
		}
	})

	// Their invitation, minted by them on their own site.
	_, theirInvite, err := store.CreateMemberInvite(ctx, other, "onlarin"+testEmailSuffix,
		panel.RoleViewer, panel.Principal{UserID: theirs.ID, Label: theirs.Email}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := signedIn(t, server.URL, mine.Email)

	// Posted to my own site's page, naming their invitation's id.
	status, body := post(t, client, server.URL+memberPath(inviteWebSite), url.Values{
		"islem": {"davet-geri-al"}, "davet": {strconv.FormatInt(theirInvite.ID, 10)},
	})
	if status == http.StatusOK {
		t.Errorf("withdrawing another site's invitation answered 200: %q", messageOf(body))
	}

	open, err := store.OpenMemberInvites(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("the other site's invitation was withdrawn by somebody with no authority over it")
	}
}

// TestAnInvitedAddressThatAlreadyHasAnAccountIsNotSignedIn.
//
// The branch that is easy to get wrong and expensive when it is. If the
// address already has an account, the password on this form was never
// applied to it - so signing them in on the strength of it would be
// signing somebody in without checking a password, on a page anybody
// holding a link can open.
//
// The membership is still granted, because that is what the invitation
// was for.
func TestAnInvitedAddressThatAlreadyHasAnAccountIsNotSignedIn(t *testing.T) {
	server, client, store, _ := inviteWebFixture(t)
	ctx := context.Background()

	// The real sequence: invited while they had no account here, and by
	// the time they open the link they have one - another site invited
	// them first, or the operator made it. The members page would have
	// added an existing account directly, so the invitation is minted
	// through the store and the account appears afterwards.
	owner, err := store.UserByEmail(ctx, "web-davet-sahip"+testEmailSuffix)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.CreateMemberInvite(ctx, inviteWebSite, "zaten-var"+testEmailSuffix,
		panel.RoleViewer, panel.Principal{UserID: owner.ID, Label: owner.Email}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	link := JoinPathPrefix + token
	existing := makeUser(t, store, "zaten-var", false)
	_ = client

	stranger := jarClient(t)
	status, body := post(t, stranger, server.URL+link, url.Values{
		"ad":            {"Başkası"},
		"parola":        {"bambaska-bir-parola-123"},
		"parola_tekrar": {"bambaska-bir-parola-123"},
	})
	if status != http.StatusOK {
		t.Fatalf("accepting answered %d: %q", status, messageOf(body))
	}

	// The membership is granted.
	access, err := store.AccessFor(ctx, principalOf(existing.ID), inviteWebSite)
	if err != nil {
		t.Fatal(err)
	}
	if access.Role != panel.RoleViewer {
		t.Errorf("the existing account holds %q on the site; the invitation said viewer", access.Role)
	}

	// And nobody was signed in. Asked of the panel rather than of the
	// cookie jar: a page that only the signed-in may open is the honest
	// question, and it answers a redirect to the sign-in form.
	resp, err := stranger.Get(server.URL + AccountPath)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("accepting with a password that was never checked produced a session.\n" +
			"This page is reachable by anybody holding a link")
	}

	// The password on the account is untouched, which is what lets them
	// sign in the way they always did.
	after, err := store.UserByEmail(ctx, existing.Email)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := panel.VerifyPassword(after.PasswordHash, testAccountPassword); !ok {
		t.Error("the existing account's own password no longer works. Accepting an " +
			"invitation must not reset a password nobody proved they held")
	}
	if ok, _ := panel.VerifyPassword(after.PasswordHash, "bambaska-bir-parola-123"); ok {
		t.Error("the password typed on the join page was applied to an account that " +
			"already had one, without anybody proving they held the old one")
	}
}
