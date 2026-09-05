// Package devseal encrypts a file so that only the holder of the
// developer password can open it, on a machine that cannot open it
// itself.
//
// # The problem it exists to solve
//
// The secrets backup is the configuration of this deployment: database
// passwords for all five roles, the session key, the mail key, and
// `ip_hash_key` - the value that makes every stored address a pseudonym
// rather than an address. internal/backup/schema.sql says what that
// means: a file holding both the data and the key undoes the
// pseudonymisation for anybody who has it. Hence two files, and hence
// this package for the second one.
//
// But "two files" only buys something if they are protected
// differently. Written plainly, both files would sit in one directory,
// owned by one account, at one mode - and whoever reads one reads the
// other. The split would be a fact about the filenames.
//
// So the secrets file is encrypted, and the interesting part is *to
// what*. Not to a key in a config file: the config file is the thing
// being backed up, and a key on the machine is a key an attacker on the
// machine has. It is encrypted to the developer password, which is not
// on the machine at all - only its argon2id hash is, in a file this
// package never reads.
//
// # The property this buys
//
// **The machine can produce a secrets backup and cannot read one.** Not
// the upgrader that wrote it, not the panel, not root. The bytes needed
// to open the file do not exist anywhere on the disk, so a stolen
// backup directory, a copied volume, a decommissioned VM image and a
// support ticket with a tarball attached are all worth nothing without
// a password that lives in somebody's head.
//
// That is the honest meaning of the plan's sentence "sırlar yedeği
// geliştirici parolasında". A file mode cannot be behind a password. A
// key derivation can.
//
// # Construction
//
// A password-derived X25519 recipient, which is the ordinary way to get
// "encrypt without being able to decrypt":
//
//	seed    = argon2id(password, salt, params)      32 bytes
//	private = X25519 scalar from seed
//	public  = X25519 public key                     the Recipient
//
// The public half goes into upgrader.toml. Sealing draws an ephemeral
// key pair, agrees a shared secret with the recipient, and derives the
// AEAD key with HKDF-SHA256:
//
//	shared = ECDH(ephemeral private, recipient public)
//	key    = HKDF(shared, salt = ephemeral public || recipient public,
//	              info = domain)
//
// The ephemeral public key travels with the file; the ephemeral private
// key is discarded before the file is closed, which is what stops the
// writer from re-deriving the same shared secret afterwards.
//
// The AEAD itself is internal/sealed, unchanged and unwrapped: one
// AES-256-GCM implementation in this codebase, with one place the nonce
// handling is reasoned about. The header is passed as its label, so the
// ciphertext is bound to the parameters somebody would have to alter to
// attack it.
//
// # What this does not protect against
//
// An attacker who is on the machine *while a backup is being taken*.
// The upgrader reads the configuration files in plaintext in order to
// encrypt them, so anything watching that process sees them. Nothing
// here changes that, and it is not the exposure this is for: the
// exposure is the file afterwards, sitting on a disk for months, in
// every copy of that disk.
//
// It also does not protect a weak password. Anybody holding the file
// can guess offline, at the cost of one argon2id computation per guess
// with the parameters the file names. That cost is the whole defence,
// which is why the parameters here are far heavier than the ones used
// for logins - see Params - and why devgate requires sixteen
// characters.
package devseal

import (
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/cruciblelab/crucible-analytic/internal/sealed"
)

// domain separates this use of the key agreement from any other.
//
// The same reasoning as internal/releasesign's: the primitives here
// would happily produce a key for bytes that came from somewhere else,
// and a version number in the string means a later format change makes
// old files fail to open rather than be reinterpreted under new rules.
const domain = "crucible-analytic/secrets-backup/v1"

// prefix marks the encoded form of a Recipient.
//
// Present from the first version, before there is anything to
// distinguish it from, for the reason internal/sealed records: a bare
// encoded value that a later format cannot be told apart from is a
// migration that has to guess.
const prefix = "cadev1."

// SaltLen is the argon2id salt carried in the recipient and the file.
const SaltLen = 16

// KeyLen is the derived seed, which is also the X25519 key size.
const KeyLen = 32

// Params are the argon2id cost parameters, carried by value rather than
// fixed, so that raising the cost later still opens today's files.
//
// # Why these are much heavier than a login's
//
// internal/argon2id uses m=19 MiB, t=2 - OWASP's first configuration -
// and explains why: it runs on the same machine as a collector, and a
// login burst that allocates 64 MiB per attempt is a denial of service
// against the site the collector protects.
//
// None of that applies here. This derivation happens twice in the life
// of a deployment: once in cmd/devpass when the recipient is generated,
// and once on the developer's own machine when a backup is opened.
// Never in a request handler, never on the serving path, never more
// than one at a time.
//
// And the threat is the opposite shape. A login is guessed online,
// through a rate limiter, against an account that locks. This file is
// guessed *offline*, by somebody holding it, as fast as their hardware
// allows. The per-guess cost is the entire defence, so it is set as
// high as a one-off interactive operation can bear rather than as low
// as a server can afford.
//
// Measured: 378 ms and 128 MiB on the machine this was written on.
type Params struct {
	// Memory in KiB, as argon2 counts it.
	Memory uint32
	// Time is the number of passes.
	Time uint32
	// Threads is the parallelism.
	Threads uint8
}

// Current is what a new recipient is generated with.
var Current = Params{Memory: 128 * 1024, Time: 3, Threads: 4}

// The bounds a stored Params must be inside.
//
// # Why a maximum matters more than a minimum
//
// The parameters travel in the file, because a restore happens when the
// configuration is gone and the file has to be self-sufficient. So an
// altered file can ask the opener to allocate whatever it likes, and
// "m=64 GiB" is a one-line denial of service against the person trying
// to recover their system - at the worst moment they will ever have.
//
// The minimum is the less important half and is worth being honest
// about: lowering it does not help an attacker. Somebody guessing
// offline uses whatever cost they please against the real ciphertext,
// and the number in the header does not bind them. It is here so that a
// file whose header was edited down fails as a malformed file rather
// than quietly deriving a cheap key and reporting that the password was
// wrong.
const (
	MinMemory  = 8 * 1024
	MaxMemory  = 1024 * 1024
	MinTime    = 1
	MaxTime    = 16
	MinThreads = 1
	MaxThreads = 16
)

// Errors callers distinguish.
var (
	// ErrNoRecipient means nothing was configured. Its own error
	// because the answer differs from a malformed one: "this deployment
	// cannot take a secrets backup" is a state an operator turns on,
	// and "the line in your config file is wrong" is a fault.
	ErrNoRecipient = errors.New("devseal: no recipient is configured")
	// ErrFormat means the encoded recipient is not this package's.
	ErrFormat = errors.New("devseal: the recipient is not in the expected format")
	// ErrParams means the cost parameters are outside the bounds above.
	ErrParams = errors.New("devseal: the argon2id parameters are outside the allowed range")
	// ErrWrongPassword means the password does not derive this
	// recipient.
	ErrWrongPassword = errors.New("devseal: this password does not match the recipient in the file")
)

// Recipient is the public half: what upgrader.toml carries and what a
// sealed file names.
//
// It is public in the strict sense - it is written in a config file, it
// travels inside every backup, and nothing about it needs hiding. What
// it holds is the salt and cost the private half is derived with, and
// the public key that half produces.
type Recipient struct {
	params Params
	salt   []byte
	pub    *ecdh.PublicKey
}

// IsSet reports whether a recipient is configured.
//
// A zero Recipient seals nothing rather than sealing to a key of
// zeroes, which is the direction that matters.
func (r Recipient) IsSet() bool { return r.pub != nil }

// String renders the recipient in the form a config file carries.
//
// Dot-separated fields, hex, one line, lower case:
//
//	cadev1.<memory>.<time>.<threads>.<salt>.<public key>
//
// Every field visible, because the one time a person reads this is when
// they are comparing the line in a config file against the line in a
// backup's manifest and something does not open. Hex for the reason
// internal/releasesign gives: no case ambiguity and no padding.
//
// The parameters are in it rather than implied by Current so that a
// recipient generated today still opens after the cost is raised.
func (r Recipient) String() string {
	if !r.IsSet() {
		return ""
	}
	return prefix + strconv.FormatUint(uint64(r.params.Memory), 10) +
		"." + strconv.FormatUint(uint64(r.params.Time), 10) +
		"." + strconv.FormatUint(uint64(r.params.Threads), 10) +
		"." + hex.EncodeToString(r.salt) +
		"." + hex.EncodeToString(r.pub.Bytes())
}

// Params are the cost parameters this recipient was made with.
func (r Recipient) Params() Params { return r.params }

// ParseRecipient reads a recipient from its encoded form.
func ParseRecipient(encoded string) (Recipient, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return Recipient{}, ErrNoRecipient
	}
	body, ok := strings.CutPrefix(encoded, prefix)
	if !ok {
		return Recipient{}, fmt.Errorf("%w: it must begin with %q", ErrFormat, prefix)
	}
	fields := strings.Split(body, ".")
	if len(fields) != 5 {
		return Recipient{}, fmt.Errorf("%w: expected 5 dot-separated fields after the "+
			"prefix, got %d", ErrFormat, len(fields))
	}

	memory, err := strconv.ParseUint(fields[0], 10, 32)
	if err != nil {
		return Recipient{}, fmt.Errorf("%w: memory: %v", ErrFormat, err)
	}
	passes, err := strconv.ParseUint(fields[1], 10, 32)
	if err != nil {
		return Recipient{}, fmt.Errorf("%w: time: %v", ErrFormat, err)
	}
	threads, err := strconv.ParseUint(fields[2], 10, 8)
	if err != nil {
		return Recipient{}, fmt.Errorf("%w: threads: %v", ErrFormat, err)
	}
	params := Params{Memory: uint32(memory), Time: uint32(passes), Threads: uint8(threads)}
	if err := params.check(); err != nil {
		return Recipient{}, err
	}

	salt, err := hex.DecodeString(fields[3])
	if err != nil || len(salt) != SaltLen {
		return Recipient{}, fmt.Errorf("%w: the salt must be %d hex characters",
			ErrFormat, hex.EncodedLen(SaltLen))
	}
	raw, err := hex.DecodeString(fields[4])
	if err != nil {
		return Recipient{}, fmt.Errorf("%w: the public key must be %d hex characters",
			ErrFormat, hex.EncodedLen(KeyLen))
	}
	pub, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return Recipient{}, fmt.Errorf("%w: public key: %v", ErrFormat, err)
	}
	return Recipient{params: params, salt: salt, pub: pub}, nil
}

// check bounds the cost parameters. See the constants for why the
// maximum is the half that protects somebody.
func (p Params) check() error {
	switch {
	case p.Memory < MinMemory || p.Memory > MaxMemory:
		return fmt.Errorf("%w: memory is %d KiB, allowed %d-%d",
			ErrParams, p.Memory, MinMemory, MaxMemory)
	case p.Time < MinTime || p.Time > MaxTime:
		return fmt.Errorf("%w: time is %d, allowed %d-%d", ErrParams, p.Time, MinTime, MaxTime)
	case p.Threads < MinThreads || p.Threads > MaxThreads:
		return fmt.Errorf("%w: threads is %d, allowed %d-%d",
			ErrParams, p.Threads, MinThreads, MaxThreads)
	}
	return nil
}

// Identity is the private half. It exists only while somebody is typing
// a password and is never written anywhere.
type Identity struct {
	recipient Recipient
	priv      *ecdh.PrivateKey
}

// Generate derives a new identity from a password, with a fresh salt
// and the current cost.
//
// Used once per deployment, by cmd/devpass, whose output is the
// recipient line for upgrader.toml. The identity itself is discarded
// immediately: nothing keeps it, and nothing can reproduce it without
// the password.
func Generate(password string) (Identity, error) {
	salt := make([]byte, SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return Identity{}, fmt.Errorf("devseal: draw salt: %w", err)
	}
	return derive(password, Current, salt)
}

// Reopen derives the identity a recipient names, from a password.
//
// It refuses a password that does not produce that recipient, rather
// than returning an identity whose only symptom is a file that will not
// open. The difference matters at the moment this is used: somebody is
// recovering a system, and "wrong password" and "wrong file" need
// different next steps.
//
// # Why this is not an oracle worth worrying about
//
// It does confirm a password guess without touching the ciphertext -
// but the recipient is public, and anybody who has it can already do
// this derivation themselves. The check gives away nothing that was not
// already given away by publishing the recipient, and what makes
// guessing expensive is the argon2id cost, which applies either way.
func Reopen(password string, r Recipient) (Identity, error) {
	if !r.IsSet() {
		return Identity{}, ErrNoRecipient
	}
	id, err := derive(password, r.params, r.salt)
	if err != nil {
		return Identity{}, err
	}
	if subtle.ConstantTimeCompare(id.recipient.pub.Bytes(), r.pub.Bytes()) != 1 {
		return Identity{}, ErrWrongPassword
	}
	return id, nil
}

// derive is the one place a password becomes a key.
func derive(password string, params Params, salt []byte) (Identity, error) {
	if password == "" {
		// Refused rather than derived from. An empty password produces
		// a perfectly good key pair, and a deployment sealed to one
		// would be a deployment whose backups anybody can open - with
		// no symptom until somebody tries.
		return Identity{}, errors.New("devseal: the password is empty")
	}
	if err := params.check(); err != nil {
		return Identity{}, err
	}
	if len(salt) != SaltLen {
		return Identity{}, fmt.Errorf("devseal: the salt must be %d bytes, got %d",
			SaltLen, len(salt))
	}

	seed := argon2.IDKey([]byte(password), salt, params.Time, params.Memory, params.Threads, KeyLen)
	priv, err := ecdh.X25519().NewPrivateKey(seed)
	if err != nil {
		return Identity{}, fmt.Errorf("devseal: derive key: %w", err)
	}
	return Identity{
		recipient: Recipient{params: params, salt: salt, pub: priv.PublicKey()},
		priv:      priv,
	}, nil
}

// Recipient is the public half of this identity.
func (i Identity) Recipient() Recipient { return i.recipient }

// IsSet reports whether this identity was derived.
func (i Identity) IsSet() bool { return i.priv != nil }

// Seal encrypts plaintext to a recipient and returns the ephemeral
// public key alongside the box.
//
// Both are needed to open it and neither is secret. The caller writes
// them into the file's header; this function does not know what a file
// looks like, which is what keeps the format decision in one place -
// see internal/secretsfile.
//
// header is authenticated but not encrypted. Everything the opener will
// rely on before it has a key - the salt, the cost, the ephemeral
// public key - belongs in it, so that altering any of them makes the
// box fail to open rather than redirecting the derivation.
func (r Recipient) Seal(header string, plaintext []byte) (ephemeral, box string, err error) {
	if !r.IsSet() {
		return "", "", ErrNoRecipient
	}
	eph, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return "", "", fmt.Errorf("devseal: draw ephemeral key: %w", err)
	}
	key, err := agree(eph, r.pub, eph.PublicKey(), r.pub)
	if err != nil {
		return "", "", err
	}
	box, err = key.Seal(header, string(plaintext))
	if err != nil {
		return "", "", fmt.Errorf("devseal: seal: %w", err)
	}
	// The ephemeral private key goes out of scope here and is never
	// written down. That is the whole of what stops the process that
	// sealed this from opening it again a minute later.
	return hex.EncodeToString(eph.PublicKey().Bytes()), box, nil
}

// Open decrypts a box sealed to this identity's recipient.
func (i Identity) Open(header, ephemeral, box string) ([]byte, error) {
	if !i.IsSet() {
		return nil, ErrNoRecipient
	}
	raw, err := hex.DecodeString(strings.TrimSpace(ephemeral))
	if err != nil {
		return nil, fmt.Errorf("%w: the ephemeral key is not hex", ErrFormat)
	}
	eph, err := ecdh.X25519().NewPublicKey(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: ephemeral key: %v", ErrFormat, err)
	}
	key, err := agree(i.priv, eph, eph, i.recipient.pub)
	if err != nil {
		return nil, err
	}
	plaintext, err := key.Open(header, box)
	if err != nil {
		return nil, err
	}
	return []byte(plaintext), nil
}

// agree turns one side of the exchange into the AEAD key.
//
// Both public keys go into the HKDF salt, in the same order on both
// sides - ephemeral first, recipient second - so the two derivations
// agree. Binding them in is standard and cheap: it means the key is for
// this pair of keys and not merely for this shared secret.
func agree(priv *ecdh.PrivateKey, peer *ecdh.PublicKey, eph, recipient *ecdh.PublicKey) (sealed.Key, error) {
	shared, err := priv.ECDH(peer)
	if err != nil {
		return sealed.Key{}, fmt.Errorf("devseal: key agreement: %w", err)
	}
	salt := make([]byte, 0, 2*KeyLen)
	salt = append(salt, eph.Bytes()...)
	salt = append(salt, recipient.Bytes()...)

	raw, err := hkdf.Key(sha256.New, shared, salt, domain, sealed.KeySize)
	if err != nil {
		return sealed.Key{}, fmt.Errorf("devseal: derive: %w", err)
	}
	// Handed to internal/sealed as hex rather than reimplemented here.
	// One AES-256-GCM in this codebase, and one place where the nonce
	// question is answered - which is the reason that package exists.
	return sealed.ParseKey(hex.EncodeToString(raw))
}
