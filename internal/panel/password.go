package panel

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Password hashing uses argon2id from golang.org/x/crypto - the
// primitive is not ours, only the encoding and the parameter choice.
// That is what "don't roll your own auth" actually asks for: nobody
// should be writing a KDF, and everybody has to decide how to store its
// output.
//
// The encoded form is the standard PHC string:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<base64 salt>$<base64 hash>
//
// Parameters travel *inside* the hash rather than living in config, so
// raising the cost later still verifies every existing password
// correctly - each hash is checked with the parameters it was made
// with, and VerifyPassword reports which ones are now below standard so
// they can be upgraded on the next successful login.

const (
	// argon2Memory is 19 MiB, OWASP's first recommended argon2id
	// configuration. Deliberately not the 64 MiB variant: this runs on
	// the same VDS as a collector, a beacon and TimescaleDB, and a
	// login burst that allocates 64 MiB per attempt is a denial of
	// service against the site the collector is supposed to protect.
	argon2Memory = 19 * 1024
	// argon2Time and argon2Threads complete the OWASP m=19456,t=2,p=1
	// configuration.
	argon2Time    = 2
	argon2Threads = 1

	argon2SaltLen = 16
	argon2KeyLen  = 32
)

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

	salt := make([]byte, argon2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("panel: draw password salt: %w", err)
	}

	key := argon2.IDKey([]byte(plain), salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)

	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
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
	params, salt, want, err := decodeHash(encoded)
	if err != nil {
		return false, false
	}

	got := argon2.IDKey([]byte(plain), salt, params.time, params.memory, params.threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false
	}

	weaker := params.memory < argon2Memory || params.time < argon2Time || len(want) < argon2KeyLen
	return true, weaker
}

// dummyHash is a real argon2id hash of a value nobody knows, computed
// once at startup.
//
// VerifyDummy exists so that a login attempt for an address with no
// account costs the same as one for an address with an account. Without
// it, "no such user" returns in microseconds and "wrong password"
// returns in tens of milliseconds, which turns the login form into a
// reliable oracle for whether a given email is registered here.
var dummyHash = func() string {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		// Fall back to a fixed value rather than panicking at init: the
		// only consequence is that this particular decoy is predictable,
		// and nothing verifies against it successfully either way.
		secret = []byte("panel-dummy-password-placeholder")
	}
	salt := make([]byte, argon2SaltLen)
	key := argon2.IDKey(secret, salt, argon2Time, argon2Memory, argon2Threads, argon2KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argon2Memory, argon2Time, argon2Threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}()

// VerifyDummy performs the same work VerifyPassword would, against a
// hash that cannot match. Call it on the "no such account" path.
func VerifyDummy(plain string) {
	VerifyPassword(dummyHash, plain)
}

// hashParams are the cost parameters read back out of an encoded hash.
type hashParams struct {
	memory  uint32
	time    uint32
	threads uint8
}

var errMalformedHash = errors.New("panel: malformed password hash")

// decodeHash parses the PHC string. Every field is validated rather than
// assumed: these values come from the database, and a corrupted or
// hostile row must not be able to make argon2 allocate an absurd amount
// of memory.
func decodeHash(encoded string) (hashParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return hashParams{}, nil, nil, errMalformedHash
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return hashParams{}, nil, nil, errMalformedHash
	}

	var p hashParams
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return hashParams{}, nil, nil, errMalformedHash
	}
	// An upper bound as well as a lower one: a row claiming m=16777216
	// would make one verification try to allocate 16 GiB.
	if p.memory < 8*1024 || p.memory > 1024*1024 || p.time < 1 || p.time > 16 || p.threads < 1 || p.threads > 16 {
		return hashParams{}, nil, nil, errMalformedHash
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return hashParams{}, nil, nil, errMalformedHash
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < 16 {
		return hashParams{}, nil, nil, errMalformedHash
	}

	return p, salt, key, nil
}
