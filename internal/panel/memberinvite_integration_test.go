//go:build integration

package panel

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

// The member invitation, against a real database.
//
// Four claims, and each one is a way this could be wrong rather than a
// way it could be right:
//
//   - redeeming it twice makes one member, not two;
//   - the invitee cannot change what they were invited to;
//   - an inviter who has lost their authority cannot still grant it;
//   - the token is not recoverable from the table that holds it.

const inviteSite = "davet-testi"

// inviteFixture makes an inviter with a role on inviteSite and cleans up
// after itself.
func inviteFixture(t *testing.T, store *Store, local string, role Role, superadmin bool) User {
	t.Helper()
	ctx := context.Background()

	hash, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.CreateUser(ctx, local+"@davet.invalid", local, hash, superadmin)
	if err != nil {
		t.Fatalf("CreateUser(%s): %v", local, err)
	}
	if role != "" {
		if err := store.AddMember(ctx, inviteSite, user.ID, role, Grant{}); err != nil {
			t.Fatalf("AddMember(%s): %v", local, err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		pool := store.Pool()
		_, _ = pool.Exec(bg, `DELETE FROM panel_member_invites WHERE site_id = $1`, inviteSite)
		_, _ = pool.Exec(bg, `DELETE FROM panel_site_members WHERE site_id = $1`, inviteSite)
		_, _ = pool.Exec(bg, `DELETE FROM panel_users WHERE email LIKE $1`, "%@davet.invalid")
	})
	return user
}

// TestMemberInviteIsSingleUseUnderConcurrency.
//
// The same window an owner claim has: a link arrives in a message,
// somebody taps it, the page is slow, they tap again. Check-then-act
// loses this; the consuming UPDATE carrying `used_at IS NULL` wins,
// because the database decides rather than the arrival order.
func TestMemberInviteIsSingleUseUnderConcurrency(t *testing.T) {
	store := newTestStore(t, "davet")
	ctx := context.Background()
	owner := inviteFixture(t, store, "yaris-sahip", RoleOwner, false)

	hash, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.CreateMemberInvite(ctx, inviteSite, "yaris-davetli@davet.invalid",
		RoleViewer, Principal{UserID: owner.ID, Label: owner.Email}, 0, 0)
	if err != nil {
		t.Fatalf("CreateMemberInvite: %v", err)
	}

	const attempts = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		users    []User
		refusals int
		other    []error
	)
	start := make(chan struct{})
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			got, err := store.RedeemMemberInvite(context.Background(), token,
				"Yarış", hash, netip.MustParseAddr("198.51.100.7"))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				users = append(users, got.User)
			case errors.Is(err, ErrInviteInvalid):
				refusals++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(users) != 1 {
		t.Errorf("%d of %d redemptions succeeded; exactly one must.\n"+
			"A second one is a member nobody invited", len(users), attempts)
	}
	if refusals != attempts-len(users) {
		t.Errorf("%d refusals for %d losing attempts; the rest failed some other way: %v",
			refusals, attempts-len(users), other)
	}

	// And exactly one membership row, at the invited role.
	var members int
	var role string
	if err := store.Pool().QueryRow(ctx, `
		SELECT count(*), coalesce(max(role), '') FROM panel_site_members WHERE site_id = $1
		  AND user_id <> $2`, inviteSite, owner.ID).Scan(&members, &role); err != nil {
		t.Fatal(err)
	}
	if members != 1 || role != string(RoleViewer) {
		t.Errorf("the site has %d invited member(s) at role %q; want 1 viewer", members, role)
	}
}

// TestMemberInviteGrantsWhatItSaysAndNothingTheInviteeChose.
//
// The invitee sends a password and a display name. Everything that
// decides what they get - the site, the role, the address - is read from
// the row. This asserts the row wins by asking for a role the invitee
// never had a way to request and checking they did not get more.
func TestMemberInviteGrantsWhatItSaysAndNothingTheInviteeChose(t *testing.T) {
	store := newTestStore(t, "davet")
	ctx := context.Background()
	owner := inviteFixture(t, store, "veren-sahip", RoleOwner, false)

	hash, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	token, invite, err := store.CreateMemberInvite(ctx, inviteSite, "Alinan@Davet.invalid",
		RoleViewer, Principal{UserID: owner.ID, Label: owner.Email}, 0, 0)
	if err != nil {
		t.Fatalf("CreateMemberInvite: %v", err)
	}
	// The address is normalised on the way in, so the invitation is for
	// one address rather than for however it happened to be typed.
	if invite.Email != "alinan@davet.invalid" {
		t.Errorf("the invitation is for %q; addresses are normalised before they are stored", invite.Email)
	}

	got, err := store.RedeemMemberInvite(ctx, token, "Alınan", hash,
		netip.MustParseAddr("198.51.100.8"))
	if err != nil {
		t.Fatalf("RedeemMemberInvite: %v", err)
	}
	user, used := got.User, got.Invite
	if !got.Created {
		t.Error("the redemption reports it did not create an account, and the address had none")
	}
	if user.Email != invite.Email {
		t.Errorf("the account was created as %q for an invitation to %q", user.Email, invite.Email)
	}
	if user.IsSuperadmin {
		t.Error("accepting an invitation made a superadmin. Owning a site and running the " +
			"deployment are different jobs, and only a shell grants the second")
	}
	if used.Role != RoleViewer {
		t.Errorf("the invitation was redeemed at role %q, not the %q it was minted for",
			used.Role, RoleViewer)
	}

	access, err := store.AccessFor(ctx, Principal{UserID: user.ID, Kind: PrincipalUser}, inviteSite)
	if err != nil {
		t.Fatal(err)
	}
	if access.Role != RoleViewer {
		t.Errorf("the invited member holds %q on the site; the invitation said %q",
			access.Role, RoleViewer)
	}
	if access.Can(CapManageMembers) {
		t.Error("a viewer invited as a viewer may manage members")
	}
}

// TestAnInviterWhoLostTheAuthorityCannotStillGrantIt.
//
// The hole a first draft leaves. The role is checked when the invitation
// is minted, which is true then; between then and redemption the inviter
// may be demoted. An invitation that still grants what they could no
// longer grant is a privilege escalation that outlives the demotion that
// was supposed to end it.
//
// Two halves, and the first is the one that matters: the refusal happens
// even when nobody remembered to withdraw the invitation.
func TestAnInviterWhoLostTheAuthorityCannotStillGrantIt(t *testing.T) {
	store := newTestStore(t, "davet")
	ctx := context.Background()
	admin := inviteFixture(t, store, "dusen-admin", RoleAdmin, false)

	hash, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatal(err)
	}

	// An admin may invite a viewer. Not an owner - CanAssign refuses that
	// at minting - so the case under test is a role they legitimately had
	// and then lost.
	token, invite, err := store.CreateMemberInvite(ctx, inviteSite, "dusen-davetli@davet.invalid",
		RoleViewer, Principal{UserID: admin.ID, Label: admin.Email}, 0, 0)
	if err != nil {
		t.Fatalf("CreateMemberInvite: %v", err)
	}

	// Demoted to viewer, which cannot manage members at all.
	if err := store.SetMemberRole(ctx, inviteSite, admin.ID, RoleViewer, nil); err != nil {
		t.Fatalf("SetMemberRole: %v", err)
	}

	_, err = store.RedeemMemberInvite(ctx, token, "Düşen", hash,
		netip.MustParseAddr("198.51.100.9"))
	if !errors.Is(err, ErrInviteInvalid) {
		t.Fatalf("redeeming an invitation from a demoted inviter returned %v; want ErrInviteInvalid.\n"+
			"Otherwise a demotion does not end what the demoted person had already set in motion", err)
	}

	// And the invitation was not consumed by the refusal: restoring the
	// role restores the link, rather than leaving the invitee holding
	// something that can never work again.
	if err := store.SetMemberRole(ctx, inviteSite, admin.ID, RoleAdmin, nil); err != nil {
		t.Fatalf("SetMemberRole back: %v", err)
	}
	if _, err := store.RedeemMemberInvite(ctx, token, "Düşen", hash,
		netip.MustParseAddr("198.51.100.9")); err != nil {
		t.Errorf("the invitation stayed dead after the inviter's role came back: %v.\n"+
			"A refusal must roll back with the transaction, not spend the link", err)
	}
	_ = invite
}

// The clock starts when somebody accepts, not when the link was minted.
//
// That is the whole reason the row carries a number of days rather than
// a date: whoever invites a contractor for a month means a month of
// work, and an absolute date would quietly spend part of it while the
// message sat in an inbox.
func TestAnInvitationsClockStartsAtAcceptance(t *testing.T) {
	store := newTestStore(t, "davet")
	ctx := context.Background()
	owner := inviteFixture(t, store, "sureli-sahip", RoleOwner, false)

	hash, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	const days = 30
	token, invite, err := store.CreateMemberInvite(ctx, inviteSite, "sureli@davet.invalid",
		RoleViewer, Principal{UserID: owner.ID, Label: owner.Email}, 0, days)
	if err != nil {
		t.Fatalf("CreateMemberInvite: %v", err)
	}
	if invite.GrantDays != days {
		t.Errorf("the invitation carries %d days, want %d", invite.GrantDays, days)
	}

	// Minted, then a pause, then accepted. The pause is what an absolute
	// date would have spent.
	minted := time.Now()
	time.Sleep(20 * time.Millisecond)
	got, err := store.RedeemMemberInvite(ctx, token, "Süreli", hash,
		netip.MustParseAddr("198.51.100.12"))
	if err != nil {
		t.Fatalf("RedeemMemberInvite: %v", err)
	}

	members, err := store.Members(ctx, inviteSite)
	if err != nil {
		t.Fatal(err)
	}
	var expires *time.Time
	for _, m := range members {
		if m.UserID == got.User.ID {
			expires = m.Expires
		}
	}
	if expires == nil {
		t.Fatal("the membership has no end date; the invitation's days were not applied")
	}
	// Counted from acceptance: the end is later than minting plus the
	// window, by at least the pause.
	if !expires.After(minted.Add(days * 24 * time.Hour)) {
		t.Errorf("the membership ends at %v, which is no later than %d days after minting - "+
			"the clock started at the wrong end", expires, days)
	}
	// And it is the window, not something else entirely.
	if expires.After(time.Now().Add((days + 1) * 24 * time.Hour)) {
		t.Errorf("the membership ends at %v, more than %d days out", expires, days)
	}

	// The invited person can actually see the site, which is the point.
	access, err := store.AccessFor(ctx, Principal{UserID: got.User.ID}, inviteSite)
	if err != nil {
		t.Fatal(err)
	}
	if access.Role != RoleViewer {
		t.Errorf("the temporarily invited member holds %q, want viewer", access.Role)
	}
}

// An invitation gives access. It is not a way to take any away.
//
// The route this closes was real and short: an administrator invites the
// site's owner as a viewer, and then opens the link themselves - it is
// printed on their own screen, which is the whole point of C7.3's rule
// that the link is always shown. Redemption's upsert wrote the
// invitation's role over the membership it found, and the site was left
// with no owner at all. Measured before it was fixed.
//
// The fix is DO NOTHING, so the assertion here is about the membership
// after the redemption rather than about an error: redeeming succeeds,
// the account is found rather than created, and the role does not move.
func TestARedeemedInvitationNeverLowersAMembershipItFinds(t *testing.T) {
	store := newTestStore(t, "davet")
	ctx := context.Background()
	owner := inviteFixture(t, store, "hedef-sahip", RoleOwner, false)
	admin := inviteFixture(t, store, "davetci-admin", RoleAdmin, false)

	hash, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatal(err)
	}

	token, _, err := store.CreateMemberInvite(ctx, inviteSite, owner.Email, RoleViewer,
		Principal{UserID: admin.ID, Label: admin.Email}, 0, 0)
	if err != nil {
		t.Fatalf("CreateMemberInvite: %v", err)
	}
	got, err := store.RedeemMemberInvite(ctx, token, "Sahip", hash,
		netip.MustParseAddr("198.51.100.11"))
	if err != nil {
		t.Fatalf("RedeemMemberInvite: %v", err)
	}
	if got.Created {
		t.Errorf("the address already had an account; Created = true would mean a second one")
	}

	access, err := store.AccessFor(ctx, Principal{UserID: owner.ID}, inviteSite)
	if err != nil {
		t.Fatalf("AccessFor: %v", err)
	}
	if access.Role != RoleOwner {
		t.Errorf("the owner's role is %q after somebody redeemed a viewer invitation aimed at them, want owner", access.Role)
	}
}

// TestMemberInviteStoresNoUsableToken.
//
// The rule every secret in this schema follows: whoever can read the
// table cannot thereby use what is in it.
func TestMemberInviteStoresNoUsableToken(t *testing.T) {
	store := newTestStore(t, "davet")
	ctx := context.Background()
	owner := inviteFixture(t, store, "jeton-sahip", RoleOwner, false)

	token, invite, err := store.CreateMemberInvite(ctx, inviteSite, "jeton@davet.invalid",
		RoleViewer, Principal{UserID: owner.ID, Label: owner.Email}, 0, 0)
	if err != nil {
		t.Fatalf("CreateMemberInvite: %v", err)
	}

	var stored string
	if err := store.Pool().QueryRow(ctx,
		`SELECT sha256 FROM panel_member_invites WHERE id = $1`, invite.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == token {
		t.Fatal("the raw token is in the table. A database dump is then a set of working invitations")
	}
	if strings.Contains(stored, token) || strings.Contains(token, stored) {
		t.Fatal("the stored value contains the token")
	}
	if len(stored) != 64 {
		t.Errorf("the stored value is %d characters; a SHA-256 hex digest is 64", len(stored))
	}
}

// TestWithdrawingAnInvitationEndsIt, and TestAnExpiredInvitationIsRefused.
//
// Both directions of "not open", and both refused the same way: an
// invitee learns that a link does not work, never which kind of not.
func TestAnInvitationThatIsNoLongerOpenIsRefused(t *testing.T) {
	store := newTestStore(t, "davet")
	ctx := context.Background()
	owner := inviteFixture(t, store, "kapali-sahip", RoleOwner, false)
	by := Principal{UserID: owner.ID, Label: owner.Email}

	hash, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatal(err)
	}

	withdrawn, invite, err := store.CreateMemberInvite(ctx, inviteSite,
		"geri-alinan@davet.invalid", RoleViewer, by, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeMemberInvite(ctx, invite.ID, inviteSite); err != nil {
		t.Fatalf("RevokeMemberInvite: %v", err)
	}

	expired, _, err := store.CreateMemberInvite(ctx, inviteSite,
		"suresi-dolan@davet.invalid", RoleViewer, by, time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)

	for name, token := range map[string]string{
		"withdrawn": withdrawn,
		"expired":   expired,
		"unknown":   "hicbir-zaman-var-olmadi",
	} {
		if _, err := store.LookupMemberInvite(ctx, token); !errors.Is(err, ErrInviteInvalid) {
			t.Errorf("looking up the %s invitation returned %v; want ErrInviteInvalid", name, err)
		}
		if _, err := store.RedeemMemberInvite(ctx, token, "Kapalı", hash,
			netip.MustParseAddr("198.51.100.10")); !errors.Is(err, ErrInviteInvalid) {
			t.Errorf("redeeming the %s invitation returned %v; want ErrInviteInvalid", name, err)
		}
	}

	// A withdrawn invitation is still a row, because "this was withdrawn
	// on that date" is a fact and a vanished row cannot state it.
	var revoked *time.Time
	if err := store.Pool().QueryRow(ctx,
		`SELECT revoked_at FROM panel_member_invites WHERE id = $1`, invite.ID).Scan(&revoked); err != nil {
		t.Fatalf("the withdrawn invitation is gone from the table: %v", err)
	}
	if revoked == nil {
		t.Error("the withdrawn invitation has no revoked_at")
	}
}

// TestOpenInvitationsAreListedAndDemotionWithdrawsThem.
func TestOpenInvitationsAreListedAndDemotionWithdrawsThem(t *testing.T) {
	store := newTestStore(t, "davet")
	ctx := context.Background()
	owner := inviteFixture(t, store, "liste-sahip", RoleOwner, false)
	by := Principal{UserID: owner.ID, Label: owner.Email}

	for _, address := range []string{"liste-bir@davet.invalid", "liste-iki@davet.invalid"} {
		if _, _, err := store.CreateMemberInvite(ctx, inviteSite, address, RoleViewer, by, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	open, err := store.OpenMemberInvites(ctx, inviteSite)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Fatalf("the site lists %d open invitations; two were minted", len(open))
	}

	n, err := store.RevokeMemberInvitesBy(ctx, owner.ID, inviteSite)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("withdrawing one person's invitations touched %d rows; want 2", n)
	}
	open, err = store.OpenMemberInvites(ctx, inviteSite)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("%d invitations are still listed as open after being withdrawn", len(open))
	}
}
