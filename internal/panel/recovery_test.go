package panel

import (
	"strings"
	"testing"
)

// TestRecoveryCodesAreDrawnFromTheAlphabetAndAreDistinct.
//
// Two properties, and the second is the one worth a test: a generator
// that returned the same code twice would look fine on the page - eight
// codes, all present - and give somebody one escape instead of eight.
func TestRecoveryCodesAreDrawnFromTheAlphabetAndAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	const draws = 500
	for range draws {
		code, err := newRecoveryCode()
		if err != nil {
			t.Fatalf("newRecoveryCode: %v", err)
		}
		if len(code) != recoveryCodeChars {
			t.Fatalf("code %q is %d characters, want %d", code, len(code), recoveryCodeChars)
		}
		for _, r := range code {
			if !strings.ContainsRune(recoveryAlphabet, r) {
				t.Fatalf("code %q contains %q, which is not in the alphabet", code, r)
			}
		}
		if seen[code] {
			t.Fatalf("newRecoveryCode returned %q twice in %d draws", code, draws)
		}
		seen[code] = true
	}
}

// TestNormalizeRecoveryCodeAcceptsWhatAPersonWouldType.
//
// The code is read off a screenshot, a password manager or a piece of
// paper and typed back. Every entry below is a way somebody reasonably
// types the same code, and refusing any of them is a support call this
// product does not need.
func TestNormalizeRecoveryCodeAcceptsWhatAPersonWouldType(t *testing.T) {
	const want = "ABCDEFGH2345"
	for name, typed := range map[string]string{
		"as issued":        "ABCD-EFGH-2345",
		"lower case":       "abcd-efgh-2345",
		"no dashes":        "ABCDEFGH2345",
		"spaces instead":   "ABCD EFGH 2345",
		"leading trailing": "  ABCD-EFGH-2345  ",
	} {
		t.Run(name, func(t *testing.T) {
			if got := NormalizeRecoveryCode(typed); got != want {
				t.Errorf("NormalizeRecoveryCode(%q) = %q, want %q", typed, got, want)
			}
		})
	}

	// The two digits that are not in the alphabet at all, and are what a
	// person writes when they read O and I off handwriting. Mapping them
	// cannot collide with a real character, because 0 and 1 can never
	// appear in a code.
	if got := NormalizeRecoveryCode("0BCD-EFGH-2345"); got != "OBCDEFGH2345" {
		t.Errorf("a typed zero was not read as O: %q", got)
	}
	if got := NormalizeRecoveryCode("1BCD-EFGH-2345"); got != "IBCDEFGH2345" {
		t.Errorf("a typed one was not read as I: %q", got)
	}
}

// TestFormatAndNormalizeRoundTrip. The grouping is presentation, so it
// has to survive being typed back exactly.
func TestFormatAndNormalizeRoundTrip(t *testing.T) {
	for range 100 {
		code, err := newRecoveryCode()
		if err != nil {
			t.Fatal(err)
		}
		shown := FormatRecoveryCode(code)
		if !strings.Contains(shown, "-") {
			t.Fatalf("FormatRecoveryCode(%q) = %q, which is not grouped", code, shown)
		}
		if got := NormalizeRecoveryCode(shown); got != code {
			t.Fatalf("a code shown as %q came back as %q, want %q", shown, got, code)
		}
	}
}

// TestNormalizeRecoveryCodeRefusesNothing. Empty in, empty out - the
// caller decides what to do about it, and UseRecoveryCode refuses an
// empty code before it touches the database.
func TestNormalizeRecoveryCodeRefusesNothing(t *testing.T) {
	for _, typed := range []string{"", "   ", "----", "!!!"} {
		if got := NormalizeRecoveryCode(typed); got != "" {
			t.Errorf("NormalizeRecoveryCode(%q) = %q, want empty", typed, got)
		}
	}
}
