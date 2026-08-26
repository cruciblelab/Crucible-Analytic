//go:build integration

package panel

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

// recoveryStore is a store whose cleanup will find what these tests
// create.
//
// The namespace matters and is not decoration: newTestStore deletes
// accounts whose address contains it, and mustUser is what puts it
// there. The first version of this file minted its own addresses under a
// domain of its own, so nothing swept them up - CreateUser refuses a
// duplicate, and the second run of the suite failed on every test with
// eight accounts left in the table.
//
// That is the third time this week a test has only passed against a
// database its own last run had not touched, and it is exactly what the
// CI gate's second integration pass exists to catch.
const recoveryNS = "panel-kurtarma"

func recoveryStore(t *testing.T) *Store {
	t.Helper()
	return newTestStore(t, recoveryNS)
}

// recoveryUser creates an account with a set of codes and returns both.
func recoveryUser(t *testing.T, s *Store, local string) (User, []string) {
	t.Helper()
	user := mustUser(t, s, recoveryNS, local, false)
	codes, err := s.GenerateRecoveryCodes(context.Background(), user.ID, 0)
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	if len(codes) != RecoveryCodeCount {
		t.Fatalf("got %d codes, want %d", len(codes), RecoveryCodeCount)
	}
	return user, codes
}

// TestRecovery_ACodeWorksOnceAndSetsThePassword is the whole feature in
// one test: the owner gets back in, and the code they used does not let
// anybody else do it again.
func TestRecovery_ACodeWorksOnceAndSetsThePassword(t *testing.T) {
	s := recoveryStore(t)
	ctx := context.Background()
	user, codes := recoveryUser(t, s, "kurtarma-bir")

	newHash, err := HashPassword("yepyeni-bir-parola-123")
	if err != nil {
		t.Fatal(err)
	}
	from := netip.MustParseAddr("203.0.113.7")

	result, err := s.UseRecoveryCode(ctx, user.Email, codes[0], newHash, false, from)
	if err != nil {
		t.Fatalf("UseRecoveryCode: %v", err)
	}
	if result.User.ID != user.ID {
		t.Errorf("the code resolved to user %d, want %d", result.User.ID, user.ID)
	}
	if result.Remaining != RecoveryCodeCount-1 {
		t.Errorf("%d codes remain, want %d", result.Remaining, RecoveryCodeCount-1)
	}

	// The password really changed.
	after, err := s.UserByID(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := VerifyPassword(after.PasswordHash, "yepyeni-bir-parola-123"); !ok {
		t.Error("the new password does not verify against the stored hash")
	}

	// And the code is spent.
	if _, err := s.UseRecoveryCode(ctx, user.Email, codes[0], newHash, false, from); err == nil {
		t.Error("a used recovery code worked a second time")
	}
}

// TestRecovery_TheCodeIsAcceptedHoweverItIsTyped. The store normalises,
// so a person reading off paper is not fighting the form.
func TestRecovery_TheCodeIsAcceptedHoweverItIsTyped(t *testing.T) {
	s := recoveryStore(t)
	ctx := context.Background()
	user, codes := recoveryUser(t, s, "kurtarma-yazim")

	hash, err := HashPassword("baska-bir-uzun-parola-1")
	if err != nil {
		t.Fatal(err)
	}
	typed := strings.ToLower(FormatRecoveryCode(codes[1]))
	if _, err := s.UseRecoveryCode(ctx, user.Email, typed, hash, false, netip.Addr{}); err != nil {
		t.Errorf("a code typed as %q was refused: %v", typed, err)
	}
}

// TestRecovery_TheAddressHasToMatchAndAWrongOneDoesNotBurnTheCode.
//
// Two properties in one test because they are the same mistake seen from
// two sides. Somebody holding a valid code for their own account must
// not be able to use it against a different address - and somebody who
// simply mistyped their own address must not lose the code for it.
func TestRecovery_TheAddressHasToMatchAndAWrongOneDoesNotBurnTheCode(t *testing.T) {
	s := recoveryStore(t)
	ctx := context.Background()
	user, codes := recoveryUser(t, s, "kurtarma-adres")
	other, _ := recoveryUser(t, s, "kurtarma-baskasi")

	hash, err := HashPassword("uzun-parola-deneme-99")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.UseRecoveryCode(ctx, other.Email, codes[0], hash, false, netip.Addr{}); err == nil {
		t.Fatal("a code was accepted against somebody else's address")
	}
	// The account it was aimed at is untouched.
	victim, err := s.UserByID(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := VerifyPassword(victim.PasswordHash, "uzun-parola-deneme-99"); ok {
		t.Fatal("the other account's password was changed by somebody else's code")
	}

	// And the real owner can still use it: the failed attempt rolled the
	// consumption back rather than spending it.
	if _, err := s.UseRecoveryCode(ctx, user.Email, codes[0], hash, false, netip.Addr{}); err != nil {
		t.Errorf("a mistyped address burned the code: %v", err)
	}
}

// TestRecovery_TheSecondFactorIsKeptUnlessAsked.
//
// The default matters more than the option. "I forgot my password" and
// "I lost my phone" arrive at the same form, and clearing the second
// factor on every reset would quietly downgrade every account that ever
// used one.
func TestRecovery_TheSecondFactorIsKeptUnlessAsked(t *testing.T) {
	s := recoveryStore(t)
	ctx := context.Background()
	user, codes := recoveryUser(t, s, "kurtarma-2fa")

	if err := s.SetTOTPSecret(ctx, user.ID, "JBSWY3DPEHPK3PXP"); err != nil {
		t.Fatalf("SetTOTPSecret: %v", err)
	}
	hash, err := HashPassword("iki-faktorlu-parola-77")
	if err != nil {
		t.Fatal(err)
	}

	result, err := s.UseRecoveryCode(ctx, user.Email, codes[0], hash, false, netip.Addr{})
	if err != nil {
		t.Fatalf("UseRecoveryCode: %v", err)
	}
	if result.SecondFactorCleared {
		t.Error("the result claims the second factor was cleared when it was not asked for")
	}
	if !result.User.HasTOTP() {
		t.Error("resetting a password removed the second factor nobody asked to remove")
	}

	// And when it is asked for, it goes.
	result, err = s.UseRecoveryCode(ctx, user.Email, codes[1], hash, true, netip.Addr{})
	if err != nil {
		t.Fatalf("UseRecoveryCode: %v", err)
	}
	if !result.SecondFactorCleared || result.User.HasTOTP() {
		t.Error("the second factor survived a reset that asked for it to be cleared")
	}
}

// TestRecovery_ADisabledAccountCannotBeRecovered, and answers the same
// way as an address that does not exist.
func TestRecovery_ADisabledAccountCannotBeRecovered(t *testing.T) {
	s := recoveryStore(t)
	ctx := context.Background()
	user, codes := recoveryUser(t, s, "kurtarma-kapali")

	if err := s.SetDisabled(ctx, user.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}
	hash, err := HashPassword("kapali-hesap-parolasi-1")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.UseRecoveryCode(ctx, user.Email, codes[0], hash, false, netip.Addr{})
	if err == nil {
		t.Fatal("a disabled account was recovered")
	}
	// The same error as an unknown address, so the refusal says nothing
	// about which of the two happened.
	if _, unknown := s.UseRecoveryCode(ctx, "yok@yok.invalid", codes[1], hash, false, netip.Addr{}); unknown.Error() != err.Error() {
		t.Errorf("a disabled account (%v) is distinguishable from an unknown address (%v)", err, unknown)
	}
}

// TestRecovery_GeneratingReplacesTheOldSet.
//
// Adding rather than replacing would mean a code somebody believes they
// revoked still opens the account, which is the opposite of what
// "generate new codes" reads as.
func TestRecovery_GeneratingReplacesTheOldSet(t *testing.T) {
	s := recoveryStore(t)
	ctx := context.Background()
	user, old := recoveryUser(t, s, "kurtarma-yenile")

	fresh, err := s.GenerateRecoveryCodes(ctx, user.ID, user.ID)
	if err != nil {
		t.Fatalf("GenerateRecoveryCodes: %v", err)
	}
	if left, err := s.CountRecoveryCodes(ctx, user.ID); err != nil {
		t.Fatal(err)
	} else if left != RecoveryCodeCount {
		t.Errorf("%d codes after regenerating, want %d - the old set was added to, not replaced",
			left, RecoveryCodeCount)
	}

	hash, err := HashPassword("yenilenmis-parola-123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseRecoveryCode(ctx, user.Email, old[0], hash, false, netip.Addr{}); err == nil {
		t.Error("a code from the replaced set still works")
	}
	if _, err := s.UseRecoveryCode(ctx, user.Email, fresh[0], hash, false, netip.Addr{}); err != nil {
		t.Errorf("a code from the new set does not work: %v", err)
	}
}

// TestRecovery_TwoTabsRedeemingOneCodeProduceOneReset.
//
// The same race the invitation flow guards, and the same answer: the
// code is consumed by an UPDATE that only matches an unused row, so the
// loser finds nothing and is refused rather than both being let in.
func TestRecovery_TwoTabsRedeemingOneCodeProduceOneReset(t *testing.T) {
	s := recoveryStore(t)
	ctx := context.Background()
	user, codes := recoveryUser(t, s, "kurtarma-yaris")

	hash, err := HashPassword("yaris-parolasi-abcdef")
	if err != nil {
		t.Fatal(err)
	}

	const attempts = 6
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		accepted int
		refused  int
		other    []error
	)
	start := make(chan struct{})
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := s.UseRecoveryCode(ctx, user.Email, codes[0], hash, false, netip.Addr{})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, ErrRecoveryInvalid):
				refused++
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
	if accepted != 1 {
		t.Errorf("%d of %d concurrent attempts on one code succeeded, want exactly 1",
			accepted, attempts)
	}
	if refused != attempts-1 {
		t.Errorf("%d attempts were refused, want %d", refused, attempts-1)
	}
}
