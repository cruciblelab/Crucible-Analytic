//go:build integration

// Real coverage of the panel's data layer against a live PostgreSQL,
// gated behind the "integration" build tag like the rest of this
// project. These exercise what the unit tests cannot: that the SQL is
// valid, and - the reason several of them exist at all - that the
// last-owner protection and the single-use developer login hold under
// genuine concurrency, which is the only condition either can fail
// under. Run with:
//
//	docker compose up -d
//	psql "$DSN" -f internal/panel/schema.sql
//	go test -tags integration ./internal/panel/... -v

package panel

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURL = "postgres://collector:collector@localhost:5432/analytics"

// newTestStore opens a Store and removes everything the test created
// afterwards. Accounts are namespaced by a per-test email suffix and
// sites by a per-test id, so concurrent test binaries cannot collide.
func newTestStore(t *testing.T, ns string) *Store {
	t.Helper()

	store, err := NewStore(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("NewStore: %v (is docker compose up, with internal/panel/schema.sql applied?)", err)
	}
	t.Cleanup(store.Close)

	cleanup := func() {
		pool, err := pgxpool.New(context.Background(), testDatabaseURL)
		if err != nil {
			t.Logf("cleanup: %v", err)
			return
		}
		defer pool.Close()
		ctx := context.Background()
		// Audit rows outlive their actor by design (ON DELETE SET NULL),
		// so they have to be removed explicitly rather than by cascade.
		for _, sql := range []string{
			`DELETE FROM panel_audit_log WHERE site_id LIKE $1 OR actor_label LIKE $1`,
			`DELETE FROM panel_login_attempts WHERE email LIKE $1`,
			`DELETE FROM panel_dev_access WHERE reason LIKE $1`,
			`DELETE FROM panel_site_members WHERE site_id LIKE $1`,
			`DELETE FROM panel_users WHERE email LIKE $1`,
		} {
			if _, err := pool.Exec(ctx, sql, "%"+ns+"%"); err != nil {
				t.Logf("cleanup %q: %v", sql, err)
			}
		}
	}
	cleanup() // in case a previous run died before its own cleanup
	t.Cleanup(cleanup)

	return store
}

func mustUser(t *testing.T, s *Store, ns, local string, superadmin bool) User {
	t.Helper()
	hash, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	u, err := s.CreateUser(context.Background(), local+"-"+ns+"@example.com", local, hash, superadmin)
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", local, err)
	}
	return u
}

func TestStore_RealDB_CreateAndLoadUser(t *testing.T) {
	ns := "panel-create"
	s := newTestStore(t, ns)
	ctx := context.Background()

	created := mustUser(t, s, ns, "ahmet", false)
	if created.ID == 0 || created.Disabled || created.DeveloperMode || created.IsSuperadmin {
		t.Errorf("a new account has surprising defaults: %+v", created)
	}
	if created.HasTOTP() {
		t.Error("a new account already has two-factor set up")
	}

	byEmail, err := s.UserByEmail(ctx, created.Email)
	if err != nil {
		t.Fatalf("UserByEmail: %v", err)
	}
	byID, err := s.UserByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if byEmail.ID != created.ID || byID.Email != created.Email {
		t.Error("the two lookups disagree about the same account")
	}

	if _, err := s.UserByEmail(ctx, "nobody-"+ns+"@example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing account gave %v, want ErrNotFound", err)
	}
}

// Without normalization the UNIQUE constraint is decorative:
// Ahmet@example.com and ahmet@example.com would be two accounts, and the
// second is a very quiet way to impersonate the first.
func TestStore_RealDB_EmailIsCaseInsensitiveAndUnique(t *testing.T) {
	ns := "panel-email"
	s := newTestStore(t, ns)
	ctx := context.Background()

	hash, _ := HashPassword(goodPassword)
	first, err := s.CreateUser(ctx, "  Ahmet-"+ns+"@Example.COM  ", "Ahmet", hash, false)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if first.Email != "ahmet-"+ns+"@example.com" {
		t.Errorf("stored email = %q, want it lowercased and trimmed", first.Email)
	}

	if _, err := s.CreateUser(ctx, "AHMET-"+ns+"@EXAMPLE.COM", "Impostor", hash, false); !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("creating a differently-cased duplicate gave %v, want ErrEmailTaken", err)
	}

	found, err := s.UserByEmail(ctx, "  AhMeT-"+ns+"@ExAmPlE.cOm ")
	if err != nil || found.ID != first.ID {
		t.Errorf("lookup by a differently-cased address failed: %v", err)
	}
}

func TestStore_RealDB_AccessResolution(t *testing.T) {
	ns := "panel-access"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns

	owner := mustUser(t, s, ns, "owner", false)
	viewer := mustUser(t, s, ns, "viewer", false)
	stranger := mustUser(t, s, ns, "stranger", false)
	staff := mustUser(t, s, ns, "staff", true)

	if err := s.AddMember(ctx, site, owner.ID, RoleOwner, nil); err != nil {
		t.Fatalf("AddMember owner: %v", err)
	}
	if err := s.AddMember(ctx, site, viewer.ID, RoleViewer, &owner.ID); err != nil {
		t.Fatalf("AddMember viewer: %v", err)
	}

	cases := []struct {
		name       string
		user       User
		wantRole   Role
		wantMember bool
		wantManage bool
	}{
		{"owner", owner, RoleOwner, true, true},
		{"viewer", viewer, RoleViewer, true, false},
		{"stranger", stranger, "", false, false},
		{"superadmin without a membership", staff, "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			access, err := s.AccessFor(ctx, principalOf(tc.user), site)
			if err != nil {
				t.Fatalf("AccessFor: %v", err)
			}
			if access.Role != tc.wantRole || access.Member != tc.wantMember {
				t.Errorf("role/member = %q/%v, want %q/%v", access.Role, access.Member, tc.wantRole, tc.wantMember)
			}
			if got := access.Can(CapManageMembers); got != tc.wantManage {
				t.Errorf("Can(manage_members) = %v, want %v", got, tc.wantManage)
			}
			// The property that matters most: a stranger reaches nothing.
			if tc.name == "stranger" && access.Can(CapViewAnalytics) {
				t.Error("a user with no membership could view another customer's analytics")
			}
		})
	}
}

func principalOf(u User) Principal {
	return Principal{
		Kind: PrincipalUser, UserID: u.ID, Label: u.Email,
		Superadmin: u.IsSuperadmin, DeveloperMode: u.DeveloperMode,
	}
}

func TestStore_RealDB_LastOwnerCannotBeRemovedOrDemoted(t *testing.T) {
	ns := "panel-lastowner"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns

	owner := mustUser(t, s, ns, "owner", false)
	admin := mustUser(t, s, ns, "admin", false)
	if err := s.AddMember(ctx, site, owner.ID, RoleOwner, nil); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.AddMember(ctx, site, admin.ID, RoleAdmin, &owner.ID); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	if err := s.RemoveMember(ctx, site, owner.ID); !errors.Is(err, ErrLastOwner) {
		t.Errorf("removing the only owner gave %v, want ErrLastOwner", err)
	}
	if err := s.SetMemberRole(ctx, site, owner.ID, RoleViewer); !errors.Is(err, ErrLastOwner) {
		t.Errorf("demoting the only owner gave %v, want ErrLastOwner - it leaves the site ownerless just as surely as removal", err)
	}

	// With a second owner, both operations become legal again.
	if err := s.SetMemberRole(ctx, site, admin.ID, RoleOwner); err != nil {
		t.Fatalf("promoting the admin: %v", err)
	}
	if err := s.RemoveMember(ctx, site, owner.ID); err != nil {
		t.Errorf("removing one of two owners: %v", err)
	}
}

// The reason RemoveMember uses a transaction with FOR UPDATE. Two
// administrators each seeing "there are 2 owners" and each removing one
// would leave a site nobody can administer - a small race, but one that
// cannot be undone from the UI afterwards.
func TestStore_RealDB_ConcurrentOwnerRemovalLeavesOneStanding(t *testing.T) {
	ns := "panel-ownerrace"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns

	first := mustUser(t, s, ns, "ownerone", false)
	second := mustUser(t, s, ns, "ownertwo", false)
	for _, u := range []User{first, second} {
		if err := s.AddMember(ctx, site, u.ID, RoleOwner, nil); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
	}

	// Both removals fire at once; exactly one must succeed.
	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	for i, u := range []User{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = s.RemoveMember(context.Background(), site, u.ID)
		}()
	}
	close(start)
	wg.Wait()

	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		} else if !errors.Is(err, ErrLastOwner) {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Errorf("%d of 2 concurrent owner removals succeeded, want exactly 1", succeeded)
	}

	members, err := s.Members(ctx, site)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	owners := 0
	for _, m := range members {
		if m.Role == RoleOwner {
			owners++
		}
	}
	if owners != 1 {
		t.Errorf("the site has %d owners after the race, want 1 - it is now unadministrable", owners)
	}
}

func TestStore_RealDB_MembersAreOrderedByAuthority(t *testing.T) {
	ns := "panel-members"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns

	// Deliberately created in an order where alphabetical sorting would
	// disagree with authority sorting.
	viewer := mustUser(t, s, ns, "aaa", false)
	owner := mustUser(t, s, ns, "zzz", false)
	admin := mustUser(t, s, ns, "mmm", false)
	if err := s.AddMember(ctx, site, viewer.ID, RoleViewer, nil); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.AddMember(ctx, site, owner.ID, RoleOwner, nil); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.AddMember(ctx, site, admin.ID, RoleAdmin, nil); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	members, err := s.Members(ctx, site)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("got %d members, want 3", len(members))
	}
	if members[0].Role != RoleOwner || members[1].Role != RoleAdmin || members[2].Role != RoleViewer {
		t.Errorf("member order = %s/%s/%s, want owner/admin/viewer", members[0].Role, members[1].Role, members[2].Role)
	}
	if members[0].Name == "" || members[0].Email == "" {
		t.Error("member rows lost their account details")
	}
}

// AddMember is an upsert, so granting a role to someone who already has
// one must change it rather than fail - which is what the members page
// relies on.
func TestStore_RealDB_AddMemberIsAnUpsert(t *testing.T) {
	ns := "panel-upsert"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns

	u := mustUser(t, s, ns, "user", false)
	if err := s.AddMember(ctx, site, u.ID, RoleViewer, nil); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.AddMember(ctx, site, u.ID, RoleAdmin, nil); err != nil {
		t.Fatalf("AddMember again: %v", err)
	}

	access, err := s.AccessFor(ctx, principalOf(u), site)
	if err != nil {
		t.Fatalf("AccessFor: %v", err)
	}
	if access.Role != RoleAdmin {
		t.Errorf("role = %q after re-granting, want admin", access.Role)
	}
}

func TestStore_RealDB_SitesListing(t *testing.T) {
	ns := "panel-sites"
	s := newTestStore(t, ns)
	ctx := context.Background()
	mine, theirs := "site-mine-"+ns, "site-theirs-"+ns

	me := mustUser(t, s, ns, "me", false)
	them := mustUser(t, s, ns, "them", false)
	staff := mustUser(t, s, ns, "staff", true)
	if err := s.AddMember(ctx, mine, me.ID, RoleOwner, nil); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.AddMember(ctx, theirs, them.ID, RoleOwner, nil); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	sites, err := s.Sites(ctx, principalOf(me), nil)
	if err != nil {
		t.Fatalf("Sites: %v", err)
	}
	if len(sites) != 1 || sites[0].SiteID != mine || sites[0].Role != RoleOwner {
		t.Errorf("my sites = %+v, want only %q as owner", sites, mine)
	}

	// A superadmin sees every site with a membership, plus sites that
	// have data but nobody assigned yet - without which they could never
	// grant the first membership on a new site.
	fresh := "site-fresh-" + ns
	all, err := s.Sites(ctx, principalOf(staff), []string{fresh, mine})
	if err != nil {
		t.Fatalf("Sites(superadmin): %v", err)
	}
	seen := map[string]bool{}
	for _, sa := range all {
		seen[sa.SiteID] = true
		if !sa.ViaSuperadmin {
			t.Errorf("%q was not marked as reached via superadmin", sa.SiteID)
		}
	}
	for _, want := range []string{mine, theirs, fresh} {
		if !seen[want] {
			t.Errorf("superadmin did not see %q; got %v", want, seen)
		}
	}
	// The known list must not produce a duplicate for a site that
	// already has members.
	count := 0
	for _, sa := range all {
		if sa.SiteID == mine {
			count++
		}
	}
	if count != 1 {
		t.Errorf("%q appeared %d times, want once", mine, count)
	}
}

func TestStore_RealDB_AuditIsAppendedAndFiltered(t *testing.T) {
	ns := "panel-audit"
	s := newTestStore(t, ns)
	ctx := context.Background()
	siteA, siteB := "site-a-"+ns, "site-b-"+ns

	actor := mustUser(t, s, ns, "actor", false)
	ip := netip.MustParseAddr("203.0.113.9")

	if err := s.RecordFor(ctx, principalOf(actor), AuditEntry{
		Action: ActionMemberAdded, SiteID: siteA, Target: "someone@example.com",
		Detail: map[string]any{"role": "viewer"}, IP: &ip, UserAgent: "test",
	}); err != nil {
		t.Fatalf("RecordFor: %v", err)
	}
	if err := s.RecordFor(ctx, developerPrincipal(), AuditEntry{
		Action: ActionTokenCreated, SiteID: siteB, Target: "panel-token",
	}); err != nil {
		t.Fatalf("RecordFor developer: %v", err)
	}

	entries, total, err := s.Audit(ctx, AuditFilter{SiteID: siteA, Limit: 50})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if total != 1 || len(entries) != 1 {
		t.Fatalf("site A audit = %d entries (total %d), want 1", len(entries), total)
	}
	e := entries[0]
	if e.Action != ActionMemberAdded || e.ActorKind != PrincipalUser || e.ActorLabel != actor.Email {
		t.Errorf("entry = %+v", e)
	}
	if e.ActorID == nil || *e.ActorID != actor.ID {
		t.Error("the entry lost its actor id")
	}
	if e.Detail["role"] != "viewer" {
		t.Errorf("detail = %v, want role=viewer", e.Detail)
	}
	if e.IP == nil || *e.IP != ip {
		t.Errorf("ip = %v, want %v", e.IP, ip)
	}

	// A developer session must be distinguishable from a user, which is
	// the whole point of giving it its own actor kind.
	devEntries, _, err := s.Audit(ctx, AuditFilter{SiteID: siteB, Limit: 50})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(devEntries) != 1 {
		t.Fatalf("site B audit = %d entries, want 1", len(devEntries))
	}
	if devEntries[0].ActorKind != PrincipalDeveloper || devEntries[0].ActorLabel != DeveloperLabel {
		t.Errorf("developer entry = %+v, want it filed under the developer identity", devEntries[0])
	}
	if devEntries[0].ActorID != nil {
		t.Error("a developer entry claimed a user id; there is no account behind it")
	}

	// A non-superadmin reading the log is limited to the sites they
	// administer, expressed as a non-nil set.
	scoped, _, err := s.Audit(ctx, AuditFilter{Sites: []string{siteA}, Limit: 50})
	if err != nil {
		t.Fatalf("Audit(scoped): %v", err)
	}
	for _, entry := range scoped {
		if entry.SiteID != siteA {
			t.Errorf("a scoped read returned an entry for %q", entry.SiteID)
		}
	}
	none, total, err := s.Audit(ctx, AuditFilter{Sites: []string{}, Limit: 50})
	if err != nil {
		t.Fatalf("Audit(empty scope): %v", err)
	}
	if len(none) != 0 || total != 0 {
		t.Error("an empty site scope returned entries; somebody who administers no site must see nothing")
	}
}

// The audit table must survive its actor: revoking access by deleting an
// account should not erase what that account did.
func TestStore_RealDB_AuditOutlivesItsActor(t *testing.T) {
	ns := "panel-auditlife"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns

	actor := mustUser(t, s, ns, "temporary", false)
	if err := s.RecordFor(ctx, principalOf(actor), AuditEntry{Action: ActionLoginSucceeded, SiteID: site}); err != nil {
		t.Fatalf("RecordFor: %v", err)
	}

	pool, err := pgxpool.New(ctx, testDatabaseURL)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `DELETE FROM panel_users WHERE id = $1`, actor.ID); err != nil {
		t.Fatalf("deleting the actor: %v", err)
	}

	entries, _, err := s.Audit(ctx, AuditFilter{SiteID: site, Limit: 50})
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("the entry vanished with its actor: got %d", len(entries))
	}
	if entries[0].ActorLabel != actor.Email {
		t.Errorf("actor label = %q, want the address captured at the time (%q)", entries[0].ActorLabel, actor.Email)
	}
	if entries[0].ActorID != nil {
		t.Error("actor_id should have been nulled by the cascade, leaving only the captured label")
	}
}

// wipeUsers empties panel_users so a test can observe the deployment in
// its "nobody owns this yet" state.
//
// That state is a global property of the database, not something a test
// can namespace its way around, so these tests deliberately clear the
// whole table. Safe against the throwaway docker database the
// integration suite targets; do not point this suite at anything you
// care about.
func wipeUsers(t *testing.T) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(context.Background(), `DELETE FROM panel_users`); err != nil {
		t.Fatalf("wiping users: %v", err)
	}
}

// Before anyone owns the deployment there is nobody to ask, and
// installing the system is exactly what developer access is for.
func TestStore_RealDB_DevAccessIsAutoApprovedBeforeSetup(t *testing.T) {
	ns := "panel-devboot"
	s := newTestStore(t, ns)
	ctx := context.Background()
	wipeUsers(t)

	token, req, err := s.RequestDevAccess(ctx, "kurulum-"+ns, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("RequestDevAccess: %v", err)
	}
	if !req.AutoApproved || req.ApprovedAt == nil {
		t.Fatalf("request was not auto-approved with no accounts present: %+v", req)
	}

	grant, err := s.RedeemDevAccess(ctx, token, netip.MustParseAddr("198.51.100.7"))
	if err != nil {
		t.Fatalf("redeeming a bootstrap link: %v", err)
	}
	if !grant.Bootstrap {
		t.Error("the grant did not report itself as a bootstrap session")
	}
	if grant.ExpiresAt.Before(time.Now()) {
		t.Error("the session expired before it began")
	}
}

// The rule the whole approval flow exists for: shell access is enough
// to get in before anyone has an account, and stops being enough the
// moment somebody does. An installer link left over from setup must not
// quietly remain a way in afterwards.
func TestStore_RealDB_BootstrapLinkDiesWhenAnAccountAppears(t *testing.T) {
	ns := "panel-devboot-dies"
	s := newTestStore(t, ns)
	ctx := context.Background()
	wipeUsers(t)

	token, req, err := s.RequestDevAccess(ctx, "kurulum-"+ns, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("RequestDevAccess: %v", err)
	}
	if !req.AutoApproved {
		t.Fatalf("expected an auto-approved request, got %+v", req)
	}

	// The site owner finishes setup. The link has not expired and has
	// never been used - and must now be dead anyway.
	mustUser(t, s, ns, "owner", false)

	if _, err := s.RedeemDevAccess(ctx, token, netip.MustParseAddr("198.51.100.7")); !errors.Is(err, ErrDevAccessInvalid) {
		t.Fatalf("a bootstrap link still worked after the owner created their account: %v", err)
	}
}

// Once there is an owner, a request is inert until they say yes.
func TestStore_RealDB_DevAccessNeedsApprovalAfterSetup(t *testing.T) {
	ns := "panel-devapprove"
	s := newTestStore(t, ns)
	ctx := context.Background()
	wipeUsers(t)

	owner := mustUser(t, s, ns, "owner", false)

	token, req, err := s.RequestDevAccess(ctx, "bakim-"+ns, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("RequestDevAccess: %v", err)
	}
	if req.AutoApproved || req.ApprovedAt != nil {
		t.Fatalf("a request was auto-approved despite an account existing: %+v", req)
	}
	if !req.Pending() {
		t.Error("a fresh request does not report itself as pending")
	}

	// Unapproved: the token exists but opens nothing.
	if _, err := s.RedeemDevAccess(ctx, token, netip.Addr{}); !errors.Is(err, ErrDevAccessInvalid) {
		t.Fatalf("an unapproved link was redeemable: %v", err)
	}

	pending, err := s.PendingDevAccess(ctx)
	if err != nil {
		t.Fatalf("PendingDevAccess: %v", err)
	}
	// Scoped to this test's own request rather than asserting the list
	// has exactly one entry. A database that has been used by anything
	// else - another suite, an earlier run, a developer poking at the
	// wizard - would fail the stricter form for a reason that has
	// nothing to do with what is being tested here.
	mine := findPending(pending, req.ID)
	if mine == nil {
		t.Fatalf("this request is not listed as pending: %+v", pending)
	}
	if mine.Reason != "bakim-"+ns {
		t.Fatalf("pending entry = %+v, want reason %q", mine, "bakim-"+ns)
	}

	if err := s.ApproveDevAccess(ctx, req.ID, owner); err != nil {
		t.Fatalf("ApproveDevAccess: %v", err)
	}

	grant, err := s.RedeemDevAccess(ctx, token, netip.Addr{})
	if err != nil {
		t.Fatalf("redeeming an approved link: %v", err)
	}
	if grant.Bootstrap {
		t.Error("a human-approved grant reported itself as a bootstrap session")
	}
	// Single use still holds.
	if _, err := s.RedeemDevAccess(ctx, token, netip.Addr{}); !errors.Is(err, ErrDevAccessInvalid) {
		t.Errorf("an approved link was redeemable twice: %v", err)
	}
}

func TestStore_RealDB_DeniedDevAccessStaysDenied(t *testing.T) {
	ns := "panel-devdeny"
	s := newTestStore(t, ns)
	ctx := context.Background()
	wipeUsers(t)

	owner := mustUser(t, s, ns, "owner", false)
	token, req, err := s.RequestDevAccess(ctx, "reddedilecek-"+ns, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("RequestDevAccess: %v", err)
	}

	if err := s.DenyDevAccess(ctx, req.ID, owner); err != nil {
		t.Fatalf("DenyDevAccess: %v", err)
	}
	if _, err := s.RedeemDevAccess(ctx, token, netip.Addr{}); !errors.Is(err, ErrDevAccessInvalid) {
		t.Errorf("a denied link was redeemable: %v", err)
	}
	// A refusal cannot be quietly reversed: the developer has to ask
	// again, so the owner sees a fresh request rather than a decision
	// they already made being overwritten.
	if err := s.ApproveDevAccess(ctx, req.ID, owner); !errors.Is(err, ErrDevAccessDecided) {
		t.Errorf("a denied request was approvable afterwards: %v", err)
	}
	if err := s.DenyDevAccess(ctx, req.ID, owner); !errors.Is(err, ErrDevAccessDecided) {
		t.Errorf("denying twice succeeded: %v", err)
	}
}

// Two owners deciding at the same moment - one approving, one denying -
// must not both succeed, or the record would say the request was
// approved and denied.
func TestStore_RealDB_ConcurrentDevAccessDecisionAdmitsOne(t *testing.T) {
	ns := "panel-devdecide"
	s := newTestStore(t, ns)
	ctx := context.Background()
	wipeUsers(t)

	first := mustUser(t, s, ns, "ownerone", false)
	second := mustUser(t, s, ns, "ownertwo", false)
	_, req, err := s.RequestDevAccess(ctx, "yaris-"+ns, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("RequestDevAccess: %v", err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	start := make(chan struct{})
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errs[0] = s.ApproveDevAccess(context.Background(), req.ID, first)
	}()
	go func() {
		defer wg.Done()
		<-start
		errs[1] = s.DenyDevAccess(context.Background(), req.ID, second)
	}()
	close(start)
	wg.Wait()

	decided := 0
	for _, err := range errs {
		switch {
		case err == nil:
			decided++
		case errors.Is(err, ErrDevAccessDecided):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if decided != 1 {
		t.Errorf("%d of 2 simultaneous decisions succeeded, want exactly 1", decided)
	}
}

// This grants operator authority, so "usually single-use" is not good
// enough: the redemption is one statement precisely so concurrent
// attempts cannot both pass.
func TestStore_RealDB_ConcurrentDevAccessRedemptionAdmitsOne(t *testing.T) {
	ns := "panel-devrace"
	s := newTestStore(t, ns)
	ctx := context.Background()
	wipeUsers(t)

	owner := mustUser(t, s, ns, "owner", false)
	token, req, err := s.RequestDevAccess(ctx, "yaris-"+ns, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("RequestDevAccess: %v", err)
	}
	if err := s.ApproveDevAccess(ctx, req.ID, owner); err != nil {
		t.Fatalf("ApproveDevAccess: %v", err)
	}

	const attempts = 16
	var wg sync.WaitGroup
	results := make([]error, attempts)
	start := make(chan struct{})
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, results[i] = s.RedeemDevAccess(context.Background(), token, netip.MustParseAddr("198.51.100.7"))
		}()
	}
	close(start)
	wg.Wait()

	succeeded := 0
	for _, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrDevAccessInvalid):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if succeeded != 1 {
		t.Errorf("%d of %d concurrent redemptions succeeded, want exactly 1", succeeded, attempts)
	}
}

func TestStore_RealDB_DevAccessExpires(t *testing.T) {
	ns := "panel-devexpiry"
	s := newTestStore(t, ns)
	ctx := context.Background()
	wipeUsers(t)

	owner := mustUser(t, s, ns, "owner", false)
	token, req, err := s.RequestDevAccess(ctx, "eskiyecek-"+ns, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("RequestDevAccess: %v", err)
	}
	if err := s.ApproveDevAccess(ctx, req.ID, owner); err != nil {
		t.Fatalf("ApproveDevAccess: %v", err)
	}

	// Age the row rather than minting it pre-expired: a non-positive TTL
	// means "use the default", so the only way to reach this state
	// through the API is the way it happens in production - time passing.
	pool, err := pgxpool.New(ctx, testDatabaseURL)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx,
		`UPDATE panel_dev_access SET request_expires_at = now() - interval '1 second' WHERE id = $1`,
		req.ID); err != nil {
		t.Fatalf("ageing the request: %v", err)
	}

	if _, err := s.RedeemDevAccess(ctx, token, netip.Addr{}); !errors.Is(err, ErrDevAccessInvalid) {
		t.Errorf("an expired link redeemed: %v", err)
	}
	pending, err := s.PendingDevAccess(ctx)
	if err != nil {
		t.Fatalf("PendingDevAccess: %v", err)
	}
	if findPending(pending, req.ID) != nil {
		t.Errorf("an expired request is still listed as pending: %+v", pending)
	}
}

// An expired request cannot be revived by approving it late: the owner
// would think they were granting a fresh visit.
func TestStore_RealDB_ExpiredDevAccessCannotBeApproved(t *testing.T) {
	ns := "panel-devlate"
	s := newTestStore(t, ns)
	ctx := context.Background()
	wipeUsers(t)

	owner := mustUser(t, s, ns, "owner", false)
	_, req, err := s.RequestDevAccess(ctx, "gec-"+ns, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("RequestDevAccess: %v", err)
	}

	pool, err := pgxpool.New(ctx, testDatabaseURL)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx,
		`UPDATE panel_dev_access SET request_expires_at = now() - interval '1 second' WHERE id = $1`,
		req.ID); err != nil {
		t.Fatalf("ageing the request: %v", err)
	}

	if err := s.ApproveDevAccess(ctx, req.ID, owner); !errors.Is(err, ErrDevAccessDecided) {
		t.Errorf("an expired request was approvable: %v", err)
	}
}

func TestStore_RealDB_DevAccessDefaultsAndHistory(t *testing.T) {
	ns := "panel-devttl"
	s := newTestStore(t, ns)
	ctx := context.Background()
	wipeUsers(t)

	// Zero durations mean "use the defaults", never "already expired" -
	// a caller that forgot to pass one should get a usable request, not
	// a dead one that fails confusingly later.
	_, req, err := s.RequestDevAccess(ctx, "varsayilan-"+ns, 0, 0)
	if err != nil {
		t.Fatalf("RequestDevAccess: %v", err)
	}
	if req.SessionTTL != DefaultSessionTTL {
		t.Errorf("SessionTTL = %v, want the default %v", req.SessionTTL, DefaultSessionTTL)
	}
	if until := time.Until(req.RequestExpiresAt); until <= 0 || until > DefaultRequestTTL+time.Minute {
		t.Errorf("request expires in %v, want about the default %v", until, DefaultRequestTTL)
	}

	// The history is what lets an owner see who asked and what happened,
	// including requests that were never approved.
	recent, err := s.RecentDevAccess(ctx, 10)
	if err != nil {
		t.Fatalf("RecentDevAccess: %v", err)
	}
	if len(recent) == 0 || recent[0].Reason != "varsayilan-"+ns {
		t.Errorf("recent = %+v, want the request just made", recent)
	}
}

func TestStore_RealDB_LoginThrottling(t *testing.T) {
	ns := "panel-throttle"
	s := newTestStore(t, ns)
	ctx := context.Background()
	email := "victim-" + ns + "@example.com"
	attacker := netip.MustParseAddr("203.0.113.66")

	if th, err := s.CheckLoginThrottle(ctx, email, attacker); err != nil || th.Blocked {
		t.Fatalf("a fresh account was throttled: %+v (%v)", th, err)
	}

	for range maxFailuresPerEmail {
		if err := s.RecordLoginAttempt(ctx, email, attacker, false); err != nil {
			t.Fatalf("RecordLoginAttempt: %v", err)
		}
	}

	th, err := s.CheckLoginThrottle(ctx, email, attacker)
	if err != nil {
		t.Fatalf("CheckLoginThrottle: %v", err)
	}
	if !th.Blocked || th.Reason != "email" {
		t.Errorf("throttle = %+v, want blocked on the email counter", th)
	}

	// A successful login clears that account's failures, so someone who
	// mistyped four times and then got it right does not start their
	// next session already halfway to a lockout.
	if err := s.ClearLoginFailures(ctx, email); err != nil {
		t.Fatalf("ClearLoginFailures: %v", err)
	}
	if th, err := s.CheckLoginThrottle(ctx, email, attacker); err != nil || th.Blocked {
		t.Errorf("still throttled after a successful login: %+v (%v)", th, err)
	}
}

// The per-IP counter catches what the per-account one cannot: one
// password sprayed across many addresses, where no single account ever
// accumulates enough failures to trip.
func TestStore_RealDB_ThrottlesPasswordSprayingAcrossAccounts(t *testing.T) {
	ns := "panel-spray"
	s := newTestStore(t, ns)
	ctx := context.Background()
	attacker := netip.MustParseAddr("203.0.113.77")

	for i := range maxFailuresPerIP {
		email := fmt.Sprintf("target%d-%s@example.com", i, ns)
		if err := s.RecordLoginAttempt(ctx, email, attacker, false); err != nil {
			t.Fatalf("RecordLoginAttempt: %v", err)
		}
		// Each individual account is nowhere near its own limit.
		if th, err := s.CheckLoginThrottle(ctx, email, netip.MustParseAddr("198.51.100.1")); err != nil || th.Blocked {
			t.Fatalf("account %s was blocked by its own counter, which is not what this test is about", email)
		}
	}

	th, err := s.CheckLoginThrottle(ctx, "fresh-"+ns+"@example.com", attacker)
	if err != nil {
		t.Fatalf("CheckLoginThrottle: %v", err)
	}
	if !th.Blocked || th.Reason != "ip" {
		t.Errorf("throttle = %+v, want blocked on the address counter", th)
	}
}

func TestStore_RealDB_UpdatesReportMissingRows(t *testing.T) {
	ns := "panel-missing"
	s := newTestStore(t, ns)
	ctx := context.Background()

	const noSuchUser = int64(-1)
	if err := s.SetDeveloperMode(ctx, noSuchUser, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("updating a missing user gave %v, want ErrNotFound - a silent no-op here would look like success in the UI", err)
	}
	if err := s.SetPasswordHash(ctx, noSuchUser, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetPasswordHash on a missing user gave %v, want ErrNotFound", err)
	}
	if err := s.RemoveMember(ctx, "site-"+ns, noSuchUser); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing a missing member gave %v, want ErrNotFound", err)
	}
}

func TestStore_RealDB_UserMutations(t *testing.T) {
	ns := "panel-mutate"
	s := newTestStore(t, ns)
	ctx := context.Background()

	u := mustUser(t, s, ns, "user", false)

	if err := s.SetDeveloperMode(ctx, u.ID, true); err != nil {
		t.Fatalf("SetDeveloperMode: %v", err)
	}
	if err := s.SetTOTPSecret(ctx, u.ID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatalf("SetTOTPSecret: %v", err)
	}
	if err := s.SetDisplayName(ctx, u.ID, "  Ahmet Yılmaz  "); err != nil {
		t.Fatalf("SetDisplayName: %v", err)
	}
	if err := s.SetDisabled(ctx, u.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	if err := s.TouchLastLogin(ctx, u.ID); err != nil {
		t.Fatalf("TouchLastLogin: %v", err)
	}

	got, err := s.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if !got.DeveloperMode || !got.HasTOTP() || !got.Disabled {
		t.Errorf("mutations did not stick: %+v", got)
	}
	if got.DisplayName != "Ahmet Yılmaz" {
		t.Errorf("DisplayName = %q, want it trimmed", got.DisplayName)
	}
	if got.LastLoginAt == nil {
		t.Error("LastLoginAt was not set")
	}

	newHash, _ := HashPassword("yeni-parola-123456")
	if err := s.SetPasswordHash(ctx, u.ID, newHash); err != nil {
		t.Fatalf("SetPasswordHash: %v", err)
	}
	reloaded, err := s.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if ok, _ := VerifyPassword(reloaded.PasswordHash, "yeni-parola-123456"); !ok {
		t.Error("the new password does not verify against the stored hash")
	}
	if ok, _ := VerifyPassword(reloaded.PasswordHash, goodPassword); ok {
		t.Error("the old password still verifies after a change")
	}
}

// findPending returns this test's own request out of the list, or nil.
//
// Every assertion about the pending list goes through here rather than
// checking its length. A test that only passes against a pristine
// database stops testing the moment anybody uses the database for
// anything else, and reports success or failure for reasons unrelated
// to the code under test.
func findPending(pending []DevAccessRequest, id int64) *DevAccessRequest {
	for i := range pending {
		if pending[i].ID == id {
			return &pending[i]
		}
	}
	return nil
}
