package panel

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/cruciblelab/crucible-analytic/internal/argon2id"
)

const goodPassword = "dogru-parola-12345"

func TestHashPassword_RoundTrips(t *testing.T) {
	hash, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$") {
		t.Errorf("hash is not a PHC argon2id string: %q", hash)
	}
	if strings.Contains(hash, goodPassword) {
		t.Fatal("the plaintext password appears in the stored hash")
	}

	ok, needsRehash := VerifyPassword(hash, goodPassword)
	if !ok {
		t.Error("the correct password did not verify")
	}
	if needsRehash {
		t.Error("a hash made with the current parameters wants rehashing")
	}
}

func TestVerifyPassword_RejectsTheWrongPassword(t *testing.T) {
	hash, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	for _, wrong := range []string{"", "dogru-parola-1234", "dogru-parola-123456", strings.ToUpper(goodPassword)} {
		if ok, _ := VerifyPassword(hash, wrong); ok {
			t.Errorf("%q verified against a hash of %q", wrong, goodPassword)
		}
	}
}

func TestHashPassword_SaltsEachHash(t *testing.T) {
	first, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	second, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if first == second {
		t.Fatal("the same password hashed to the same string twice - the salt is not random, so one leak would reveal every account using that password")
	}
	// Both must still verify: a random salt is only useful if it
	// round-trips through the encoding.
	if ok, _ := VerifyPassword(second, goodPassword); !ok {
		t.Error("the second hash did not verify")
	}
}

func TestValidatePassword_BoundsAreCountedInRunes(t *testing.T) {
	// 12 Turkish characters: 12 runes but more than 12 bytes. Counting
	// bytes would accept this while rejecting a 12-character ASCII
	// password, which is exactly backwards.
	if err := ValidatePassword("şşşşşşşşşşşş"); err != nil {
		t.Errorf("a 12-rune password was rejected: %v", err)
	}
	if err := ValidatePassword("şşşşşşşşşşş"); err == nil {
		t.Error("an 11-rune password was accepted")
	}
	if err := ValidatePassword(strings.Repeat("a", MaxPasswordLen+1)); err == nil {
		t.Error("an over-long password was accepted; argon2 would hash the whole thing")
	}
	if err := ValidatePassword(strings.Repeat("a", MaxPasswordLen)); err != nil {
		t.Errorf("a password at exactly the maximum was rejected: %v", err)
	}
}

func TestHashPassword_RefusesAnInvalidPassword(t *testing.T) {
	if _, err := HashPassword("kisa"); err == nil {
		t.Error("HashPassword accepted a password ValidatePassword rejects")
	}
}

func TestVerifyPassword_MalformedHashesFailClosed(t *testing.T) {
	cases := map[string]string{
		"empty":           "",
		"not phc":         "hunter2",
		"wrong algorithm": "$argon2i$v=19$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"wrong version":   "$argon2id$v=16$m=19456,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"missing fields":  "$argon2id$v=19$m=19456,t=2,p=1$c2FsdA",
		"bad base64":      "$argon2id$v=19$m=19456,t=2,p=1$!!!!$!!!!",
		"short salt":      "$argon2id$v=19$m=19456,t=2,p=1$YQ$aGFzaGhhc2hoYXNoaGFzaA",
		"nonsense params": "$argon2id$v=19$m=abc,t=def,p=ghi$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
	}
	for name, hash := range cases {
		t.Run(name, func(t *testing.T) {
			// Must not panic, and must not verify.
			if ok, _ := VerifyPassword(hash, goodPassword); ok {
				t.Errorf("a malformed hash verified: %q", hash)
			}
		})
	}
}

// A hash is data read back out of the database. A corrupted or hostile
// row claiming m=16777216 would make one verification try to allocate
// 16 GiB - a single row turning into a whole-machine outage.
func TestVerifyPassword_RefusesAbsurdCostParameters(t *testing.T) {
	cases := map[string]string{
		"16 GiB of memory": "$argon2id$v=19$m=16777216,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"huge time cost":   "$argon2id$v=19$m=19456,t=99999,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"zero memory":      "$argon2id$v=19$m=0,t=2,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
		"zero time":        "$argon2id$v=19$m=19456,t=0,p=1$c2FsdHNhbHRzYWx0$aGFzaGhhc2hoYXNoaGFzaA",
	}
	for name, hash := range cases {
		t.Run(name, func(t *testing.T) {
			done := make(chan struct{})
			go func() {
				defer close(done)
				if ok, _ := VerifyPassword(hash, goodPassword); ok {
					t.Error("an absurdly-parameterized hash verified")
				}
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("verification did not return promptly; the parameters were not bounded before reaching argon2")
			}
		})
	}
}

// hashWith builds a real, verifiable argon2id hash using the given cost
// parameters, so the rehash test exercises the actual comparison path
// rather than a hand-written string that could never have matched.
func hashWith(password string, memory, time uint32, threads uint8) string {
	salt := []byte("saltsaltsaltsalt")
	key := argon2.IDKey([]byte(password), salt, time, memory, threads, argon2id.KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memory, time, threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

func TestVerifyPassword_ReportsWeakerParametersForRehash(t *testing.T) {
	// A hash made with the parameters an older release might have used.
	// It must still verify - people should not be locked out by an
	// upgrade - but it must ask to be replaced.
	weak := hashWith(goodPassword, 8192, 1, 1)

	ok, needsRehash := VerifyPassword(weak, goodPassword)
	if !ok {
		t.Fatal("a hash with older parameters did not verify; an upgrade would lock everyone out")
	}
	if !needsRehash {
		t.Error("weaker parameters were not reported for rehashing")
	}
}

// VerifyDummy must cost what a real verification costs, or the login
// form becomes an oracle for which email addresses are registered.
func TestVerifyDummy_CostsTheSameAsARealVerification(t *testing.T) {
	hash, err := HashPassword(goodPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}

	start := time.Now()
	VerifyPassword(hash, "wrong-password-here")
	real := time.Since(start)

	start = time.Now()
	VerifyDummy("wrong-password-here")
	dummy := time.Since(start)

	// Generous bounds: this asserts the dummy path does the same order
	// of work, not that two argon2 runs take identical wall time on a
	// shared CI machine.
	if dummy < real/4 {
		t.Errorf("VerifyDummy took %v against a real %v - far too fast to be doing the same work, so response time reveals whether an account exists", dummy, real)
	}
}
