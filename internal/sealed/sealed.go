// Package sealed encrypts the few secrets this product has to be able to
// read back.
//
// # Why anything here is reversible at all
//
// Everything else that looks like a credential in this database is a
// hash: panel passwords are argon2id, invitations and API tokens and
// recovery codes are SHA-256 digests. Nothing needs the original, so
// nothing keeps it.
//
// An SMTP password is different in kind. It has to be handed to somebody
// else's mail server on every send, so it has to be recoverable, and no
// amount of preferring hashes changes that. The question is therefore not
// "hashed or not" but "what does a copy of the database get you", and the
// answer here is: nothing, without the key, which is in a file the
// database has never seen.
//
// # What this protects against, and what it does not
//
// It protects against the database being read without the config file.
// That is the common exposure and it is worth naming precisely, because
// it happens constantly and rarely looks like an attack: a nightly dump
// copied to object storage, a restore into a staging box that nobody
// hardened, a support ticket with a table attached, a decommissioned
// replica. Every one of those hands over a working mail password if the
// column is plaintext.
//
// It does not protect against an attacker who has the panel process or
// its filesystem. They have the key and the database, and the password
// is theirs. Saying otherwise would be the more comfortable sentence and
// it would be false - and a security property stated too broadly is worse
// than none, because somebody stops thinking about the case it does not
// cover.
//
// # Construction
//
// AES-256-GCM through crypto/cipher.NewGCMWithRandomNonce, which draws
// the nonce itself and carries it in the ciphertext. Chosen because nonce
// reuse is the way GCM actually fails in the field, and no code here can
// make that mistake if no code here handles a nonce.
//
// Each ciphertext is bound to a label naming what it is, as additional
// authenticated data. A value lifted out of one column and pasted into
// another fails to open rather than decrypting into the wrong meaning.
package sealed

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// KeySize is 32 bytes, for AES-256.
const KeySize = 32

// prefix marks the format of a stored value.
//
// Present from the first version, before there is anything to
// distinguish, because the alternative is a column of bare base64 that a
// later format cannot be told apart from - and the migration that has to
// guess is the one that corrupts the rows it guesses wrong.
const prefix = "v1."

// Errors callers distinguish. The panel shows a different sentence for
// each: a key that changed is somebody's edit to a config file, and a
// value that will not open is a row to re-enter, not a bug to report.
var (
	// ErrNoKey means no encryption key is configured.
	ErrNoKey = errors.New("sealed: no encryption key is configured")
	// ErrKeySize means the configured key is the wrong length.
	ErrKeySize = fmt.Errorf("sealed: the encryption key must be %d bytes", KeySize)
	// ErrFormat means the stored value is not in this package's format.
	ErrFormat = errors.New("sealed: the stored value is not in the expected format")
	// ErrCannotOpen means the value did not authenticate. Almost always
	// the key having changed rather than the data having been tampered
	// with, but the two are indistinguishable from here and the honest
	// message covers both.
	ErrCannotOpen = errors.New("sealed: this value cannot be decrypted with the configured key")
)

// Key is the deployment's encryption key.
//
// A type rather than a []byte so a caller cannot pass the wrong 32 bytes
// - a hash, a token, a site id - without saying so.
type Key struct {
	aead cipher.AEAD
	// set distinguishes a zero Key from a configured one. A zero Key is
	// what an installation without mail has, and it must fail loudly on
	// use rather than encrypting under a key of zeroes.
	set bool
}

// ParseKey reads a key from its configured form: 64 hex characters, or
// standard base64 of 32 bytes.
//
// Two accepted forms because operators paste what their tools produce -
// `openssl rand -hex 32` and `openssl rand -base64 32` are both what
// people have to hand, and rejecting one of them produces a support
// question rather than a security property.
func ParseKey(encoded string) (Key, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return Key{}, ErrNoKey
	}

	raw, err := decodeKey(encoded)
	if err != nil {
		return Key{}, err
	}
	if len(raw) != KeySize {
		return Key{}, fmt.Errorf("%w (got %d)", ErrKeySize, len(raw))
	}

	block, err := aes.NewCipher(raw)
	if err != nil {
		return Key{}, fmt.Errorf("sealed: %w", err)
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return Key{}, fmt.Errorf("sealed: %w", err)
	}
	return Key{aead: aead, set: true}, nil
}

// decodeKey picks the encoding by length rather than by trying one and
// falling back.
//
// Not because the alternative is unsafe. The first draft of this comment
// claimed a base64 key could be misread as hex, and measuring it showed
// that cannot happen: 32 bytes in standard base64 is always 44
// characters ending in "=", and "=" is not a hex digit, so hex decoding
// of a well-formed base64 key always fails. Ten thousand random keys,
// zero misreads. The claim was wrong and is recorded here rather than
// quietly deleted, because a comment asserting a danger that does not
// exist is how a later reader learns to disbelieve the comments that do.
//
// So this is a readability choice: one length, one encoding, no
// fall-through to reason about.
//
// The genuine ambiguity is the other direction and neither approach
// removes it. A 64-character string is read as hex, and 64 characters is
// also the base64 length of a 48-byte key - so a 48-byte key given in
// base64, made entirely of hex digits, would decode as a different
// 32-byte key without complaint. Left alone deliberately: the chance is
// (22/64)^64, and code guarding it would be read as guarding something
// that happens.
func decodeKey(encoded string) ([]byte, error) {
	if len(encoded) == hex.EncodedLen(KeySize) {
		raw, err := hex.DecodeString(encoded)
		if err == nil {
			return raw, nil
		}
		return nil, fmt.Errorf("sealed: the key looks like hex but does not decode: %w", err)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("sealed: the key is neither %d hex characters nor base64: %w",
			hex.EncodedLen(KeySize), err)
	}
	return raw, nil
}

// IsSet reports whether a key is configured.
func (k Key) IsSet() bool { return k.set }

// Seal encrypts plaintext under a label describing what it is.
//
// The label is authenticated but not secret. It exists so a ciphertext
// belongs to one column: moving the sealed SMTP password into a column
// read with a different label produces a value that will not open,
// rather than one that opens into the wrong meaning.
func (k Key) Seal(label, plaintext string) (string, error) {
	if !k.set {
		return "", ErrNoKey
	}
	// The second nil is the nonce, and it has to be empty.
	//
	// It reads like the worst mistake in GCM and is the opposite of one.
	// NewGCMWithRandomNonce returns an AEAD that draws its own nonce -
	// crypto/internal/fips140/aes/gcm calls drbg.Read(nonce) - and
	// panics on any nonce passed in ("non-empty nonce passed to
	// GCMWithRandomNonce"). Handing it one is not possible, which is why
	// this construction was chosen over the ordinary NewGCM.
	//
	// gosec flags it as G407, hardcoded IV. Triaged in
	// .sast-baseline.json rather than worked around; the note is here as
	// well because the next person to read this line will have the same
	// reaction the scanner did, and they deserve the answer without
	// having to go and find the baseline.
	box := k.aead.Seal(nil, nil, []byte(plaintext), []byte(label))
	return prefix + base64.StdEncoding.EncodeToString(box), nil
}

// Open decrypts a value sealed under the same label.
func (k Key) Open(label, stored string) (string, error) {
	if !k.set {
		return "", ErrNoKey
	}
	body, ok := strings.CutPrefix(stored, prefix)
	if !ok {
		return "", ErrFormat
	}
	box, err := base64.StdEncoding.DecodeString(body)
	if err != nil {
		return "", ErrFormat
	}
	plaintext, err := k.aead.Open(nil, nil, box, []byte(label))
	if err != nil {
		// The underlying error is deliberately not wrapped. GCM's
		// failure carries no detail worth having and every word of it
		// invites somebody to treat "why" as answerable, which for an
		// authentication failure it is not.
		return "", ErrCannotOpen
	}
	return string(plaintext), nil
}
