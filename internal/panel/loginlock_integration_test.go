//go:build integration

// The sign-in lock and the owner's way of lifting it (catalogue #25),
// against a real database.
//
// The owner's decision, 2026-09-28: a lock lifts by itself within fifteen
// minutes, the budget is one of three, four or five (five was chosen, see
// maxFailuresPerEmail), and a site's owner may lift a member's lock
// sooner.

package panel

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// failedAgo writes one failed attempt at a chosen age, through the
// schema's owner: the panel role cannot choose the time of a row, and a
// window cannot be tested from one side of its edge.
func failedAgo(t *testing.T, admin *pgxpool.Pool, email string, ago time.Duration) {
	t.Helper()
	if _, err := admin.Exec(context.Background(), `
		INSERT INTO panel_login_attempts (email, ip, success, at)
		VALUES ($1, '203.0.113.40', false, now() - make_interval(secs => $2))`,
		NormalizeEmail(email), ago.Seconds()); err != nil {
		t.Fatalf("writing a failure %v ago: %v", ago, err)
	}
}

func failTimes(t *testing.T, s *Store, email string, n int) {
	t.Helper()
	for range n {
		if err := s.RecordLoginAttempt(context.Background(), email,
			netip.MustParseAddr("203.0.113.41"), false); err != nil {
			t.Fatalf("RecordLoginAttempt: %v", err)
		}
	}
}

func blocked(t *testing.T, s *Store, email string) bool {
	t.Helper()
	// An address nobody else uses, so only the account counter can fire.
	th, err := s.CheckLoginThrottle(context.Background(), email, netip.MustParseAddr("198.51.100.200"))
	if err != nil {
		t.Fatalf("CheckLoginThrottle: %v", err)
	}
	return th.Blocked
}

func unlockEntries(t *testing.T, s *Store, email string) int {
	t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM panel_audit_log WHERE action = $1 AND target = $2`,
		ActionLoginUnlocked, NormalizeEmail(email)).Scan(&n); err != nil {
		t.Fatalf("counting unlock entries: %v", err)
	}
	return n
}

// TestTheBudgetIsFiveFailuresInFifteenMinutes pins the owner's numbers
// with numbers.
//
// Written as literals, not as the constants: a test whose expectation
// comes from the value under test passes whatever the value is. Both
// edges of both limits - four is not a lock and five is; a failure just
// inside fifteen minutes counts and one just outside does not.
func TestTheBudgetIsFiveFailuresInFifteenMinutes(t *testing.T) {
	ns := "panel-butce"
	s := newTestStore(t, ns)
	admin := testdb.Admin(t)
	email := "hak-" + ns + "@example.com"

	failTimes(t, s, email, 4)
	if blocked(t, s, email) {
		t.Fatal("four failures locked the account; the budget is five")
	}
	failTimes(t, s, email, 1)
	if !blocked(t, s, email) {
		t.Fatal("five failures did not lock the account")
	}

	window := "pencere-" + ns + "@example.com"
	failTimes(t, s, window, 4)
	failedAgo(t, admin, window, 15*time.Minute+30*time.Second)
	if blocked(t, s, window) {
		t.Error("a failure fifteen and a half minutes old still counts; the window is fifteen minutes")
	}
	failedAgo(t, admin, window, 14*time.Minute+30*time.Second)
	if !blocked(t, s, window) {
		t.Error("a failure fourteen and a half minutes old did not count; the window is fifteen minutes")
	}
}

// TestALockLiftsWhenItsFifthNewestFailureLeavesTheWindow is the "when"
// the members page shows, read against rows whose ages are chosen.
//
// Six failures: the oldest, twelve minutes ago, is not the one that
// decides - the lock holds while five remain, so it lifts when the fifth
// newest (ten minutes ago) leaves the window: in five minutes, not three.
func TestALockLiftsWhenItsFifthNewestFailureLeavesTheWindow(t *testing.T) {
	ns := "panel-kalkis"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns
	member := mustUser(t, s, ns, "uye", false)
	if err := s.AddMember(ctx, site, member.ID, RoleViewer, Grant{}); err != nil {
		t.Fatal(err)
	}
	admin := testdb.Admin(t)
	for _, ago := range []time.Duration{12, 10, 8, 6, 2, 1} {
		failedAgo(t, admin, member.Email, ago*time.Minute)
	}

	locks, err := s.LoginLocks(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	l, ok := locks[member.ID]
	if !ok {
		t.Fatalf("a member with six failures in the window is not listed as locked: %+v", locks)
	}
	if l.Failures != 6 || l.Email != member.Email || l.Lifted {
		t.Errorf("lock = %+v, want six failures, the member's address, not lifted", l)
	}
	if l.LiftsIn < 4*time.Minute+50*time.Second || l.LiftsIn > 5*time.Minute+10*time.Second {
		t.Errorf("the lock lifts in %v; the fifth newest failure is ten minutes old, so five minutes", l.LiftsIn)
	}
}

// TestAnOwnerLiftsAMembersLock: the whole of what a lift does.
func TestAnOwnerLiftsAMembersLock(t *testing.T) {
	ns := "panel-kaldir"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns

	owner := mustUser(t, s, ns, "sahip", false)
	member := mustUser(t, s, ns, "uye", false)
	if err := s.AddMember(ctx, site, owner.ID, RoleOwner, Grant{}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, site, member.ID, RoleAdmin, Grant{By: &owner.ID}); err != nil {
		t.Fatal(err)
	}
	// One failure long out of the window, to show a lift forgets what
	// counts and nothing more: the table is also a record of who tried.
	failedAgo(t, testdb.Admin(t), member.Email, 40*time.Minute)
	failTimes(t, s, member.Email, 5)
	if !blocked(t, s, member.Email) {
		t.Fatal("the member is not locked; nothing below would mean anything")
	}

	done, err := s.UnlockLogin(ctx, site, principalOf(owner), member.ID)
	if err != nil {
		t.Fatalf("the owner could not lift a member's lock: %v", err)
	}
	if done.Email != member.Email || done.Failures != 5 {
		t.Errorf("lift = %+v, want the member's address and five failures", done)
	}
	if blocked(t, s, member.Email) {
		t.Error("the member is still locked after the lift")
	}
	var left int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM panel_login_attempts WHERE email = $1 AND NOT success`,
		member.Email).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 1 {
		t.Errorf("%d failures left after the lift, want the one outside the window", left)
	}
	locks, err := s.LoginLocks(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := locks[member.ID]; ok {
		t.Error("the members page would still show the lock")
	}

	entries, _, err := s.Audit(ctx, AuditFilter{SiteID: site, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range entries {
		if e.Action != ActionLoginUnlocked {
			continue
		}
		found = true
		if e.Target != member.Email || e.ActorKind != PrincipalUser || e.ActorLabel != owner.Email ||
			e.ActorID == nil || *e.ActorID != owner.ID {
			t.Errorf("entry = %+v; it should say which owner lifted whose lock", e)
		}
		if e.Detail["failures"] != float64(5) || e.Detail["user_id"] != float64(member.ID) {
			t.Errorf("detail = %v, want five failures and the member's id", e.Detail)
		}
	}
	if !found {
		t.Error("the lift left no audit entry")
	}
}

// TestOnlyAnOwnerOfTheSiteMayLift: every refusal, and after all of them
// the lock still holds and no entry was written.
//
// The superadmin and the developer are refused on purpose. Putting this
// behind the developer password was the alternative the owner declined.
func TestOnlyAnOwnerOfTheSiteMayLift(t *testing.T) {
	ns := "panel-kaldiramaz"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site, elsewhere := "site-"+ns, "baska-"+ns

	owner := mustUser(t, s, ns, "sahip", false)
	admin := mustUser(t, s, ns, "yonetici", false)
	viewer := mustUser(t, s, ns, "izleyici", false)
	staff := mustUser(t, s, ns, "isletmeci", true)
	stranger := mustUser(t, s, ns, "baskasi", false)
	member := mustUser(t, s, ns, "uye", false)
	for _, m := range []struct {
		site string
		u    User
		role Role
	}{
		{site, owner, RoleOwner}, {site, admin, RoleAdmin}, {site, viewer, RoleViewer},
		{site, member, RoleViewer}, {elsewhere, stranger, RoleOwner},
	} {
		if err := s.AddMember(ctx, m.site, m.u.ID, m.role, Grant{}); err != nil {
			t.Fatal(err)
		}
	}
	failTimes(t, s, member.Email, 5)
	failTimes(t, s, owner.Email, 5)

	cases := []struct {
		name  string
		actor Principal
		site  string
		user  int64
		want  error
	}{
		{"an administrator", principalOf(admin), site, member.ID, ErrMayNotLift},
		{"a viewer", principalOf(viewer), site, member.ID, ErrMayNotLift},
		{"a superadmin with no membership", principalOf(staff), site, member.ID, ErrMayNotLift},
		{"a developer session", developerPrincipal(), site, member.ID, ErrMayNotLift},
		{"the owner of another site, asking about this one", principalOf(stranger), site, member.ID, ErrMayNotLift},
		{"the owner of another site, about their own site", principalOf(stranger), elsewhere, member.ID, ErrNotFound},
		{"the owner, on their own lock", principalOf(owner), site, owner.ID, ErrMayNotLift},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.UnlockLogin(ctx, tc.site, tc.actor, tc.user); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if !blocked(t, s, member.Email) || !blocked(t, s, owner.Email) {
		t.Error("a refused lift lifted a lock anyway")
	}
	if n := unlockEntries(t, s, member.Email) + unlockEntries(t, s, owner.Email); n != 0 {
		t.Errorf("%d unlock entries written by refused lifts", n)
	}

	// And the rows those refusals were decided against: the owner may.
	if _, err := s.UnlockLogin(ctx, site, principalOf(owner), member.ID); err != nil {
		t.Errorf("the owner was refused too: %v; then the refusals above prove nothing", err)
	}
}

// TestAnExpiredMemberIsNotAnOwnersToLift: an expired membership belongs to
// nothing, so the owner of that site has no business with the account.
func TestAnExpiredMemberIsNotAnOwnersToLift(t *testing.T) {
	ns := "panel-bitmis"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns
	owner := mustUser(t, s, ns, "sahip", false)
	gone := mustUser(t, s, ns, "eski", false)
	if err := s.AddMember(ctx, site, owner.ID, RoleOwner, Grant{}); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Hour)
	if err := s.AddMember(ctx, site, gone.ID, RoleViewer, Grant{By: &owner.ID, Until: &past}); err != nil {
		t.Fatal(err)
	}
	failTimes(t, s, gone.Email, 5)

	locks, err := s.LoginLocks(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := locks[gone.ID]; ok {
		t.Error("the page would offer to lift the lock of somebody whose access has ended")
	}
	if _, err := s.UnlockLogin(ctx, site, principalOf(owner), gone.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if !blocked(t, s, gone.Email) {
		t.Error("the lock was lifted anyway")
	}
}

// TestALiftNeedsALock: four failures are not a lock, and a lift that
// found none must not quietly reset them - that would be a way to give
// somebody a fresh budget before they had used this one.
func TestALiftNeedsALock(t *testing.T) {
	ns := "panel-kilitsiz"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns
	owner := mustUser(t, s, ns, "sahip", false)
	member := mustUser(t, s, ns, "uye", false)
	for _, u := range []User{owner, member} {
		role := RoleViewer
		if u.ID == owner.ID {
			role = RoleOwner
		}
		if err := s.AddMember(ctx, site, u.ID, role, Grant{}); err != nil {
			t.Fatal(err)
		}
	}
	failTimes(t, s, member.Email, 4)

	// Four is not shown either: a count below the budget is nothing
	// anybody can act on.
	locks, err := s.LoginLocks(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	if l, ok := locks[member.ID]; ok {
		t.Errorf("four failures are listed as a lock: %+v", l)
	}
	if _, err := s.UnlockLogin(ctx, site, principalOf(owner), member.ID); !errors.Is(err, ErrNotLocked) {
		t.Fatalf("err = %v, want ErrNotLocked", err)
	}
	failTimes(t, s, member.Email, 1)
	if !blocked(t, s, member.Email) {
		t.Error("the four failures were forgotten by a lift that found no lock")
	}
	if n := unlockEntries(t, s, member.Email); n != 0 {
		t.Errorf("%d unlock entries for a lift that did nothing", n)
	}
}

// TestALockIsLiftedOncePerWindow: the second lift inside fifteen minutes
// is refused - for every owner, because the rule is about the account -
// and allowed again once the first one is older than the window.
func TestALockIsLiftedOncePerWindow(t *testing.T) {
	ns := "panel-birkez"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns
	first := mustUser(t, s, ns, "sahip1", false)
	second := mustUser(t, s, ns, "sahip2", false)
	member := mustUser(t, s, ns, "uye", false)
	for _, u := range []User{first, second} {
		if err := s.AddMember(ctx, site, u.ID, RoleOwner, Grant{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddMember(ctx, site, member.ID, RoleViewer, Grant{}); err != nil {
		t.Fatal(err)
	}

	failTimes(t, s, member.Email, 5)
	if _, err := s.UnlockLogin(ctx, site, principalOf(first), member.ID); err != nil {
		t.Fatal(err)
	}
	failTimes(t, s, member.Email, 5)

	locks, err := s.LoginLocks(ctx, site)
	if err != nil {
		t.Fatal(err)
	}
	if l := locks[member.ID]; !l.Lifted {
		t.Errorf("lock = %+v; the page would offer a second lift the store refuses", l)
	}
	for _, owner := range []User{first, second} {
		if _, err := s.UnlockLogin(ctx, site, principalOf(owner), member.ID); !errors.Is(err, ErrLiftedRecently) {
			t.Errorf("a second lift by %s gave %v, want ErrLiftedRecently", owner.Email, err)
		}
	}
	if !blocked(t, s, member.Email) {
		t.Fatal("the second lift went through")
	}

	// The first lift, moved to just outside the window.
	if _, err := testdb.Admin(t).Exec(ctx, `
		UPDATE panel_audit_log SET time = now() - interval '15 minutes 30 seconds'
		 WHERE action = $1 AND target = $2`, ActionLoginUnlocked, member.Email); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UnlockLogin(ctx, site, principalOf(second), member.ID); err != nil {
		t.Errorf("a lift was refused with the previous one outside the window: %v", err)
	}
}

// TestTwoOwnersLiftingAtOnceLiftOnce: the account row is locked before
// anything is read, so the second of two simultaneous lifts reads the
// first one's result. Twenty rounds, because one round of a race proves
// little either way.
func TestTwoOwnersLiftingAtOnceLiftOnce(t *testing.T) {
	ns := "panel-yaris"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns
	owners := []User{mustUser(t, s, ns, "sahip1", false), mustUser(t, s, ns, "sahip2", false)}
	member := mustUser(t, s, ns, "uye", false)
	for _, u := range owners {
		if err := s.AddMember(ctx, site, u.ID, RoleOwner, Grant{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddMember(ctx, site, member.ID, RoleViewer, Grant{}); err != nil {
		t.Fatal(err)
	}

	admin := testdb.Admin(t)
	for round := range 20 {
		failTimes(t, s, member.Email, 5)
		var wg sync.WaitGroup
		errs := make([]error, len(owners))
		start := make(chan struct{})
		for i, owner := range owners {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = s.UnlockLogin(context.Background(), site, principalOf(owner), member.ID)
			}()
		}
		close(start)
		wg.Wait()

		lifted := 0
		for _, err := range errs {
			switch {
			case err == nil:
				lifted++
			case errors.Is(err, ErrNotLocked), errors.Is(err, ErrLiftedRecently):
			default:
				t.Fatalf("round %d: unexpected error %v", round, err)
			}
		}
		if lifted != 1 {
			t.Fatalf("round %d: %d of two simultaneous lifts went through, want exactly one", round, lifted)
		}
		if n := unlockEntries(t, s, member.Email); n != round+1 {
			t.Fatalf("round %d: %d unlock entries, want %d - one per lift", round, n, round+1)
		}
		// Out of the window, so the next round may lift again.
		if _, err := admin.Exec(ctx, `
			UPDATE panel_audit_log SET time = now() - interval '1 hour'
			 WHERE action = $1 AND target = $2`, ActionLoginUnlocked, member.Email); err != nil {
			t.Fatal(err)
		}
	}
}

// TestALiftThatCannotBeRecordedDoesNotHappen: the audit entry is written
// inside the lift's own transaction, so a lift whose record is refused is
// no lift at all. The record is refused here by an actor label PostgreSQL
// will not store (a NUL byte); what matters is that the refusal comes from
// the audit INSERT, the last statement, after the failures were deleted.
func TestALiftThatCannotBeRecordedDoesNotHappen(t *testing.T) {
	ns := "panel-kayitsiz"
	s := newTestStore(t, ns)
	ctx := context.Background()
	site := "site-" + ns
	owner := mustUser(t, s, ns, "sahip", false)
	member := mustUser(t, s, ns, "uye", false)
	if err := s.AddMember(ctx, site, owner.ID, RoleOwner, Grant{}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, site, member.ID, RoleViewer, Grant{}); err != nil {
		t.Fatal(err)
	}
	failTimes(t, s, member.Email, 5)

	actor := principalOf(owner)
	actor.Label = "sahip\x00" + ns
	_, err := s.UnlockLogin(ctx, site, actor, member.ID)
	if err == nil {
		t.Fatal("a lift whose audit entry could not be written reported success")
	}
	if !strings.Contains(err.Error(), "record audit entry") {
		t.Fatalf("the lift failed somewhere else (%v); this test is about the last statement", err)
	}
	if !blocked(t, s, member.Email) {
		t.Error("the lock was lifted with no record of who lifted it")
	}
	if n := unlockEntries(t, s, member.Email); n != 0 {
		t.Errorf("%d unlock entries", n)
	}
}
