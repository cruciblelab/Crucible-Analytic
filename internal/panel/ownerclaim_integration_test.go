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

// TestOwnerClaimIsSingleUseUnderConcurrency is the test this whole
// design exists to pass.
//
// An invitation creates an owner. Two of them create two, and the second
// is an account nobody meant to exist with full authority over the
// customer's data. The window is not hypothetical: a link arrives in a
// message, somebody taps it, the page is slow, they tap again - and now
// two requests are inside the handler at the same instant with the same
// token.
//
// Check-then-act loses this every time. What wins is the consuming
// UPDATE carrying `used_at IS NULL` in its WHERE clause, so the database
// decides rather than the order the requests happened to arrive in.
func TestOwnerClaimIsSingleUseUnderConcurrency(t *testing.T) {
	store := newTestStore(t, "claim")
	ctx := context.Background()

	hash, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	token, claim, err := store.CreateOwnerClaim(ctx,
		"yaris-claim@example.invalid", "Yarış", Principal{Label: "test"}, 0)
	if err != nil {
		t.Fatalf("CreateOwnerClaim: %v", err)
	}
	t.Cleanup(func() {
		pool := store.Pool()
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM panel_site_members WHERE site_id = $1`, "claim-yaris")
		_, _ = pool.Exec(bg, `DELETE FROM panel_owner_claims WHERE id = $1`, claim.ID)
		_, _ = pool.Exec(bg, `DELETE FROM panel_users WHERE email LIKE $1`, "%claim@example.invalid")
	})

	const attempts = 8
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		users    []User
		refusals int
		other    []error
	)
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Released together, so the attempts genuinely overlap
			// rather than queueing behind each other's setup.
			<-start
			user, err := store.RedeemOwnerClaim(ctx, token, hash,
				[]string{"claim-yaris"}, netip.MustParseAddr("203.0.113.9"))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				users = append(users, user)
			case errors.Is(err, ErrClaimInvalid):
				refusals++
			default:
				other = append(other, err)
			}
		}()
	}
	close(start)
	wg.Wait()

	for _, err := range other {
		t.Errorf("unexpected error: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("%d accounts were created from one invitation; it must produce exactly one", len(users))
	}
	if refusals != attempts-1 {
		t.Errorf("%d refusals, want %d - every loser must be told no rather than failing some other way",
			refusals, attempts-1)
	}

	// And the winner really is an owner of the site, which is the half
	// of the transaction that would be easy to lose.
	access, err := store.AccessFor(ctx, Principal{Kind: PrincipalUser, UserID: users[0].ID}, "claim-yaris")
	if err != nil {
		t.Fatal(err)
	}
	if access.Role != RoleOwner {
		t.Fatalf("the new account's role is %q, want owner", access.Role)
	}
	if users[0].IsSuperadmin {
		t.Error("accepting an invitation produced a superadmin; owning a site and running the deployment are different jobs")
	}
}

// TestOwnerClaimRefusesEveryDeadLinkTheSameWay: unknown, expired and
// already-used must be indistinguishable, or the difference is an oracle
// for guessing tokens.
func TestOwnerClaimRefusesEveryDeadLinkTheSameWay(t *testing.T) {
	store := newTestStore(t, "claim")
	ctx := context.Background()
	hash, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatal(err)
	}

	// Already used.
	usedToken, usedClaim, err := store.CreateOwnerClaim(ctx,
		"kullanilmis-claim@example.invalid", "", Principal{Label: "test"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	user, err := store.RedeemOwnerClaim(ctx, usedToken, hash, nil, netip.Addr{})
	if err != nil {
		t.Fatal(err)
	}

	// Expired: minted with a lifetime already in the past.
	expiredToken, expiredClaim, err := store.CreateOwnerClaim(ctx,
		"suresi-dolmus-claim@example.invalid", "", Principal{Label: "test"}, -time.Minute)
	if err != nil {
		// A negative ttl is normalised to the default, so expire it by
		// hand instead - which is also what a real expiry looks like.
		t.Fatal(err)
	}
	if _, err := store.Pool().Exec(ctx,
		`UPDATE panel_owner_claims SET expires_at = now() - interval '1 hour' WHERE id = $1`,
		expiredClaim.ID); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		pool := store.Pool()
		bg := context.Background()
		_, _ = pool.Exec(bg, `DELETE FROM panel_owner_claims WHERE id = ANY($1)`,
			[]int64{usedClaim.ID, expiredClaim.ID})
		_, _ = pool.Exec(bg, `DELETE FROM panel_users WHERE id = $1`, user.ID)
	})

	for _, tc := range []struct {
		name  string
		token string
	}{
		{"never existed", "bu-token-hic-var-olmadi"},
		{"already used", usedToken},
		{"expired", expiredToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.LookupOwnerClaim(ctx, tc.token); !errors.Is(err, ErrClaimInvalid) {
				t.Errorf("LookupOwnerClaim = %v, want ErrClaimInvalid", err)
			}
			_, err := store.RedeemOwnerClaim(ctx, tc.token, hash, nil, netip.Addr{})
			if !errors.Is(err, ErrClaimInvalid) {
				t.Errorf("RedeemOwnerClaim = %v, want ErrClaimInvalid", err)
			}
			// Same error value, therefore the same message: nothing
			// downstream can tell these apart to render them differently.
			if err != nil && !strings.Contains(err.Error(), ErrClaimInvalid.Error()) {
				t.Errorf("the message differs from the others: %q", err)
			}
		})
	}
}

// TestOwnerClaimRefusesAnAddressThatAlreadyHasAnAccount catches the
// mistake at the point where somebody can still fix it.
//
// The unique index would refuse it anyway - at redemption, after the
// link has been handed over and the person who minted it has gone.
func TestOwnerClaimRefusesAnAddressThatAlreadyHasAnAccount(t *testing.T) {
	store := newTestStore(t, "claim")
	ctx := context.Background()
	existing := mustUser(t, store, "claim", "mevcut", false)

	_, _, err := store.CreateOwnerClaim(ctx, existing.Email, "", Principal{Label: "test"}, 0)
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("CreateOwnerClaim = %v, want ErrEmailTaken", err)
	}
}

// TestOwnerClaimStoresNoUsableToken. The table is readable by the panel
// role and by anybody with the database; what is in it must not be
// enough to use.
func TestOwnerClaimStoresNoUsableToken(t *testing.T) {
	store := newTestStore(t, "claim")
	ctx := context.Background()

	token, claim, err := store.CreateOwnerClaim(ctx,
		"hash-claim@example.invalid", "", Principal{Label: "test"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = store.Pool().Exec(context.Background(),
			`DELETE FROM panel_owner_claims WHERE id = $1`, claim.ID)
	})

	var stored string
	if err := store.Pool().QueryRow(ctx,
		`SELECT sha256 FROM panel_owner_claims WHERE id = $1`, claim.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == token {
		t.Fatal("the raw token is in the table")
	}
	if strings.Contains(stored, token) {
		t.Fatal("the stored value contains the raw token")
	}
	if len(stored) != 64 {
		t.Errorf("stored value is %d characters, want a 64-character hex SHA-256", len(stored))
	}
}
