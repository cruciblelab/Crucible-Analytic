// Package releasesign proves who made a release package.
//
// # Why this exists
//
// Every package already carries SHA256SUMS, and it is worth exactly what
// it claims: it detects a file that was corrupted or edited after the
// list was written. It says nothing at all about who wrote the list,
// because the build produces it and ships it inside the same tarball.
// Anybody who can hand somebody a package can hand them a matching
// SHA256SUMS with it.
//
// That was fine while installing meant a person deciding to unpack an
// archive they had chosen. It stops being fine the moment the panel can
// ask for an update: then "install this package" is a request that
// arrives over the network, and a checksum the requester also supplies
// is not a check. Without a signature, a panel that can ask for an
// update is a panel that can run code, and the panel is the part of this
// system that faces the internet.
//
// So the rule this package exists to enforce:
//
//	the machine installs only what a key it already trusts has signed.
//
// # Where the key lives, and why it matters more than the algorithm
//
// The public key belongs in upgrader.toml, which is mode 0640 and owned
// by the `crucible-upgrader` group. The four services run as `crucible`
// and cannot read it. That is the same separation the five database
// roles buy, applied to code instead of tables: the panel may write a
// request row saying "please update", and it cannot influence what gets
// installed, because the answer to "is this really ours" is computed
// from a file the panel cannot reach.
//
// A public key in the database would undo all of it. The panel can write
// to the database.
//
// # Ed25519, and what is signed
//
// Ed25519 because it is in the standard library, its keys are 32 bytes
// and paste into a config file on one line, and it has no parameters to
// get wrong. No key sizes, no curve choices, no padding modes.
//
// What is signed is the SHA256SUMS file, byte for byte - not the tarball
// and not the individual binaries. One signature then covers every file
// in the package, because SHA256SUMS already covers them and is already
// verified on unpack. Signing the archive instead would leave the
// unpacked tree unverifiable, which is the state it is in when the
// installer runs.
package releasesign

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// The domain separator mixed into every signature.
//
// Ed25519 signs whatever bytes it is given, so a signature over some
// other document produced by this project - a token, a snippet, a
// config file - would verify here if somebody could arrange for those
// bytes to be presented as a SHA256SUMS. Prefixing a string that names
// this use makes a signature from elsewhere fail, and costs one line.
//
// The version number in it is not decoration. If the signed content ever
// changes shape, this string changes with it, and old signatures stop
// verifying rather than being reinterpreted under new rules.
const domain = "crucible-analytic/release-sums/v1\n"

// Sizes, exported so a caller can say why a key was rejected without
// importing crypto/ed25519 itself.
const (
	PublicKeySize  = ed25519.PublicKeySize
	PrivateKeySize = ed25519.PrivateKeySize
	SignatureSize  = ed25519.SignatureSize
)

var (
	// ErrNoKey means nothing was configured. Its own error because the
	// caller answers it differently from a malformed one: "this
	// deployment does not do signed updates" is a state, and "the key
	// in your config file is wrong" is a fault.
	ErrNoKey = errors.New("releasesign: no key configured")
	// ErrKeySize means the value decoded but is the wrong length.
	ErrKeySize = errors.New("releasesign: a public key is 32 bytes")
	// ErrBadSignature means the bytes are not what this key signed.
	// Returned for every failure of verification - wrong key, edited
	// sums, truncated signature - because the difference is not
	// information the caller should act on differently, and reporting it
	// tells whoever supplied the package which part to change.
	ErrBadSignature = errors.New("releasesign: the signature does not match this package")
)

// PublicKey is a verifying key, in a type of its own so a caller cannot
// pass a private key, a hash, or an IP token key by accident.
type PublicKey struct {
	key ed25519.PublicKey
}

// IsSet reports whether a key is configured.
//
// A zero PublicKey verifies nothing rather than verifying everything,
// which is the direction that matters: an unset key must not be a key
// that accepts.
func (p PublicKey) IsSet() bool { return len(p.key) == PublicKeySize }

// ParsePublicKey reads a key from its configured form: 64 hex
// characters, or base64.
//
// Both forms, and the length-first split, follow internal/sealed - the
// operator pastes what their tool produced, and a second convention for
// the same job in one config file is a support question waiting to
// happen. Unlike sealed's key this one is public, so nothing here is
// secret; the care is about reading the right 32 bytes, not hiding them.
func ParsePublicKey(encoded string) (PublicKey, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return PublicKey{}, ErrNoKey
	}
	raw, err := decode(encoded, PublicKeySize)
	if err != nil {
		return PublicKey{}, err
	}
	if len(raw) != PublicKeySize {
		return PublicKey{}, fmt.Errorf("%w (got %d)", ErrKeySize, len(raw))
	}
	return PublicKey{key: ed25519.PublicKey(raw)}, nil
}

// String renders the key in the form a config file carries.
//
// Hex, one line, lower case. Chosen over base64 for the one place a
// person reads this: comparing the key in a config file against the key
// on a release page, character by character, when something does not
// verify. Hex has no case ambiguity and no padding.
func (p PublicKey) String() string {
	if !p.IsSet() {
		return ""
	}
	return hex.EncodeToString(p.key)
}

// Verify reports whether sig is this key's signature over sums.
//
// sums is the SHA256SUMS content, exactly as it sits on disk. Callers
// must not trim, re-order or normalise it: the signature covers bytes,
// and a caller that tidies the input is a caller that will one day tidy
// it differently from the signer.
func (p PublicKey) Verify(sums, sig []byte) error {
	if !p.IsSet() {
		return ErrNoKey
	}
	if len(sig) != SignatureSize {
		return fmt.Errorf("%w (the signature is %d bytes, not %d)",
			ErrBadSignature, len(sig), SignatureSize)
	}
	if !ed25519.Verify(p.key, signedBytes(sums), sig) {
		return ErrBadSignature
	}
	return nil
}

// PrivateKey signs. It lives on whoever builds releases, never on a
// customer's machine, and nothing in the shipped binaries calls Sign.
type PrivateKey struct {
	key ed25519.PrivateKey
}

// IsSet reports whether a key is configured.
func (s PrivateKey) IsSet() bool { return len(s.key) == PrivateKeySize }

// ParsePrivateKey reads a signing key: 128 hex characters, or base64 of
// 64 bytes.
//
// The 64 bytes are Ed25519's seed-plus-public-key form, which is what
// ed25519.GenerateKey returns. Taking that whole value rather than the
// 32-byte seed means the key a person stores is the key the standard
// library hands them, with no step in between where they could store
// half of it.
func ParsePrivateKey(encoded string) (PrivateKey, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return PrivateKey{}, ErrNoKey
	}
	raw, err := decode(encoded, PrivateKeySize)
	if err != nil {
		return PrivateKey{}, err
	}
	if len(raw) != PrivateKeySize {
		return PrivateKey{}, fmt.Errorf("releasesign: a signing key is %d bytes (got %d)",
			PrivateKeySize, len(raw))
	}
	return PrivateKey{key: ed25519.PrivateKey(raw)}, nil
}

// Generate makes a new signing key.
//
// Here rather than in the tool that calls it so the key's shape is
// decided in the same file that parses it, and so a test can make a real
// pair without reaching for crypto/ed25519 and getting the two halves
// the wrong way round - which is easy, because GenerateKey returns the
// public one first.
func Generate() (PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return PrivateKey{}, fmt.Errorf("releasesign: generating a key: %w", err)
	}
	return PrivateKey{key: priv}, nil
}

// String renders the signing key in the form CA_RELEASE_KEY takes.
//
// Hex, for the same reason the public key uses it: somebody compares
// this against what their password manager holds. It is a secret, so
// nothing in the shipped binaries calls this - only the maintainer's
// tool, once, when the key is made.
func (s PrivateKey) String() string {
	if !s.IsSet() {
		return ""
	}
	return hex.EncodeToString(s.key)
}

// Public returns the verifying half, so the person holding the signing
// key can print what to put in upgrader.toml without a second tool and
// without a chance of publishing a key that does not match.
func (s PrivateKey) Public() PublicKey {
	if !s.IsSet() {
		return PublicKey{}
	}
	pub, ok := s.key.Public().(ed25519.PublicKey)
	if !ok {
		// Unreachable with a key this package parsed: ed25519's own
		// Public() returns exactly this type. Handled rather than
		// asserted because a panic in a release tool is a worse failure
		// than a key that reports itself unset.
		return PublicKey{}
	}
	return PublicKey{key: pub}
}

// Sign produces the signature that goes beside SHA256SUMS.
func (s PrivateKey) Sign(sums []byte) ([]byte, error) {
	if !s.IsSet() {
		return nil, ErrNoKey
	}
	return ed25519.Sign(s.key, signedBytes(sums)), nil
}

// signedBytes is what both halves actually run over.
//
// One function rather than the prefix written twice, because a signer
// and a verifier that disagree about the domain separator produce a
// system where nothing verifies and both sides look correct in isolation.
func signedBytes(sums []byte) []byte {
	out := make([]byte, 0, len(domain)+len(sums))
	out = append(out, domain...)
	return append(out, sums...)
}

// decode picks hex or base64 by length, like internal/sealed.
func decode(encoded string, size int) ([]byte, error) {
	if len(encoded) == hex.EncodedLen(size) {
		raw, err := hex.DecodeString(encoded)
		if err == nil {
			return raw, nil
		}
		return nil, fmt.Errorf("releasesign: the key looks like hex but does not decode: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("releasesign: the key is neither %d hex characters nor base64: %w",
			hex.EncodedLen(size), err)
	}
	return raw, nil
}
