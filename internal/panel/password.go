package panel

import (
	"fmt"
	"unicode/utf8"

	"github.com/cruciblelab/crucible-analytic/internal/argon2id"
)

// Account passwords are argon2id hashes in PHC encoding. The primitive
// and the encoding live in internal/argon2id, which internal/devgate
// also uses; what stays here is the policy this project applies to a
// *panel account* password, which is not the same policy the developer
// gate applies to its own secret.
//
// That is the whole reason for the split. "Don't roll your own auth"
// asks that nobody writes a KDF; it does not excuse anybody from
// deciding how long a password must be, and that decision differs by
// what the password protects.

// Password length bounds.
const (
	// MinPasswordLen is 12 rather than the 8 NIST allows for general
	// accounts. This is an administrative panel over other people's
	// traffic data, and length is the only property that reliably buys
	// anything - which is also why there are deliberately no
	// composition rules (an upper case letter and a digit turn
	// "password" into "Password1", which is not progress).
	MinPasswordLen = 12
	// MaxPasswordLen bounds the input argon2 has to hash. Without a cap,
	// a multi-megabyte "password" is a cheap way to make the server do
	// expensive work.
	MaxPasswordLen = 128
)

// ErrPasswordTooShort and ErrPasswordTooLong are returned by
// ValidatePassword so callers can render the right message rather than
// matching on text.
var (
	ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	ErrPasswordTooLong  = fmt.Errorf("password must be at most %d characters", MaxPasswordLen)
)

// ValidatePassword checks a proposed password against the length bounds.
// Counted in runes, not bytes: a Turkish passphrase is shorter in
// characters than in bytes, and rejecting it for being "too long" while
// it looks short would be baffling.
func ValidatePassword(plain string) error {
	switch n := utf8.RuneCountInString(plain); {
	case n < MinPasswordLen:
		return ErrPasswordTooShort
	case n > MaxPasswordLen:
		return ErrPasswordTooLong
	}
	return nil
}

// HashPassword derives an argon2id hash of plain and returns it in PHC
// encoded form.
func HashPassword(plain string) (string, error) {
	if err := ValidatePassword(plain); err != nil {
		return "", err
	}
	return argon2id.Hash(plain)
}

// VerifyPassword checks plain against an encoded hash.
//
// It returns ok=false for a wrong password *and* for a malformed hash,
// with no distinction: a caller that could tell them apart would leak
// which accounts have corrupt records, and there is nothing useful to do
// differently in either case anyway.
//
// needsRehash reports that the stored hash used weaker parameters than
// the current ones. The caller should re-hash and store on a successful
// login, which is the only moment the plaintext is available.
func VerifyPassword(encoded, plain string) (ok bool, needsRehash bool) {
	return argon2id.Verify(encoded, plain)
}

// VerifyDummy performs the same work VerifyPassword would, against a
// hash that cannot match. Call it on the "no such account" path, so a
// login attempt for an address with no account costs the same as one for
// an address with an account - otherwise the login form is a reliable
// oracle for whether a given email is registered here.
func VerifyDummy(plain string) {
	argon2id.VerifyDummy(plain)
}
