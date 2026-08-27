package sealed

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func testKey(t *testing.T) Key {
	t.Helper()
	var raw [KeySize]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	k, err := ParseKey(hex.EncodeToString(raw[:]))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestRoundTrip(t *testing.T) {
	k := testKey(t)

	for _, plaintext := range []string{
		"hunter2",
		"",
		"şifre içinde Türkçe var",
		strings.Repeat("uzun", 1000),
		"boşluk ve\nsatır sonu\tdahil",
	} {
		box, err := k.Seal("panel_smtp.password", plaintext)
		if err != nil {
			t.Fatalf("Seal(%q): %v", plaintext, err)
		}
		got, err := k.Open("panel_smtp.password", box)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if got != plaintext {
			t.Errorf("round trip gave %q, want %q", got, plaintext)
		}
	}
}

// The password must not be visible in what gets stored. Obvious, and
// worth an assertion anyway: a change that made Seal a no-op on some
// path would pass every round-trip test in this file.
func TestSealedValueDoesNotContainThePlaintext(t *testing.T) {
	k := testKey(t)
	const password = "TahminEdilemezBirSifre123"

	box, err := k.Seal("panel_smtp.password", password)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(box, password) {
		t.Fatalf("the sealed value contains the password: %s", box)
	}
	// And the base64 body decodes to something that is not the password
	// either - a check that catches "encrypted" meaning "base64 encoded",
	// which is the way this mistake is actually made.
	body := strings.TrimPrefix(box, prefix)
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), password) {
		t.Fatalf("the sealed bytes contain the password: %q", raw)
	}
}

// Two seals of the same plaintext must differ. Equal ciphertexts would
// mean a fixed nonce, and would tell anybody reading the table which
// accounts share a password.
func TestSealIsNotDeterministic(t *testing.T) {
	k := testKey(t)
	seen := make(map[string]bool)
	for range 50 {
		box, err := k.Seal("panel_smtp.password", "aynı şifre")
		if err != nil {
			t.Fatal(err)
		}
		if seen[box] {
			t.Fatalf("the same plaintext sealed to the same ciphertext twice: %s", box)
		}
		seen[box] = true
	}
}

// The label is the whole point of using additional authenticated data: a
// value moved between columns must not open.
func TestLabelBinding(t *testing.T) {
	k := testKey(t)

	box, err := k.Seal("panel_smtp.password", "gizli")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.Open("panel_api_tokens.secret", box); !errors.Is(err, ErrCannotOpen) {
		t.Errorf("opening under another label gave %v, want ErrCannotOpen", err)
	}
	if _, err := k.Open("", box); !errors.Is(err, ErrCannotOpen) {
		t.Errorf("opening under an empty label gave %v, want ErrCannotOpen", err)
	}
}

// A different key must not open it, and the failure must be the one the
// panel can explain - "the key in your config file changed" - rather than
// a panic or, far worse, silence and an empty password.
func TestAnotherKeyCannotOpen(t *testing.T) {
	a, b := testKey(t), testKey(t)

	box, err := a.Seal("panel_smtp.password", "gizli")
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Open("panel_smtp.password", box)
	if !errors.Is(err, ErrCannotOpen) {
		t.Errorf("err = %v, want ErrCannotOpen", err)
	}
	if got != "" {
		t.Errorf("a failed open returned %q; anything but empty here would be sent to a mail server", got)
	}
}

// Any edit to the stored value must fail rather than decrypt to
// something. Tested byte by byte over the whole ciphertext, because a
// construction that authenticated only part of it would pass a test that
// flipped one byte in a place it happened to cover.
func TestTamperingIsDetected(t *testing.T) {
	k := testKey(t)

	box, err := k.Seal("panel_smtp.password", "gizli")
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimPrefix(box, prefix)
	raw, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		t.Fatal(err)
	}

	for i := range raw {
		edited := append([]byte(nil), raw...)
		edited[i] ^= 0x01
		stored := prefix + base64.StdEncoding.EncodeToString(edited)
		if _, err := k.Open("panel_smtp.password", stored); err == nil {
			t.Fatalf("flipping a bit in byte %d of %d still opened", i, len(raw))
		}
	}
}

func TestOpenRejectsMalformedValues(t *testing.T) {
	k := testKey(t)
	tests := []struct {
		name   string
		stored string
		want   error
	}{
		{"empty", "", ErrFormat},
		// A plaintext password sitting in the column - what a row written
		// before this package existed, or by hand, would look like. It
		// must not come back as itself.
		{"bare plaintext", "hunter2", ErrFormat},
		{"no prefix", base64.StdEncoding.EncodeToString([]byte("whatever")), ErrFormat},
		{"prefix but not base64", prefix + "!!!not base64!!!", ErrFormat},
		{"prefix and valid base64 that is not a box", prefix + base64.StdEncoding.EncodeToString([]byte("short")), ErrCannotOpen},
		{"a future format", "v2." + base64.StdEncoding.EncodeToString([]byte("whatever")), ErrFormat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := k.Open("panel_smtp.password", tc.stored)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if got != "" {
				t.Errorf("returned %q on a failure", got)
			}
		})
	}
}

// A zero Key is what a deployment without mail has. It must refuse
// rather than encrypt under a key of zeroes, which would look exactly
// like encryption and protect nothing.
func TestZeroKeyRefuses(t *testing.T) {
	var k Key
	if k.IsSet() {
		t.Error("IsSet() is true for a zero Key")
	}
	if _, err := k.Seal("panel_smtp.password", "gizli"); !errors.Is(err, ErrNoKey) {
		t.Errorf("Seal err = %v, want ErrNoKey", err)
	}
	if _, err := k.Open("panel_smtp.password", "v1.whatever"); !errors.Is(err, ErrNoKey) {
		t.Errorf("Open err = %v, want ErrNoKey", err)
	}
}

func TestParseKey(t *testing.T) {
	raw := make([]byte, KeySize)
	for i := range raw {
		raw[i] = byte(i)
	}

	tests := []struct {
		name    string
		encoded string
		wantErr error
	}{
		{"hex", hex.EncodeToString(raw), nil},
		{"base64", base64.StdEncoding.EncodeToString(raw), nil},
		{"hex with surrounding space", "  " + hex.EncodeToString(raw) + "\n", nil},
		{"empty", "", ErrNoKey},
		{"only whitespace", "   ", ErrNoKey},
		{"too short", hex.EncodeToString(raw[:16]), ErrKeySize},
		{"base64 of the wrong length", base64.StdEncoding.EncodeToString(raw[:16]), ErrKeySize},
		{"not an encoding at all", "bu bir anahtar değil", nil}, // checked below
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k, err := ParseKey(tc.encoded)
			switch {
			case tc.name == "not an encoding at all":
				if err == nil {
					t.Error("a phrase was accepted as a key")
				}
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("err = %v, want %v", err, tc.wantErr)
				}
			default:
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if !k.IsSet() {
					t.Error("IsSet() is false for a parsed key")
				}
			}
		})
	}
}

// Both encodings of the same bytes must produce the same key - otherwise
// an operator who re-generated their key file in the other format would
// find every stored password unreadable and no explanation for it.
func TestHexAndBase64OfTheSameBytesAgree(t *testing.T) {
	var raw [KeySize]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	fromHex, err := ParseKey(hex.EncodeToString(raw[:]))
	if err != nil {
		t.Fatal(err)
	}
	fromBase64, err := ParseKey(base64.StdEncoding.EncodeToString(raw[:]))
	if err != nil {
		t.Fatal(err)
	}

	box, err := fromHex.Seal("panel_smtp.password", "gizli")
	if err != nil {
		t.Fatal(err)
	}
	got, err := fromBase64.Open("panel_smtp.password", box)
	if err != nil {
		t.Fatalf("a key given in base64 could not open what the same key in hex sealed: %v", err)
	}
	if got != "gizli" {
		t.Errorf("got %q", got)
	}
}

// TestTheTwoEncodingsCannotBeConfused pins the fact decodeKey's comment
// rests on, rather than the fall-through the first draft was written to
// avoid.
//
// The comment originally claimed a base64 key could be misread as hex.
// It cannot: base64 of 32 bytes is 44 characters ending in "=". This
// test is what established that, and it stays so the claim keeps being
// checked rather than being believed - if a future KeySize made the
// padding disappear, the two lengths would start to overlap and the
// decoder would need a real rule instead of a readable one.
func TestTheTwoEncodingsCannotBeConfused(t *testing.T) {
	var raw [KeySize]byte
	for range 2000 {
		if _, err := rand.Read(raw[:]); err != nil {
			t.Fatal(err)
		}
		encoded := base64.StdEncoding.EncodeToString(raw[:])
		if len(encoded) == hex.EncodedLen(KeySize) {
			t.Fatalf("base64 of a key is %d characters, the same as hex; the decoder needs a real rule", len(encoded))
		}
		if _, err := hex.DecodeString(encoded); err == nil {
			t.Fatalf("a base64 key decoded as hex: %s", encoded)
		}
		// And it parses, which is the property an operator cares about.
		if _, err := ParseKey(encoded); err != nil {
			t.Fatalf("base64 key %s was rejected: %v", encoded, err)
		}
	}
}
