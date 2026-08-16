// Package argon2id encodes, decodes and verifies argon2id hashes in the
// standard PHC string format.
//
// It is its own package because two different things now hash secrets
// with it and neither should own the other: internal/panel hashes
// account passwords, and internal/devgate verifies the developer
// password that guards the settings with legal weight. A second copy of
// the PHC parsing would be a second place for the bounds checks below to
// be forgotten, and the entire value of those checks is that they are
// never forgotten.
//
// The primitive is golang.org/x/crypto/argon2 - nobody here is writing a
// KDF. What belongs to this package is the encoding, the parameter
// choice, and the refusal to believe what a stored hash claims about its
// own cost.
//
// The encoded form is the standard PHC string:
//
//	$argon2id$v=19$m=19456,t=2,p=1$<base64 salt>$<base64 hash>
//
// Parameters travel inside the hash rather than in configuration, so
// raising the cost later still verifies every existing secret correctly:
// each hash is checked with the parameters it was made with, and Verify
// reports which ones are now below standard so they can be upgraded at
// the only moment the plaintext is available.
package argon2id

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// The current cost parameters: OWASP's first recommended argon2id
// configuration, m=19456,t=2,p=1.
//
// Deliberately not the 64 MiB variant. This runs on the same machine as
// a collector, a beacon and TimescaleDB, and a login burst that
// allocates 64 MiB per attempt is a denial of service against the site
// the collector is supposed to be protecting.
const (
	// Memory is 19 MiB, in KiB as argon2 counts it.
	Memory  = 19 * 1024
	Time    = 2
	Threads = 1

	SaltLen = 16
	KeyLen  = 32
)

// MaxInputBytes bounds the plaintext argon2 is asked to hash.
//
// Without a cap, a multi-megabyte "password" is a cheap way to make the
// server do expensive work, and the cap has to live down here rather
// than in each caller's own validation: a caller that forgot would be
// the one path an attacker uses.
const MaxInputBytes = 1024

// Hash derives an argon2id hash of plain and returns it PHC-encoded.
//
// It enforces no minimum length. How long a secret must be is a policy
// question that differs by what the secret protects - an account
// password and a developer gate password have different answers - so it
// belongs to the caller, and every caller in this project has one.
func Hash(plain string) (string, error) {
	if len(plain) > MaxInputBytes {
		return "", fmt.Errorf("argon2id: input is longer than %d bytes", MaxInputBytes)
	}

	salt := make([]byte, SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("argon2id: draw salt: %w", err)
	}

	key := argon2.IDKey([]byte(plain), salt, Time, Memory, Threads, KeyLen)
	return encode(salt, key, params{memory: Memory, time: Time, threads: Threads}), nil
}

// Verify checks plain against an encoded hash.
//
// It returns ok=false for a wrong secret *and* for a malformed hash,
// with no distinction: a caller able to tell them apart would leak which
// records are corrupt, and there is nothing useful to do differently in
// either case anyway.
//
// needsRehash reports that the stored hash used weaker parameters than
// the current ones. The caller should re-hash and store on success,
// which is the only moment the plaintext exists.
func Verify(encoded, plain string) (ok bool, needsRehash bool) {
	if len(plain) > MaxInputBytes {
		// Refused before argon2 sees it, for the same reason Hash bounds
		// it. Not a timing oracle: the caller already knows how long the
		// value it sent was.
		return false, false
	}

	p, salt, want, err := decode(encoded)
	if err != nil {
		return false, false
	}

	got := argon2.IDKey([]byte(plain), salt, p.time, p.memory, p.threads, uint32(len(want)))
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false
	}

	weaker := p.memory < Memory || p.time < Time || len(want) < KeyLen
	return true, weaker
}

// CheckEncoding reports whether encoded is a hash this package can
// verify against, without needing the plaintext.
//
// It exists for configuration validation. A mistyped hash in a config
// file would otherwise behave exactly like a permanently wrong password:
// every attempt refused, no error anywhere, and the operator hunting for
// a typo in the one thing they cannot see. Failing at startup with a
// clear message is worth the few lines.
func CheckEncoding(encoded string) error {
	_, _, _, err := decode(encoded)
	return err
}

// dummy is a real hash of a value nobody knows, computed once at init.
//
// VerifyDummy exists so an attempt against something that has no stored
// hash costs the same as one against something that does. Without it,
// "no such account" returns in microseconds while "wrong password"
// returns in tens of milliseconds, which turns any login form into a
// reliable oracle for which identities exist.
var dummy = func() string {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		// Fall back rather than panicking at init: the only consequence
		// is that this particular decoy is predictable, and nothing
		// verifies against it successfully either way.
		secret = []byte("argon2id-dummy-placeholder-value")
	}
	salt := make([]byte, SaltLen)
	key := argon2.IDKey(secret, salt, Time, Memory, Threads, KeyLen)
	return encode(salt, key, params{memory: Memory, time: Time, threads: Threads})
}()

// VerifyDummy performs the work Verify would, against a hash that cannot
// match. Call it on the "nothing to compare against" path.
func VerifyDummy(plain string) {
	Verify(dummy, plain)
}

// params are the cost parameters read back out of an encoded hash.
type params struct {
	memory  uint32
	time    uint32
	threads uint8
}

func encode(salt, key []byte, p params) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, p.memory, p.time, p.threads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	)
}

// ErrMalformed is returned by CheckEncoding for anything this package
// would refuse to verify against.
var ErrMalformed = errors.New("argon2id: malformed hash")

// decode parses the PHC string. Every field is validated rather than
// assumed: these values arrive from a database row or a config file, and
// a corrupted or hostile one must not be able to make argon2 allocate an
// absurd amount of memory.
func decode(encoded string) (params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// "", "argon2id", "v=19", "m=..,t=..,p=..", salt, hash
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return params{}, nil, nil, ErrMalformed
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return params{}, nil, nil, ErrMalformed
	}

	var p params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.memory, &p.time, &p.threads); err != nil {
		return params{}, nil, nil, ErrMalformed
	}
	// An upper bound as well as a lower one: a record claiming
	// m=16777216 would make one verification try to allocate 16 GiB.
	if p.memory < 8*1024 || p.memory > 1024*1024 || p.time < 1 || p.time > 16 || p.threads < 1 || p.threads > 16 {
		return params{}, nil, nil, ErrMalformed
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) < 8 {
		return params{}, nil, nil, ErrMalformed
	}
	key, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(key) < 16 {
		return params{}, nil, nil, ErrMalformed
	}

	return p, salt, key, nil
}
