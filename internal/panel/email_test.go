package panel

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// UsableEmail and loginKey, at their edges (PLAN §H8).
//
// The pair carries one promise: for every address an account can have,
// the key the sign-in records use is the address itself. So every case
// asks both - whether the address is usable, and whether loginKey left
// it alone - and the two answers must agree. A case where they disagree
// is an account whose failures the throttle counts under one key and the
// members page reads under another.
func TestAnAddressIsUsableExactlyWhenTheSignInKeepsItWhole(t *testing.T) {
	at := func(n int) string { return strings.Repeat("a", n-len("@ornek.test")) + "@ornek.test" }
	for _, c := range []struct {
		name   string
		email  string
		usable bool
	}{
		{"ordinary", "ahmet@ornek.test", true},
		{"case and space are normalisation, not damage", "  Ahmet@Ornek.Test ", true},
		{"exactly the bound", at(MaxEmailBytes), true},
		{"one byte over", at(MaxEmailBytes + 1), false},
		{"64 four-byte runes: 256 bytes", strings.Repeat("𠀀", 64), false},
		{"63 four-byte runes: 252 bytes", strings.Repeat("𠀀", 63), true},
		{"NUL", "ahmet\x00@ornek.test", false},
		{"a control character", "ahmet\x01@ornek.test", false},
		{"a line separator", "ahmet @ornek.test", false},
		{"empty", "", false},
		{"only space", "   ", false},
	} {
		got := UsableEmail(c.email)
		if got != c.usable {
			t.Errorf("%s: UsableEmail = %v, want %v", c.name, got, c.usable)
		}
		n := NormalizeEmail(c.email)
		kept := n != "" && loginKey(c.email) == n
		if kept != got {
			t.Errorf("%s: UsableEmail says %v and loginKey kept the address: %v. The two "+
				"must agree, or an account's failures live under two keys", c.name, got, kept)
		}
	}
}

// Whatever was typed, the key is something a text column and its index
// accept: valid, free of control characters, inside the bound.
func TestALoginKeyIsAlwaysStorable(t *testing.T) {
	for _, typed := range []string{
		strings.Repeat("𠀀", 700) + "@ornek.test",
		"bozuk\xff\xfe@ornek.test",
		"nul\x00\x00@ornek.test",
		strings.Repeat("ğ", 200),
		"",
	} {
		key := loginKey(typed)
		if len(key) > MaxEmailBytes {
			t.Errorf("loginKey(%.20q...) is %d bytes, past %d", typed, len(key), MaxEmailBytes)
		}
		control := strings.IndexFunc(key, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0
		if !utf8.ValidString(key) || control {
			t.Errorf("loginKey(%.20q...) = %q, which a text column refuses", typed, key)
		}
	}
}
