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
//	./release/install.sh   # see internal/testdb for the whole recipe
//	go test -tags integration ./internal/panel/... -v

package panel

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The role the panel actually runs as.
//
// It was `collector` until an end-to-end run of the installed package
// showed why that mattered: the development database had been created by
// collector, so collector owned every table and this suite ran with
// authority no deployment grants it. Three real holes were hiding behind
// that - a retention feature that had never worked, two ungranted
// tables - and none of them could have been caught from here.
//
// A suite that tests a role-separated design has to connect as the role.
const testDatabaseURL = "postgres://panel_user:panel_user@localhost:5432/analytics"

// newTestStore opens a Store and removes everything the test created
// afterwards. Accounts are namespaced by a per-test email suffix and
// sites by a per-test id, so concurrent test binaries cannot collide.
func newTestStore(t *testing.T, ns string) *Store {
	t.Helper()

	store, err := NewStore(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("NewStore: %v (is the database up and installed? see internal/testdb)", err)
	}
	t.Cleanup(store.Close)
	testdb.Lock(t, store.Pool(), testdb.AccountsLock)

	// Cleared through the schema's owner, not the panel's own pool.
	//
	// panel_user holds SELECT and INSERT on panel_audit_log and no
	// DELETE - "nobody can erase the audit log" is an assertion in
	// release/sql/verify.sql - so on a properly installed database the
	// panel cannot remove its own audit rows, correctly. It could here
	// only because the development database had been created by the
	// role the tests connect as.
	admin := testdb.Admin(t)
	cleanup := func() {
		pool := admin
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
		// panel_smtp is a single global row, so it cannot be namespaced
		// the way everything above is. Removed unconditionally instead,
		// which is safe because the suite holds the advisory lock: no
		// other run is looking at this database.
		//
		// Unconditional rather than conditional on a test having written
		// one. A row left behind makes the *next* suite's "nothing is
		// configured" case start from something configured, and that
		// test would pass or fail depending on which tests ran before
		// it - the exact shape of failure the namespacing above exists
		// to prevent.
		if _, err := pool.Exec(ctx, `DELETE FROM panel_smtp`); err != nil {
			t.Logf("cleanup panel_smtp: %v", err)
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

	if err := s.AddMember(ctx, site, owner.ID, RoleOwner, Grant{}); err != nil {
		t.Fatalf("AddMember owner: %v", err)
	}
	if err := s.AddMember(ctx, site, viewer.ID, RoleViewer, Grant{By: &owner.ID}); err != nil {
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
	if err := s.AddMember(ctx, site, owner.ID, RoleOwner, Grant{}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.AddMember(ctx, site, admin.ID, RoleAdmin, Grant{By: &owner.ID}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	if err := s.RemoveMember(ctx, site, owner.ID, nil); !errors.Is(err, ErrLastOwner) {
		t.Errorf("removing the only owner gave %v, want ErrLastOwner", err)
	}
	if err := s.SetMemberRole(ctx, site, owner.ID, RoleViewer, nil); !errors.Is(err, ErrLastOwner) {
		t.Errorf("demoting the only owner gave %v, want ErrLastOwner - it leaves the site ownerless just as surely as removal", err)
	}

	// With a second owner, both operations become legal again.
	if err := s.SetMemberRole(ctx, site, admin.ID, RoleOwner, nil); err != nil {
		t.Fatalf("promoting the admin: %v", err)
	}
	if err := s.RemoveMember(ctx, site, owner.ID, nil); err != nil {
		t.Errorf("removing one of two owners: %v", err)
	}
}

// CanAssign had no counterpart, and three writers went through the gap.
//
// The rule it states - nobody may grant authority above their own - was
// only ever asked about the role being handed out. Nothing asked about
// the role being taken away, so an administrator could not make an owner
// and could unmake one, by any of three routes. All three were measured
// against this database before the check existed; all three left the
// owner holding "viewer".
func TestStore_RealDB_AnAdminCannotUnmakeAnOwner(t *testing.T) {
	ns := "panel-unmake"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns

	owner := mustUser(t, s, ns, "owner", false)
	second := mustUser(t, s, ns, "ownertwo", false)
	admin := mustUser(t, s, ns, "admin", false)
	for _, u := range []User{owner, second} {
		if err := s.AddMember(ctx, site, u.ID, RoleOwner, Grant{}); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
	}
	if err := s.AddMember(ctx, site, admin.ID, RoleAdmin, Grant{}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	// A second owner stands, so nothing here is refused by the
	// last-owner rule. What refuses it is the admin's own authority.
	for _, tc := range []struct {
		what string
		do   func() error
	}{
		{"re-adding an owner with a lower role", func() error {
			return s.AddMember(ctx, site, owner.ID, RoleViewer, Grant{By: &admin.ID})
		}},
		{"setting an owner's role", func() error {
			return s.SetMemberRole(ctx, site, owner.ID, RoleViewer, &admin.ID)
		}},
		{"removing an owner", func() error {
			return s.RemoveMember(ctx, site, owner.ID, &admin.ID)
		}},
	} {
		if err := tc.do(); !errors.Is(err, ErrNotPermitted) {
			t.Errorf("an admin %s gave %v, want ErrNotPermitted", tc.what, err)
		}
	}

	// And the owner is untouched by all three attempts.
	access, err := s.AccessFor(ctx, principalOf(owner), site)
	if err != nil {
		t.Fatalf("AccessFor: %v", err)
	}
	if access.Role != RoleOwner {
		t.Errorf("the owner's role is now %q, want owner", access.Role)
	}

	// The half that was always stated, asserted here against the store
	// rather than only through the page. Both halves live in one
	// transaction now, and a rule that only the handler enforces is a
	// rule the next handler can forget.
	if err := s.AddMember(ctx, site, admin.ID, RoleOwner, Grant{By: &admin.ID}); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("an admin making themselves an owner gave %v, want ErrNotPermitted", err)
	}
}

// The other half, and it needs its own test because the authority check
// above would hide it: an owner *may* act on an owner, so only the
// structural rule stands between a site and having nobody who owns it.
//
// Re-granting was the route that skipped that rule entirely. Removal and
// demotion had it from the beginning; AddMember wrote the same change
// with a different verb and no check at all.
func TestStore_RealDB_ReGrantingCannotStripTheLastOwner(t *testing.T) {
	ns := "panel-regrant"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns

	owner := mustUser(t, s, ns, "owner", false)
	if err := s.AddMember(ctx, site, owner.ID, RoleOwner, Grant{}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	if err := s.AddMember(ctx, site, owner.ID, RoleViewer, Grant{By: &owner.ID}); !errors.Is(err, ErrLastOwner) {
		t.Errorf("re-granting the only owner a lower role gave %v, want ErrLastOwner", err)
	}

	var owners int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM panel_site_members WHERE site_id = $1 AND role = 'owner'`,
		site).Scan(&owners); err != nil {
		t.Fatalf("counting owners: %v", err)
	}
	if owners != 1 {
		t.Errorf("the site has %d owners, want 1 - a site with none cannot be repaired from the panel", owners)
	}
}

// The operator is not caught by the rule above, and should not be.
//
// Hosting the deployment is what superadmin means; a rule that stopped
// the operator from repairing a customer's site would only mean the
// operator editing the table by hand.
func TestStore_RealDB_TheOperatorMayStillActOnAnOwner(t *testing.T) {
	ns := "panel-opowner"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns

	owner := mustUser(t, s, ns, "owner", false)
	second := mustUser(t, s, ns, "ownertwo", false)
	staff := mustUser(t, s, ns, "staff", true)
	for _, u := range []User{owner, second} {
		if err := s.AddMember(ctx, site, u.ID, RoleOwner, Grant{}); err != nil {
			t.Fatalf("AddMember: %v", err)
		}
	}

	if err := s.SetMemberRole(ctx, site, owner.ID, RoleViewer, &staff.ID); err != nil {
		t.Errorf("the operator demoting an owner: %v", err)
	}
}

// Authority is read from the database inside the writing transaction,
// not from whatever the caller believed when it started.
//
// A demoted administrator whose session is still open is the ordinary
// case, and the demotion has to reach them without anybody logging them
// out.
func TestStore_RealDB_AuthorityIsReadAtWriteTime(t *testing.T) {
	ns := "panel-livewrite"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns

	owner := mustUser(t, s, ns, "owner", false)
	admin := mustUser(t, s, ns, "admin", false)
	target := mustUser(t, s, ns, "target", false)
	if err := s.AddMember(ctx, site, owner.ID, RoleOwner, Grant{}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.AddMember(ctx, site, admin.ID, RoleAdmin, Grant{}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.AddMember(ctx, site, target.ID, RoleViewer, Grant{}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	// While they are an admin, this is allowed.
	if err := s.SetMemberRole(ctx, site, target.ID, RoleAdmin, &admin.ID); err != nil {
		t.Fatalf("an admin re-roling a viewer: %v", err)
	}

	// Demoted, and the same call is refused - with no session involved.
	if err := s.SetMemberRole(ctx, site, admin.ID, RoleViewer, &owner.ID); err != nil {
		t.Fatalf("demoting the admin: %v", err)
	}
	if err := s.SetMemberRole(ctx, site, target.ID, RoleViewer, &admin.ID); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("a demoted admin re-roling somebody gave %v, want ErrNotPermitted", err)
	}

	// A disabled account is refused too, whatever role its row still
	// carries. Sessions outlive the switch that turns an account off.
	if err := s.SetDisabled(ctx, owner.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	if err := s.SetMemberRole(ctx, site, target.ID, RoleAdmin, &owner.ID); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("a disabled owner re-roling somebody gave %v, want ErrNotPermitted", err)
	}
}

// The claim the whole phase rests on: an expired membership grants
// nothing, and nothing had to run for that to be true.
//
// The test asserts the second half as well as the first. No sweeper is
// called, and the row is still sitting in the table when the assertions
// are made - so what refused the access was the reading query, not a job
// that had tidied the row away. An expiry enforced by a job is an expiry
// that lasts until the job runs, and that window is one nobody watches.
func TestStore_RealDB_AnExpiredMembershipGrantsNothing(t *testing.T) {
	ns := "panel-expired"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns

	owner := mustUser(t, s, ns, "owner", false)
	guest := mustUser(t, s, ns, "guest", false)
	if err := s.AddMember(ctx, site, owner.ID, RoleOwner, Grant{}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	if err := s.AddMember(ctx, site, guest.ID, RoleAdmin, Grant{By: &owner.ID, Until: &past}); err != nil {
		t.Fatalf("AddMember with an end date: %v", err)
	}

	access, err := s.AccessFor(ctx, principalOf(guest), site)
	if err != nil {
		t.Fatalf("AccessFor: %v", err)
	}
	if access.Role != "" || access.Member {
		t.Errorf("an expired membership resolved to role %q, member=%v", access.Role, access.Member)
	}
	if access.Can(CapViewAnalytics) {
		t.Error("an expired membership still carries a capability")
	}

	sites, err := s.Sites(ctx, principalOf(guest), nil)
	if err != nil {
		t.Fatalf("Sites: %v", err)
	}
	for _, sa := range sites {
		if sa.SiteID == site {
			t.Error("the site is still in the expired member's own site list")
		}
	}

	// And the row is still there, untouched. This is what makes the
	// assertions above about the query rather than about a cleanup.
	var rows int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM panel_site_members WHERE site_id = $1 AND user_id = $2`,
		site, guest.ID).Scan(&rows); err != nil {
		t.Fatalf("counting: %v", err)
	}
	if rows != 1 {
		t.Fatalf("the membership row is gone (%d rows); then the access was refused by something "+
			"having removed it, which is not what this phase claims", rows)
	}

	// The page can still see it, and knows it has ended.
	members, err := s.Members(ctx, site)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	var found bool
	for _, m := range members {
		if m.UserID != guest.ID {
			continue
		}
		found = true
		if !m.Expired || m.Expires == nil {
			t.Errorf("the member list reports Expired=%v Expires=%v for a membership that has ended",
				m.Expired, m.Expires)
		}
	}
	if !found {
		t.Error("an expired membership vanished from the member list; then nothing can explain it")
	}
}

// The page's label and the access decision are two different queries,
// and this is the assertion that they can never mean different things.
//
// # The failure being guarded against
//
// The members page says "this access has ended" while the reading query
// still grants it. That is the quiet direction: nobody re-checks a person
// they believe is already out, so the access could stand for as long as
// the site exists. The reverse - listed as live, refused at the door - is
// loud, and somebody complains within the hour.
//
// # Why PostgreSQL is asked rather than Go
//
// The two conditions differ only by a NOT, and the whole subtlety is
// three-valued logic: NULL means "no end date", and a hand-written
// complement of the form `expires_at <= now()` returns NULL rather than
// TRUE for those rows. NULL is not TRUE, so it happens to behave - but
// "happens to" is the word that makes this a test rather than a comment.
// The strings come from the same functions the real queries use, so
// editing either one moves this test.
func TestTheTwoHalvesOfExpiryAreExactComplements(t *testing.T) {
	s := newTestStore(t, "panel-tamlayici")
	ctx := context.Background()

	rows, err := s.pool.Query(ctx, `
		SELECT m.label, `+liveMembership("m")+`, `+endedMembership("m")+`
		  FROM (VALUES
		    ('bitis yok',  NULL::timestamptz),
		    ('gecmis',     now() - interval '1 hour'),
		    ('tam simdi',  now()),
		    ('gelecek',    now() + interval '1 hour')
		  ) AS m(label, expires_at)`)
	if err != nil {
		t.Fatalf("asking the database: %v", err)
	}
	defer rows.Close()

	want := map[string]bool{ // true means the membership still grants access
		"bitis yok": true, "gecmis": false, "tam simdi": false, "gelecek": true,
	}
	seen := 0
	for rows.Next() {
		var label string
		var live, ended bool
		if err := rows.Scan(&label, &live, &ended); err != nil {
			t.Fatal(err)
		}
		seen++
		if live == ended {
			t.Errorf("%s: live=%v ended=%v - a row that is both, or neither, is a row the "+
				"page and the door disagree about", label, live, ended)
		}
		if live != want[label] {
			t.Errorf("%s: the membership is live=%v, want %v", label, live, want[label])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != len(want) {
		t.Fatalf("%d cases came back, wrote %d - a case that never runs proves nothing", seen, len(want))
	}
}

// The same guarantee one level up: for real rows, what the members page
// says about somebody and what the door does to them always agree.
//
// Three readers rather than two. The page's label, the per-site
// authorization choke point, and the person's own site list are three
// separate queries against the same table, and a customer meets all
// three. Any pair of them disagreeing is the panel lying to somebody.
func TestWhatThePageSaysAndWhatTheDoorDoesAgree(t *testing.T) {
	ns := "panel-mutabakat"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns

	owner := mustUser(t, s, ns, "owner", false)
	if err := s.AddMember(ctx, site, owner.ID, RoleOwner, Grant{}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	past, future := time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)
	people := map[string]User{
		"suresiz": mustUser(t, s, ns, "suresiz", false),
		"canli":   mustUser(t, s, ns, "canli", false),
		"dolmus":  mustUser(t, s, ns, "dolmus", false),
	}
	for label, u := range people {
		g := Grant{By: &owner.ID}
		switch label {
		case "canli":
			g.Until = &future
		case "dolmus":
			g.Until = &past
		}
		if err := s.AddMember(ctx, site, u.ID, RoleViewer, g); err != nil {
			t.Fatalf("AddMember(%s): %v", label, err)
		}
	}

	members, err := s.Members(ctx, site)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	byID := map[int64]Member{}
	for _, m := range members {
		byID[m.UserID] = m
	}

	for label, u := range people {
		m, listed := byID[u.ID]
		if !listed {
			t.Errorf("%s: the members page does not list them at all", label)
			continue
		}

		access, err := s.AccessFor(ctx, principalOf(u), site)
		if err != nil {
			t.Fatalf("AccessFor(%s): %v", label, err)
		}
		sites, err := s.Sites(ctx, principalOf(u), nil)
		if err != nil {
			t.Fatalf("Sites(%s): %v", label, err)
		}
		inList := false
		for _, sa := range sites {
			if sa.SiteID == site {
				inList = true
			}
		}

		// One fact, asserted from three directions.
		if m.Expired == access.Member {
			t.Errorf("%s: the page says expired=%v and the door says member=%v. "+
				"The dangerous half of this is expired=true with member=true: a person "+
				"the owner believes is out, who is not", label, m.Expired, access.Member)
		}
		if m.Expired == inList {
			t.Errorf("%s: the page says expired=%v and their own site list %s the site",
				label, m.Expired, map[bool]string{true: "contains", false: "does not contain"}[inList])
		}
	}
}

// The other half, and it is the one that would go unnoticed: a grant
// with no end date behaves exactly as every grant did before this column
// existed.
func TestStore_RealDB_AGrantWithNoEndIsUnchanged(t *testing.T) {
	ns := "panel-noend"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns

	u := mustUser(t, s, ns, "member", false)
	if err := s.AddMember(ctx, site, u.ID, RoleAdmin, Grant{}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	access, err := s.AccessFor(ctx, principalOf(u), site)
	if err != nil {
		t.Fatalf("AccessFor: %v", err)
	}
	if access.Role != RoleAdmin || !access.Member {
		t.Errorf("role = %q member = %v, want an ordinary admin", access.Role, access.Member)
	}
	members, err := s.Members(ctx, site)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 1 || members[0].Expires != nil || members[0].Expired {
		t.Errorf("a grant with no end date came back as %+v", members)
	}
}

// Giving access again has to restart the clock. Carrying the old date
// forward would hand somebody an access that was already dead.
func TestStore_RealDB_ReGrantingRestartsTheClock(t *testing.T) {
	ns := "panel-regrantclock"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns

	owner := mustUser(t, s, ns, "owner", false)
	guest := mustUser(t, s, ns, "guest", false)
	if err := s.AddMember(ctx, site, owner.ID, RoleOwner, Grant{}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	if err := s.AddMember(ctx, site, guest.ID, RoleViewer, Grant{By: &owner.ID, Until: &past}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	// A fresh window.
	future := time.Now().Add(48 * time.Hour)
	if err := s.AddMember(ctx, site, guest.ID, RoleViewer, Grant{By: &owner.ID, Until: &future}); err != nil {
		t.Fatalf("re-granting: %v", err)
	}
	if access, err := s.AccessFor(ctx, principalOf(guest), site); err != nil {
		t.Fatal(err)
	} else if access.Role != RoleViewer {
		t.Errorf("role = %q after being given access again; the old end date survived the new grant", access.Role)
	}

	// And back to no end at all.
	if err := s.AddMember(ctx, site, guest.ID, RoleViewer, Grant{By: &owner.ID}); err != nil {
		t.Fatalf("re-granting without an end: %v", err)
	}
	members, err := s.Members(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.UserID == guest.ID && m.Expires != nil {
			t.Errorf("a grant with no end date left the old one in place: %v", m.Expires)
		}
	}
}

// An ownership may never be temporary, on any path.
//
// A site whose only owner expires cannot be repaired from the panel: the
// last-owner rule protects against removing them and demoting them, and
// this is the same loss arriving on a timer.
func TestStore_RealDB_AnOwnershipCannotBeTemporary(t *testing.T) {
	ns := "panel-tempowner"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns

	owner := mustUser(t, s, ns, "owner", false)
	guest := mustUser(t, s, ns, "guest", false)
	if err := s.AddMember(ctx, site, owner.ID, RoleOwner, Grant{}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	future := time.Now().Add(24 * time.Hour)
	if err := s.AddMember(ctx, site, guest.ID, RoleOwner,
		Grant{By: &owner.ID, Until: &future}); !errors.Is(err, ErrOwnershipCannotExpire) {
		t.Errorf("a temporary ownership gave %v, want ErrOwnershipCannotExpire", err)
	}
	if _, _, err := s.CreateMemberInvite(ctx, site, "gecici-sahip-"+ns+"@example.com",
		RoleOwner, principalOf(owner), 0, 7); !errors.Is(err, ErrOwnershipCannotExpire) {
		t.Errorf("a temporary ownership invitation gave %v, want ErrOwnershipCannotExpire", err)
	}

	// The database refuses it too, with the Go check taken out of the
	// way. Both, because the Go check is the message and the constraint
	// is the guarantee - and four paths write this table.
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO panel_site_members (site_id, user_id, role, expires_at)
		VALUES ($1, $2, 'owner', $3)`, site, guest.ID, future); err == nil {
		t.Error("the database accepted an ownership with an end date")
	}

	// Promoting a temporary member to owner clears the end date rather
	// than failing: making somebody responsible for a site is deliberate,
	// and refusing it because they arrived on a temporary grant would
	// make them start over for nothing.
	soon := time.Now().Add(time.Hour)
	if err := s.AddMember(ctx, site, guest.ID, RoleViewer, Grant{By: &owner.ID, Until: &soon}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.SetMemberRole(ctx, site, guest.ID, RoleOwner, &owner.ID); err != nil {
		t.Fatalf("promoting a temporary member to owner: %v", err)
	}
	members, err := s.Members(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.UserID == guest.ID && m.Expires != nil {
			t.Errorf("a promoted owner kept an end date of %v", m.Expires)
		}
	}
}

// The actor's own membership is read through the same filter, so an
// administrator whose access has run out cannot act on anybody - and
// cannot have an invitation of theirs redeemed either.
func TestStore_RealDB_AnExpiredAdminCannotActOrGrant(t *testing.T) {
	ns := "panel-expiredadmin"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns

	owner := mustUser(t, s, ns, "owner", false)
	admin := mustUser(t, s, ns, "admin", false)
	target := mustUser(t, s, ns, "target", false)
	if err := s.AddMember(ctx, site, owner.ID, RoleOwner, Grant{}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.AddMember(ctx, site, target.ID, RoleViewer, Grant{}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	// While their access is live, this is allowed.
	future := time.Now().Add(time.Hour)
	if err := s.AddMember(ctx, site, admin.ID, RoleAdmin, Grant{By: &owner.ID, Until: &future}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.SetMemberRole(ctx, site, target.ID, RoleAdmin, &admin.ID); err != nil {
		t.Fatalf("a live temporary admin acting: %v", err)
	}

	// Once it has run out, the same call is refused - with nothing having
	// changed except the clock.
	past := time.Now().Add(-time.Minute)
	if err := s.AddMember(ctx, site, admin.ID, RoleAdmin, Grant{By: &owner.ID, Until: &past}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.SetMemberRole(ctx, site, target.ID, RoleViewer, &admin.ID); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("an expired admin acting gave %v, want ErrNotPermitted", err)
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
		if err := s.AddMember(ctx, site, u.ID, RoleOwner, Grant{}); err != nil {
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
			errs[i] = s.RemoveMember(context.Background(), site, u.ID, nil)
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
	if err := s.AddMember(ctx, site, viewer.ID, RoleViewer, Grant{}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.AddMember(ctx, site, owner.ID, RoleOwner, Grant{}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.AddMember(ctx, site, admin.ID, RoleAdmin, Grant{}); err != nil {
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
	if err := s.AddMember(ctx, site, u.ID, RoleViewer, Grant{}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.AddMember(ctx, site, u.ID, RoleAdmin, Grant{}); err != nil {
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
	if err := s.AddMember(ctx, mine, me.ID, RoleOwner, Grant{}); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	if err := s.AddMember(ctx, theirs, them.ID, RoleOwner, Grant{}); err != nil {
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
	if err := s.RemoveMember(ctx, "site-"+ns, noSuchUser, nil); !errors.Is(err, ErrNotFound) {
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

// TestStore_RealDB_AnOversizedReasonIsRefusedNotTrimmed.
//
// The reason is free text typed at a shell and, from C5 onwards,
// rendered into the page an owner decides on. The column is TEXT, so
// nothing bounded it until something read it out loud.
//
// Refused rather than truncated, and the difference is the whole test:
// a sentence cut off mid-word is one an owner might decide differently
// on, and silently shortening the text a decision is made from trades
// their judgement for our convenience. The person who typed it is at a
// shell and can retype it.
func TestStore_RealDB_AnOversizedReasonIsRefusedNotTrimmed(t *testing.T) {
	ns := "panel-devreason"
	s := newTestStore(t, ns)
	ctx := context.Background()

	// Multi-byte on purpose. A byte-counted limit would cut this at
	// roughly half the promised length, and would do it only for the
	// languages this panel was written for.
	long := strings.Repeat("ş", MaxReasonRunes+1)
	if _, _, err := s.RequestDevAccess(ctx, long, time.Hour, time.Hour); !errors.Is(err, ErrReasonTooLong) {
		t.Fatalf("err = %v, want ErrReasonTooLong", err)
	}

	// And exactly at the limit is accepted, so the boundary is where it
	// says it is rather than one short of it.
	atLimit := strings.Repeat("ş", MaxReasonRunes)
	_, req, err := s.RequestDevAccess(ctx, atLimit, time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("a reason of exactly %d runes was refused: %v", MaxReasonRunes, err)
	}
	if utf8.RuneCountInString(req.Reason) != MaxReasonRunes {
		t.Errorf("the stored reason is %d runes, not the %d that were sent",
			utf8.RuneCountInString(req.Reason), MaxReasonRunes)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM panel_dev_access WHERE id = $1`, req.ID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}
