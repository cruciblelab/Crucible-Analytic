//go:build integration

// Handover to the owner's first page, against a real database.
//
// This is the chain that did not exist before C3: a finished technical
// installation produces an invitation, the invitation produces an
// account, and the account owns the sites the deployment was configured
// for. Every link in it is asserted here, including the two that are
// refusals.

package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// claimLinkPattern pulls the invitation out of the handover page, which
// is the only place it is ever shown.
var claimLinkPattern = regexp.MustCompile(`<code class="secret">([^<]+)</code>`)

func TestHandoverCreatesAnOwner(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "devir-testi"
	const email = "sahip@devir-testi.invalid"

	// The deployment is configured for a site, as the technical wizard
	// would have left it.
	if err := store.SetSetting(ctx, panel.KeyBeaconSites, "", []string{site}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool := store.Pool()
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM panel_audit_log WHERE actor_label = $1`, email)
		_, _ = pool.Exec(bg, `DELETE FROM panel_site_members WHERE site_id = $1`, site)
		_, _ = pool.Exec(bg, `DELETE FROM panel_owner_claims WHERE email = $1`, email)
		_, _ = pool.Exec(bg, `DELETE FROM panel_users WHERE email = $1`, email)
	})

	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	// ---- the developer reaches the handover step ----
	dev := newClient(t, server.URL)
	token, req, err := store.RequestDevAccess(ctx, testReasonPrefix+"devir", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !req.AutoApproved {
		t.Skip("this database already has accounts, so the developer link needs approval")
	}
	resp, err := dev.Get(server.URL + DevAccessPathPrefix + token)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	handover := server.URL + SetupPathPrefix + "devir"
	status, body := get(t, dev, handover)
	if status != http.StatusOK {
		t.Fatalf("the handover step answered %d", status)
	}

	// ---- an address that is not an address is refused ----
	status, body = post(t, dev, handover, url.Values{"eposta": {""}})
	if status != http.StatusBadRequest {
		t.Errorf("an empty address answered %d", status)
	}

	// ---- and a real one mints a link, shown once ----
	status, body = post(t, dev, handover, url.Values{
		"eposta": {email}, "ad": {"Devir Sahibi"},
	})
	if status != http.StatusOK {
		t.Fatalf("handover answered %d: %q", status, messageOf(body))
	}
	m := claimLinkPattern.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no invitation link on the page:\n%s", body)
	}
	claimURL := m[1]
	if !strings.Contains(claimURL, ClaimPathPrefix) {
		t.Fatalf("the printed link is not an invitation: %q", claimURL)
	}
	// The page has to say the link is shown once, or somebody closes the
	// tab expecting to find it again.
	if !strings.Contains(body, "yalnız burada gösterilir") {
		t.Error("the page does not say the link cannot be shown again")
	}

	// Reloading the step does not show it again.
	_, reloaded := get(t, dev, handover)
	if claimLinkPattern.MatchString(reloaded) {
		t.Error("the link is still on the page after a reload; only its hash is stored")
	}
	// But the invitation is listed as open, so a second one is a
	// deliberate choice rather than an accident.
	if !strings.Contains(reloaded, email) {
		t.Error("the open invitation is not listed")
	}

	// ---- the customer opens it ----
	path := claimURL[strings.Index(claimURL, ClaimPathPrefix):]
	owner := newClient(t, server.URL)
	status, body = get(t, owner, server.URL+path)
	if status != http.StatusOK {
		t.Fatalf("the invitation page answered %d", status)
	}
	if !strings.Contains(body, email) {
		t.Error("the page does not say which address the account is for")
	}

	// A short password is refused, by the same rule the account page uses.
	status, body = post(t, owner, server.URL+path, url.Values{
		"parola": {"kisa"}, "parola_tekrar": {"kisa"},
	})
	if status != http.StatusBadRequest || !strings.Contains(body, "en az 12") {
		t.Errorf("a short password answered %d: %q", status, messageOf(body))
	}

	// ---- and sets a real one ----
	const password = "devir-testi-parolasi-uzun"
	_, body = get(t, owner, server.URL+path)
	resp, err = owner.PostForm(server.URL+path, url.Values{
		"csrf_token":    {csrfFrom(t, body)},
		"ad":            {"Devir Sahibi"},
		"parola":        {password},
		"parola_tekrar": {password},
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claiming answered %d", resp.StatusCode)
	}

	// The recovery codes, rendered rather than redirected past.
	//
	// This used to be a 303 into the owner's wizard. It is a page now
	// because these codes exist in readable form exactly once - carrying
	// them through a redirect would mean putting eight of them in the
	// session, which is a database table, and they are stored as digests
	// everywhere else precisely so they are not readable at rest.
	//
	// Counted rather than matched: the codes are random, so what can be
	// asserted is that the right number of them arrived and that the
	// page says where the reader goes next.
	if got := strings.Count(string(claimed), `class="secret"`); got != panel.RecoveryCodeCount {
		t.Errorf("the page after claiming shows %d codes, want %d", got, panel.RecoveryCodeCount)
	}
	if !strings.Contains(string(claimed), WelcomePathPrefix+welcomeSteps[0].ID) {
		t.Error("the recovery-code page does not link on to the owner's wizard")
	}

	// The account exists, owns the site, and is not a superadmin.
	user, err := store.UserByEmail(ctx, email)
	if err != nil {
		t.Fatalf("the account was not created: %v", err)
	}
	if user.IsSuperadmin {
		t.Error("accepting an invitation produced a superadmin")
	}
	access, err := store.AccessFor(ctx, panel.Principal{Kind: panel.PrincipalUser, UserID: user.ID}, site)
	if err != nil {
		t.Fatal(err)
	}
	if access.Role != panel.RoleOwner {
		t.Fatalf("the new account's role on %s is %q, want owner", site, access.Role)
	}

	// ---- the same link cannot be used twice ----
	second := newClient(t, server.URL)
	status, body = get(t, second, server.URL+path)
	if status != http.StatusNotFound {
		t.Errorf("a used invitation answered %d, want 404", status)
	}
	if !strings.Contains(body, "kullanılamıyor") {
		t.Errorf("the refusal does not explain itself: %s", body)
	}

	// ---- and the wizard works for the new owner ----
	status, body = get(t, owner, server.URL+WelcomePathPrefix+"site")
	if status != http.StatusOK {
		t.Fatalf("the owner's wizard answered %d", status)
	}
	if !strings.Contains(body, site) {
		t.Error("the wizard does not list the configured site")
	}

	status, body = post(t, owner, server.URL+WelcomePathPrefix+"site", url.Values{
		"ad:" + site: {"Benim Sitem"},
	})
	if status != http.StatusOK {
		t.Fatalf("saving the site name answered %d: %q", status, messageOf(body))
	}
	name, err := store.GetSetting(ctx, panel.KeySiteName, site)
	if err != nil {
		t.Fatal(err)
	}
	if name != "Benim Sitem" {
		t.Errorf("the site name is %v, want the one that was typed", name)
	}
}

// TestOwnerWizardRefusesAnythingButAnOwner. The wizard sets what a site
// is called and which clock it is read against - the customer's
// decisions, not their staff's.
func TestOwnerWizardRefusesAnythingButAnOwner(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "hosgeldiniz-yetki"

	admin := makeUser(t, store, "hg-yonetici", false)
	viewer := makeUser(t, store, "hg-izleyici", false)
	owner := makeUser(t, store, "hg-sahip", false)
	for _, m := range []struct {
		id   int64
		role panel.Role
	}{{admin.ID, panel.RoleAdmin}, {viewer.ID, panel.RoleViewer}, {owner.ID, panel.RoleOwner}} {
		if err := store.AddMember(ctx, site, m.id, m.role, nil); err != nil {
			t.Fatal(err)
		}
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	page := server.URL + WelcomePathPrefix + "site"

	for _, tc := range []struct {
		name  string
		email string
		want  int
	}{
		{"viewer", viewer.Email, http.StatusForbidden},
		{"admin", admin.Email, http.StatusForbidden},
		{"owner", owner.Email, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := signedIn(t, server.URL, tc.email)
			if status, _ := get(t, c, page); status != tc.want {
				t.Errorf("%s got %d, want %d", tc.name, status, tc.want)
			}
		})
	}
}

// TestTimezoneSettingIsCheckedRatherThanAccepted. A panel that takes a
// name it cannot load and then renders in UTC tells a shop in Istanbul
// its evening peak happened in the afternoon, and nothing says why.
func TestTimezoneSettingIsCheckedRatherThanAccepted(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "saat-testi"

	owner := makeUser(t, store, "saat-sahip", false)
	if err := store.AddMember(ctx, site, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.Pool().Exec(context.Background(),
			`DELETE FROM panel_settings WHERE key = $1`, string(panel.KeyPanelTimezone))
	})

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	page := server.URL + WelcomePathPrefix + "saat"
	client := signedIn(t, server.URL, owner.Email)

	status, body := post(t, client, page, url.Values{"saat_dilimi": {"Avrupa/İstanbul"}})
	if status != http.StatusBadRequest {
		t.Errorf("a name that is not a zone answered %d", status)
	}
	if !strings.Contains(body, "Avrupa/") {
		t.Errorf("the refusal does not quote what was typed: %q", messageOf(body))
	}

	status, body = post(t, client, page, url.Values{"saat_dilimi": {"Europe/Istanbul"}})
	if status != http.StatusOK {
		t.Fatalf("a real zone answered %d: %q", status, messageOf(body))
	}
	stored, err := store.GetSetting(ctx, panel.KeyPanelTimezone, "")
	if err != nil {
		t.Fatal(err)
	}
	if stored != "Europe/Istanbul" {
		t.Errorf("stored zone is %v", stored)
	}

	// And the setting actually reaches the rendered page, which is the
	// only thing that makes it worth storing.
	_, body = get(t, client, server.URL+AccountPath)
	if !strings.Contains(body, "Europe/Istanbul") {
		t.Error("the chosen zone does not appear in the page footer")
	}
}

// TestTheTechnicalDoorWarnsBeforeItOpens covers the confirmation, and
// the two refusals around it.
func TestTheTechnicalDoorWarnsBeforeItOpens(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()
	const site = "teknik-kapi"

	owner := makeUser(t, store, "kapi-sahip", false)
	admin := makeUser(t, store, "kapi-yonetici", false)
	if err := store.AddMember(ctx, site, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, site, admin.ID, panel.RoleAdmin, nil); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()

	// ---- an admin never reaches it ----
	adminClient := signedIn(t, server.URL, admin.Email)
	if status, _ := get(t, adminClient, server.URL+TechnicalDoorPath); status != http.StatusForbidden {
		t.Errorf("an admin got %d at the technical door, want 403", status)
	}

	// ---- an owner meets the warning, not the wizard ----
	ownerClient := signedIn(t, server.URL, owner.Email)
	status, body := get(t, ownerClient, server.URL+TechnicalDoorPath)
	if status != http.StatusOK {
		t.Fatalf("the owner got %d at the door", status)
	}
	if !strings.Contains(body, "geliştiriciniz tarafından tamamlandı") {
		t.Error("the door does not carry the warning")
	}

	// Walking straight at the wizard sends them to the door rather than
	// refusing: they may go through it, they have just not been warned.
	resp, err := ownerClient.Get(server.URL + SetupPathPrefix + "baslangic")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("an unwarned owner got %d at the wizard, want a redirect to the door", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != TechnicalDoorPath {
		t.Errorf("the wizard sent them to %q", got)
	}

	// ---- confirming opens it ----
	_, body = get(t, ownerClient, server.URL+TechnicalDoorPath)
	resp, err = ownerClient.PostForm(server.URL+TechnicalDoorPath, url.Values{
		"csrf_token": {csrfFrom(t, body)},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("confirming answered %d", resp.StatusCode)
	}
	if status, _ := get(t, ownerClient, server.URL+SetupPathPrefix+"baslangic"); status != http.StatusOK {
		t.Errorf("the wizard answered %d after the owner confirmed", status)
	}

	// The decision is in the audit log, because "who went in there, and
	// when" is the first question asked when a working installation
	// stops working.
	var count int
	if err := store.Pool().QueryRow(ctx,
		`SELECT count(*) FROM panel_audit_log WHERE action = $1 AND actor_id = $2`,
		panel.ActionTechnicalDoorOpened, owner.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("%d audit entries for opening the door, want 1", count)
	}

	// ---- and a different session is warned again ----
	//
	// The confirmation is about this visit, not about this person
	// forever: the thing it warns about does not get less true with
	// familiarity.
	fresh := signedIn(t, server.URL, owner.Email)
	resp, err = fresh.Get(server.URL + SetupPathPrefix + "baslangic")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Error("a new session walked into the wizard without seeing the warning")
	}
}
