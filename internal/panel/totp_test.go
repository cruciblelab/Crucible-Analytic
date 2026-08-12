package panel

import (
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

// codeAt generates the code an authenticator app would show at a given
// moment, so tests can drive the clock instead of waiting on it.
func codeAt(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{
		Period: totpPeriod, Skew: 0, Digits: totpDigits, Algorithm: totpAlgo,
	})
	if err != nil {
		t.Fatalf("GenerateCodeCustom: %v", err)
	}
	return code
}

func newSecret(t *testing.T) string {
	t.Helper()
	key, err := NewTOTPSecret("ahmet@example.com")
	if err != nil {
		t.Fatalf("NewTOTPSecret: %v", err)
	}
	if key.Secret() == "" {
		t.Fatal("generated an empty secret")
	}
	return key.Secret()
}

func TestNewTOTPSecret_ProducesAScannableKey(t *testing.T) {
	key, err := NewTOTPSecret("ahmet@example.com")
	if err != nil {
		t.Fatalf("NewTOTPSecret: %v", err)
	}
	// The otpauth URL is what the QR code encodes; an app that cannot
	// read it is a two-factor feature nobody turns on.
	url := key.URL()
	for _, want := range []string{"otpauth://totp/", "secret=", "issuer="} {
		if !contains(url, want) {
			t.Errorf("otpauth URL %q is missing %q", url, want)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestCheckTOTP_AcceptsTheCurrentCode(t *testing.T) {
	secret := newSecret(t)
	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

	step, ok := checkTOTP(secret, codeAt(t, secret, now), now)
	if !ok {
		t.Fatal("the current code did not verify")
	}
	if want := now.Unix() / totpPeriod; step != want {
		t.Errorf("step = %d, want %d", step, want)
	}
}

// Phone clocks drift, and a code typed at the very end of its window
// arrives in the next one. Both neighbours are accepted for that reason
// - which is also exactly why replay protection is needed.
func TestCheckTOTP_ToleratesOneStepOfDrift(t *testing.T) {
	secret := newSecret(t)
	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)

	for _, offset := range []time.Duration{-totpPeriod * time.Second, totpPeriod * time.Second} {
		at := now.Add(offset)
		if _, ok := checkTOTP(secret, codeAt(t, secret, at), now); !ok {
			t.Errorf("a code from %v away was rejected", offset)
		}
	}

	// Two steps away is too far: at that point it is a stale code, not
	// a clock that is slightly off.
	for _, offset := range []time.Duration{-2 * totpPeriod * time.Second, 2 * totpPeriod * time.Second} {
		at := now.Add(offset)
		if _, ok := checkTOTP(secret, codeAt(t, secret, at), now); ok {
			t.Errorf("a code from %v away was accepted", offset)
		}
	}
}

// The step is what makes replay protection possible: a helper that only
// answered yes or no could not provide it.
func TestCheckTOTP_ReportsWhichStepMatched(t *testing.T) {
	secret := newSecret(t)
	now := time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
	earlier := now.Add(-totpPeriod * time.Second)

	step, ok := checkTOTP(secret, codeAt(t, secret, earlier), now)
	if !ok {
		t.Fatal("the previous step's code did not verify")
	}
	if want := earlier.Unix() / totpPeriod; step != want {
		t.Errorf("step = %d, want the earlier step %d", step, want)
	}
}

func TestCheckTOTP_RejectsRubbish(t *testing.T) {
	secret := newSecret(t)
	now := time.Now()

	cases := map[string]struct{ secret, code string }{
		"empty code":     {secret, ""},
		"empty secret":   {"", codeAt(t, secret, now)},
		"wrong digits":   {secret, "000000"},
		"not a number":   {secret, "abcdef"},
		"too short":      {secret, "123"},
		"invalid secret": {"not-base32!!", "123456"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := checkTOTP(tc.secret, tc.code, now); ok {
				t.Errorf("checkTOTP accepted %+v", tc)
			}
		})
	}
}

func TestCheckTOTP_IgnoresSurroundingWhitespace(t *testing.T) {
	// Copy-pasting from an authenticator app routinely brings a space
	// along; rejecting that reads as "wrong code" to the user.
	secret := newSecret(t)
	now := time.Now()
	if _, ok := checkTOTP(secret, "  "+codeAt(t, secret, now)+" ", now); !ok {
		t.Error("a code with surrounding whitespace was rejected")
	}
}

func TestCheckTOTP_DifferentSecretsDoNotShareCodes(t *testing.T) {
	first, second := newSecret(t), newSecret(t)
	now := time.Now()
	if _, ok := checkTOTP(second, codeAt(t, first, now), now); ok {
		t.Error("a code generated from one secret verified against another")
	}
}
