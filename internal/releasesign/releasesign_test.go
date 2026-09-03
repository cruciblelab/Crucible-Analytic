package releasesign

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// newPair returns a signing key and its public half.
//
// testing.TB rather than *testing.T so the fuzz targets can use it. A
// second copy for *testing.F would be two places for the sanity check
// below to drift out of.
func newPair(t testing.TB) (PrivateKey, PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if !pub.Equal(priv.Public()) {
		t.Fatal("the generated halves do not match, so nothing below tests anything")
	}
	return PrivateKey{key: priv}, PublicKey{key: pub}
}

// A real SHA256SUMS is a list of lines. The exact content does not
// matter to the signature, but using the real shape keeps the failure
// messages readable when something breaks.
const sampleSums = `e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  ./bin/collector
5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03  ./bin/panel
`

func TestAPackageWeSignedVerifies(t *testing.T) {
	priv, pub := newPair(t)

	sig, err := priv.Sign([]byte(sampleSums))
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if len(sig) != SignatureSize {
		t.Errorf("the signature is %d bytes, not %d", len(sig), SignatureSize)
	}
	if err := pub.Verify([]byte(sampleSums), sig); err != nil {
		t.Fatalf("a package this key signed did not verify: %v", err)
	}
}

// TestEveryWayAPackageCanBeWrongIsRejected.
//
// The table is the point. Each row is a thing somebody can actually do
// to a package between our build and a customer's machine, and the one
// answer that must never appear is nil.
func TestEveryWayAPackageCanBeWrongIsRejected(t *testing.T) {
	priv, pub := newPair(t)
	good, err := priv.Sign([]byte(sampleSums))
	if err != nil {
		t.Fatal(err)
	}
	other, _ := newPair(t)
	otherSig, err := other.Sign([]byte(sampleSums))
	if err != nil {
		t.Fatal(err)
	}

	flip := func(b []byte, i int) []byte {
		out := append([]byte(nil), b...)
		out[i] ^= 0x01
		return out
	}

	cases := []struct {
		what string
		sums []byte
		sig  []byte
		// why says what the attacker achieved if this returned nil.
		why string
		// wantIn is a fragment the message must carry, for the rows
		// where "does not match" would send somebody hunting.
		//
		// A wrong-length signature is not a security case:
		// ed25519.Verify refuses one on its own, and a mutation that
		// deleted the explicit check passed every other assertion here.
		// It is a diagnosis case. A .sig file truncated by a broken
		// download and a package somebody edited produce the same
		// verdict, and only one of them is fixed by downloading again.
		wantIn string
	}{
		{
			what: "a line was edited",
			sums: []byte(strings.Replace(sampleSums, "e3b0", "0000", 1)),
			sig:  good,
			why:  "a swapped binary with the list adjusted to match it would install",
		},
		{
			what: "a line was appended",
			sums: append([]byte(sampleSums), "0000  ./bin/extra\n"...),
			sig:  good,
			why:  "an extra file could be added to a package we signed",
		},
		{
			what: "a line was removed",
			sums: []byte(strings.SplitAfter(sampleSums, "\n")[0]),
			sig:  good,
			why:  "a file could be dropped from a package we signed",
		},
		{
			what: "trailing whitespace was added",
			sums: []byte(sampleSums + " "),
			sig:  good,
			why:  "the signature would cover something other than the bytes on disk",
		},
		{
			what: "signed by a different key",
			sums: []byte(sampleSums),
			sig:  otherSig,
			why:  "anybody with any key could sign a package for this deployment",
		},
		{
			what: "one bit of the signature flipped",
			sums: []byte(sampleSums),
			sig:  flip(good, 0),
			why:  "a mangled signature would pass",
		},
		{
			what:   "the signature was truncated",
			sums:   []byte(sampleSums),
			sig:    good[:SignatureSize-1],
			why:    "a short read of the .sig file would pass",
			wantIn: "63 bytes",
		},
		{
			what:   "the signature is empty",
			sums:   []byte(sampleSums),
			sig:    nil,
			why:    "a package with no signature at all would install",
			wantIn: "0 bytes",
		},
		{
			what: "the sums are empty",
			sums: nil,
			sig:  good,
			why:  "an unreadable SHA256SUMS would verify",
		},
	}

	for _, c := range cases {
		t.Run(c.what, func(t *testing.T) {
			err := pub.Verify(c.sums, c.sig)
			if err == nil {
				t.Fatalf("this verified, and it must not: %s", c.why)
			}
			if !errors.Is(err, ErrBadSignature) {
				t.Errorf("rejected, but as %v rather than ErrBadSignature", err)
			}
			if c.wantIn != "" && !strings.Contains(err.Error(), c.wantIn) {
				t.Errorf("the refusal does not say %q, so it reads the same as a "+
					"package that was tampered with:\n  %v", c.wantIn, err)
			}
		})
	}
}

// TestAnUnsetKeyVerifiesNothing.
//
// The direction that matters. A deployment with no key configured must
// refuse every package, not accept every package - and a zero PublicKey
// is exactly what a config file with the line missing produces.
func TestAnUnsetKeyVerifiesNothing(t *testing.T) {
	priv, _ := newPair(t)
	sig, err := priv.Sign([]byte(sampleSums))
	if err != nil {
		t.Fatal(err)
	}

	var zero PublicKey
	if zero.IsSet() {
		t.Fatal("a zero PublicKey reports itself configured")
	}
	if err := zero.Verify([]byte(sampleSums), sig); !errors.Is(err, ErrNoKey) {
		t.Fatalf("an unset key answered %v; it must refuse, and say which kind of "+
			"refusal so the caller can tell \"not configured\" from \"wrong package\"", err)
	}

	var zeroPriv PrivateKey
	if _, err := zeroPriv.Sign([]byte(sampleSums)); !errors.Is(err, ErrNoKey) {
		t.Errorf("an unset signing key answered %v rather than refusing", err)
	}
}

// TestBothKeyEncodingsReadTheSameKey.
//
// Operators paste what their tool produced. The two forms must be the
// same key, and a key that round-trips through String() must still be
// that key - String() is what a person copies into upgrader.toml.
func TestBothKeyEncodingsReadTheSameKey(t *testing.T) {
	priv, pub := newPair(t)
	raw := []byte(pub.key)

	fromHex, err := ParsePublicKey(hex.EncodeToString(raw))
	if err != nil {
		t.Fatalf("hex: %v", err)
	}
	fromB64, err := ParsePublicKey(base64.StdEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	if fromHex.String() != fromB64.String() {
		t.Fatalf("the two encodings produced different keys:\n  hex %s\n  b64 %s",
			fromHex.String(), fromB64.String())
	}

	// And whitespace, because a key pasted from a web page arrives with
	// a newline on it more often than not.
	padded, err := ParsePublicKey("  " + hex.EncodeToString(raw) + "\n")
	if err != nil {
		t.Fatalf("a key with surrounding whitespace was rejected: %v", err)
	}

	sig, err := priv.Sign([]byte(sampleSums))
	if err != nil {
		t.Fatal(err)
	}
	for name, k := range map[string]PublicKey{"hex": fromHex, "base64": fromB64, "padded": padded} {
		if err := k.Verify([]byte(sampleSums), sig); err != nil {
			t.Errorf("the key read from %s does not verify what its own half signed: %v", name, err)
		}
	}
}

// TestTheSigningKeyKnowsItsOwnPublicHalf.
//
// Public() exists so nobody has to derive the verifying key by a second
// route and publish one that does not match. If it were wrong, every
// customer's config would carry a key that rejects every package we
// ship, and the symptom would arrive weeks later at the first update.
func TestTheSigningKeyKnowsItsOwnPublicHalf(t *testing.T) {
	priv, pub := newPair(t)
	if got := priv.Public().String(); got != pub.String() {
		t.Fatalf("Public() returned a different key:\n  got  %s\n  want %s", got, pub.String())
	}

	derived := priv.Public()
	sig, err := priv.Sign([]byte(sampleSums))
	if err != nil {
		t.Fatal(err)
	}
	if err := derived.Verify([]byte(sampleSums), sig); err != nil {
		t.Fatalf("the derived key does not verify its own signature: %v", err)
	}

	var zero PrivateKey
	if zero.Public().IsSet() {
		t.Error("an unset signing key produced a public key, which would be a key of zeroes")
	}
}

// TestKeysThatAreNotKeysAreRefused.
func TestKeysThatAreNotKeysAreRefused(t *testing.T) {
	long := strings.Repeat("a", 200)
	cases := map[string]string{
		"empty":            "",
		"only whitespace":  "   \n\t",
		"too short in hex": strings.Repeat("ab", PublicKeySize-1),
		"too long in hex":  strings.Repeat("ab", PublicKeySize+1),
		"not hex at all":   strings.Repeat("zz", PublicKeySize),
		"not base64":       "!!!!",
		"far too long":     long,
	}
	for what, in := range cases {
		t.Run(what, func(t *testing.T) {
			k, err := ParsePublicKey(in)
			if err == nil {
				t.Fatalf("%q was accepted as a key", in)
			}
			if k.IsSet() {
				t.Error("the rejected key still reports itself configured")
			}
			if k.String() != "" {
				t.Errorf("a rejected key renders as %q", k.String())
			}
		})
	}
}

// TestTheDomainSeparatorIsLoadBearing.
//
// Ed25519 signs bytes. Without the prefix, a signature this project
// produced over some other document would verify here if those bytes
// could be presented as a SHA256SUMS. This asserts the prefix is
// actually mixed in, by checking that a signature over the raw sums -
// what a signer that forgot it would produce - is refused.
func TestTheDomainSeparatorIsLoadBearing(t *testing.T) {
	priv, pub := newPair(t)

	// Signed the way a caller would if signedBytes did nothing.
	naive := ed25519.Sign(priv.key, []byte(sampleSums))

	if err := pub.Verify([]byte(sampleSums), naive); err == nil {
		t.Fatal("a signature over the bare sums verified, so the domain separator is " +
			"not being mixed in and a signature from elsewhere in this project could " +
			"be replayed as a release signature")
	}
}

// FuzzVerifyNeverPanicsAndNeverAccepts.
//
// Verification runs on a machine that has just been handed a file by
// somebody else, so the two properties are: it must not crash, and it
// must not say yes. Only a signature this test's own key produced over
// exactly these bytes may verify, and the fuzzer cannot construct one.
func FuzzVerifyNeverPanicsAndNeverAccepts(f *testing.F) {
	priv, pub := newPair(f)
	good, err := priv.Sign([]byte(sampleSums))
	if err != nil {
		f.Fatal(err)
	}

	f.Add([]byte(sampleSums), good)
	f.Add([]byte(sampleSums), []byte(nil))
	f.Add([]byte(nil), good)
	f.Add([]byte("0000  ./bin/collector\n"), good)
	f.Add([]byte(sampleSums), append([]byte(nil), byte(0)))

	f.Fuzz(func(t *testing.T, sums, sig []byte) {
		err := pub.Verify(sums, sig)
		if err == nil && !(string(sums) == sampleSums && string(sig) == string(good)) {
			t.Fatalf("verified something we did not sign:\n  sums %q\n  sig  %x", sums, sig)
		}
	})
}

// FuzzParsePublicKeyNeverPanics. The value comes from a config file an
// operator edited, so every shape of wrong has to be a message.
func FuzzParsePublicKeyNeverPanics(f *testing.F) {
	f.Add(strings.Repeat("ab", PublicKeySize))
	f.Add("")
	f.Add("   ")
	f.Add("=")
	f.Add(strings.Repeat("a", 64))

	f.Fuzz(func(t *testing.T, in string) {
		k, err := ParsePublicKey(in)
		if err != nil {
			if k.IsSet() {
				t.Fatalf("ParsePublicKey(%q) failed and still returned a usable key", in)
			}
			return
		}
		if !k.IsSet() {
			t.Fatalf("ParsePublicKey(%q) succeeded and returned an unset key", in)
		}
		if len(k.key) != PublicKeySize {
			t.Fatalf("ParsePublicKey(%q) returned %d bytes", in, len(k.key))
		}
	})
}
