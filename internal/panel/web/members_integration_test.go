//go:build integration

// Roles, and the refusals that make them mean something.
//
// Every test here has a matching pair: the thing an authorised person
// can do, and the same request from somebody who may not. A permission
// check with only the allowed half tested is a permission check nobody
// has watched fail.

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// signedIn returns a client already holding a session for a user.
func signedIn(t *testing.T, base, email string) *http.Client {
	t.Helper()
	c := newClient(t, base)
	resp := signIn(t, c, base, email, testAccountPassword)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("signing %s in answered %d", email, resp.StatusCode)
	}
	return c
}

// post submits a form to a page, taking its CSRF token from a prior GET
// of the same page.
func post(t *testing.T, c *http.Client, pageURL string, form url.Values) (int, string) {
	t.Helper()
	_, body := get(t, c, pageURL)
	return postWithToken(t, c, pageURL, csrfFrom(t, body), form)
}

// postWithToken submits with a token taken from somewhere else.
//
// It exists for the forged-request tests. A CSRF token belongs to the
// session, not to the page, so somebody refused on one page can still
// carry a perfectly valid token from another - which is exactly the
// request a permission check has to survive. Taking the token from the
// page being attacked would make those tests fail for the wrong reason:
// no token, rather than no authority.
func postWithToken(t *testing.T, c *http.Client, pageURL, token string, form url.Values) (int, string) {
	t.Helper()
	form.Set("csrf_token", token)
	resp, err := c.PostForm(pageURL, form)
	if err != nil {
		t.Fatalf("POST %s: %v", pageURL, err)
	}
	return resp.StatusCode, readBody(t, resp)
}

// sessionToken fetches a CSRF token from a page this client may see.
func sessionToken(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	_, body := get(t, c, base+AccountPath)
	return csrfFrom(t, body)
}

func TestMembersPageEnforcesTheRoleItDraws(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "uye-testi"

	owner := makeUser(t, store, "sahip", false)
	admin := makeUser(t, store, "yonetici", false)
	viewer := makeUser(t, store, "izleyici", false)
	outsider := makeUser(t, store, "yabanci", false)

	for _, m := range []struct {
		id   int64
		role string
	}{
		{owner.ID, "owner"}, {admin.ID, "admin"}, {viewer.ID, "viewer"},
	} {
		if err := store.AddMember(ctx, site, m.id, roleOf(m.role), nil); err != nil {
			t.Fatalf("AddMember(%s): %v", m.role, err)
		}
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	page := server.URL + memberPath(site)

	// ---- an owner sees the page ----
	ownerClient := signedIn(t, server.URL, owner.Email)
	status, body := get(t, ownerClient, page)
	if status != http.StatusOK {
		t.Fatalf("the owner got %d", status)
	}
	for _, want := range []string{owner.Email, admin.Email, viewer.Email} {
		if !strings.Contains(body, want) {
			t.Errorf("the member list is missing %s", want)
		}
	}

	// ---- a viewer is refused, and it is a refusal not a disguise ----
	//
	// 403 rather than 404: the viewer is looking at this site, they know
	// it exists, and telling them "this needs a role you do not have"
	// beats a page pretending not to be there.
	viewerClient := signedIn(t, server.URL, viewer.Email)
	if status, _ := get(t, viewerClient, page); status != http.StatusForbidden {
		t.Errorf("a viewer got %d on the member page, want 403", status)
	}
	// And the refusal is not only in the GET: posting the form directly
	// must fail too, because hiding a form is not authorisation. The
	// token comes from the viewer's own account page - a real session
	// carrying a real token, which is the request that has to be
	// refused on authority alone.
	postStatus, _ := postWithToken(t, viewerClient, page, sessionToken(t, viewerClient, server.URL),
		url.Values{
			"islem": {"cikar"}, "kullanici": {strconv.FormatInt(admin.ID, 10)},
		})
	if postStatus != http.StatusForbidden {
		t.Errorf("a viewer's POST answered %d, want 403", postStatus)
	}

	// ---- somebody with no membership does not learn the site exists ----
	outsiderClient := signedIn(t, server.URL, outsider.Email)
	if status, _ := get(t, outsiderClient, page); status != http.StatusNotFound {
		t.Errorf("a non-member got %d, want 404 - a 403 confirms the site exists", status)
	}

	// ---- an admin may not grant ownership ----
	//
	// The select never offers it. This posts it anyway, which is what an
	// admin who reads the HTML would do.
	adminClient := signedIn(t, server.URL, admin.Email)
	status, body = post(t, adminClient, page, url.Values{
		"islem": {"rol"}, "kullanici": {strconv.FormatInt(viewer.ID, 10)}, "rol": {"owner"},
	})
	if status != http.StatusBadRequest {
		t.Errorf("an admin promoting somebody to owner answered %d, want a refusal", status)
	}
	if !strings.Contains(body, "üstünde bir rol veremezsiniz") {
		t.Errorf("the refusal does not explain itself: %q", messageOf(body))
	}
	// And it did not happen.
	if access, err := store.AccessFor(ctx, principalOf(viewer.ID), site); err != nil {
		t.Fatal(err)
	} else if access.Role != "viewer" {
		t.Fatalf("the viewer became %q; the check was cosmetic", access.Role)
	}

	// ---- an owner may ----
	status, _ = post(t, ownerClient, page, url.Values{
		"islem": {"rol"}, "kullanici": {strconv.FormatInt(admin.ID, 10)}, "rol": {"owner"},
	})
	if status != http.StatusOK {
		t.Errorf("an owner promoting an admin answered %d", status)
	}
}

// TestTheLastOwnerCannotBeRemoved is the store's rule seen from the
// page. What is being tested is not the rule - that has its own test -
// but that the page turns it into a sentence instead of a 500.
func TestTheLastOwnerCannotBeRemoved(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "son-sahip-testi"

	owner := makeUser(t, store, "tek-sahip", false)
	helper := makeUser(t, store, "yardimci", false)
	if err := store.AddMember(ctx, site, owner.ID, roleOf("owner"), nil); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, site, helper.ID, roleOf("admin"), nil); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	page := server.URL + memberPath(site)
	helperClient := signedIn(t, server.URL, helper.Email)

	// The admin tries to remove the only owner.
	status, body := post(t, helperClient, page, url.Values{
		"islem": {"cikar"}, "kullanici": {strconv.FormatInt(owner.ID, 10)},
	})
	if status == http.StatusInternalServerError {
		t.Fatal("the last-owner rule surfaced as a server error")
	}
	if !strings.Contains(body, "en az bir sahibi olmalı") {
		t.Errorf("the page did not explain the refusal: %q", messageOf(body))
	}
	// And demotion is refused for the same reason, by the same rule.
	_, body = post(t, helperClient, page, url.Values{
		"islem": {"rol"}, "kullanici": {strconv.FormatInt(owner.ID, 10)}, "rol": {"admin"},
	})
	if !strings.Contains(body, "en az bir sahibi olmalı") {
		t.Errorf("demoting the last owner was not refused: %q", messageOf(body))
	}

	// The owner is still there.
	access, err := store.AccessFor(ctx, principalOf(owner.ID), site)
	if err != nil {
		t.Fatal(err)
	}
	if access.Role != "owner" {
		t.Fatalf("the site lost its last owner; role is now %q", access.Role)
	}
}

// TestAddingAMemberNeedsAnExistingAccount covers the sentence that
// exists because this page cannot send an invitation.
func TestAddingAMemberNeedsAnExistingAccount(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "davet-testi"

	owner := makeUser(t, store, "davet-sahibi", false)
	guest := makeUser(t, store, "davetli", false)
	if err := store.AddMember(ctx, site, owner.ID, roleOf("owner"), nil); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	page := server.URL + memberPath(site)
	client := signedIn(t, server.URL, owner.Email)

	// An address nobody has an account for.
	status, body := post(t, client, page, url.Values{
		"islem": {"ekle"}, "eposta": {"kimse" + testEmailSuffix}, "rol": {"viewer"},
	})
	if status != http.StatusBadRequest {
		t.Errorf("adding a nonexistent account answered %d", status)
	}
	if !strings.Contains(body, "hesap yok") {
		t.Errorf("the refusal does not say what is missing: %q", messageOf(body))
	}

	// One that does.
	status, body = post(t, client, page, url.Values{
		"islem": {"ekle"}, "eposta": {guest.Email}, "rol": {"viewer"},
	})
	if status != http.StatusOK {
		t.Fatalf("adding an existing account answered %d: %q", status, messageOf(body))
	}
	access, err := store.AccessFor(ctx, principalOf(guest.ID), site)
	if err != nil {
		t.Fatal(err)
	}
	if access.Role != "viewer" {
		t.Fatalf("the membership was not written; role is %q", access.Role)
	}
}

// TestASuperadminReachesEverySite: hosting the deployment is what that
// means, and hiding it behind membership rows would only produce an
// operator quietly granting themselves one.
func TestASuperadminReachesEverySite(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "operator-testi"

	owner := makeUser(t, store, "site-sahibi", false)
	operator := makeUser(t, store, "isletmeci", true)
	if err := store.AddMember(ctx, site, owner.ID, roleOf("owner"), nil); err != nil {
		t.Fatal(err)
	}
	_ = ctx

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	client := signedIn(t, server.URL, operator.Email)

	status, body := get(t, client, server.URL+memberPath(site))
	if status != http.StatusOK {
		t.Fatalf("the operator got %d on a site they have no membership on", status)
	}
	// And the chrome says so, so nobody forgets whose data is on screen.
	if !strings.Contains(body, "İşletmeci olarak görüyorsunuz") {
		t.Error("the page does not say this is operator access")
	}
}

// roleOf and principalOf are one-line adapters so the tests above read
// as prose. They are here rather than in the panel package because they
// are only ever useful in a test.
func roleOf(s string) panel.Role { return panel.Role(s) }

func principalOf(userID int64) panel.Principal {
	return panel.Principal{Kind: panel.PrincipalUser, UserID: userID}
}
