package panel

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Recovery codes are how somebody gets back into their own panel when
// nobody else is awake.
//
// # Why this exists at all
//
// The alternative it replaces is "ring whoever installed it", which is
// fine for one customer and a support queue at thirty - with the person
// queuing locked out of their own analytics at eleven at night. The
// panel had also already promised these: the account page said, in both
// languages, that recovery codes did not exist "yet".
//
// # Why not email
//
// Because email is a configuration burden with a silent failure mode.
// Mail leaving a fresh server without SPF, DKIM and DMARC lands in spam
// or is rejected outright, and a reset that vanishes quietly is the
// worst failure available: the person waits while the panel says "sent".
// A code the customer already has needs no server, no DNS and no
// third party. See PLAN.md, C7.2.
const (
	// RecoveryCodeCount is how many are issued at once.
	//
	// Eight: enough that losing one or two is not an event, few enough
	// that a person will actually write them down. A number so large it
	// gets saved as "somewhere in my downloads" is a number that is
	// gone when it is needed.
	RecoveryCodeCount = 8

	// recoveryCodeChars is the length of one code, before grouping.
	//
	// Twelve characters of the alphabet below is 60 bits. That is far
	// past guessing even without help, and it does not stand alone: this
	// flow shares the sign-in throttle, so an attacker gets a handful of
	// attempts per address rather than a stream.
	recoveryCodeChars = 12

	// recoveryGroup is how many characters go between dashes when the
	// code is shown. Purely presentational - input is normalised before
	// anything is compared.
	recoveryGroup = 4
)

// recoveryAlphabet is RFC 4648 base32 without padding: A-Z and 2-7.
//
// It excludes 0, 1, 8 and 9 by construction, which removes the classic
// handwriting confusions in one direction. The other direction - a
// person typing 0 for O or 1 for I - is handled when the input is
// normalised, because refusing a code over a digit somebody reasonably
// read off paper is a support call this product does not need.
const recoveryAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

// ErrRecoveryInvalid is returned when an address and code do not
// together identify an unused code.
//
// One error for both halves, deliberately. A message that distinguished
// "no such account" from "wrong code" would answer, to anyone on the
// internet, which addresses have accounts here.
var ErrRecoveryInvalid = errors.New("panel: that address and recovery code do not match")

// newRecoveryCode draws one code.
func newRecoveryCode() (string, error) {
	// One byte per character, rejecting the values that would bias the
	// alphabet rather than taking the modulus. 256 is not a multiple of
	// 32 only if the alphabet is not a power of two - it is, so every
	// byte maps evenly and there is nothing to reject. The mask is
	// written out anyway so this stays correct if the alphabet changes.
	const mask = 0x1f // len(recoveryAlphabet) - 1
	if len(recoveryAlphabet) != mask+1 {
		return "", fmt.Errorf("panel: recovery alphabet is %d characters, not %d",
			len(recoveryAlphabet), mask+1)
	}
	raw := make([]byte, recoveryCodeChars)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("panel: draw recovery code: %w", err)
	}
	out := make([]byte, recoveryCodeChars)
	for i, b := range raw {
		out[i] = recoveryAlphabet[b&mask]
	}
	return string(out), nil
}

// FormatRecoveryCode groups a code for reading: ABCD-EFGH-IJKL.
//
// Only ever applied on the way out. Nothing compares a formatted code -
// NormalizeRecoveryCode strips the grouping again on the way in - so the
// separator is a presentation choice and can change without invalidating
// anything already written down.
func FormatRecoveryCode(code string) string {
	var b strings.Builder
	for i, r := range code {
		if i > 0 && i%recoveryGroup == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// NormalizeRecoveryCode turns what somebody typed into what was stored.
//
// Upper-cases, drops anything that is not in the alphabet (so dashes,
// spaces and a stray full stop are all fine), and maps the two digits a
// person most often reads off handwriting: 0 for O and 1 for I. Those
// two digits are not in the alphabet, so the mapping cannot collide with
// a real character and does not shrink the space a guess has to cover.
func NormalizeRecoveryCode(typed string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(typed)) {
		switch r {
		case '0':
			r = 'O'
		case '1':
			r = 'I'
		}
		if strings.ContainsRune(recoveryAlphabet, r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// GenerateRecoveryCodes replaces every unused code a user has with a
// fresh set, and returns the new codes in the clear - the only time they
// exist in readable form.
//
// Replaces rather than adds: two live sets would mean a code somebody
// believes they revoked still opens the account. by is who issued them,
// zero when the account minted its own at creation.
func (s *Store) GenerateRecoveryCodes(ctx context.Context, userID, by int64) ([]string, error) {
	codes := make([]string, 0, RecoveryCodeCount)
	for range RecoveryCodeCount {
		code, err := newRecoveryCode()
		if err != nil {
			return nil, err
		}
		codes = append(codes, code)
	}
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		return storeRecoveryCodes(ctx, tx, userID, by, codes)
	})
	if err != nil {
		return nil, err
	}
	return codes, nil
}

// storeRecoveryCodes writes a set inside an existing transaction, so an
// account and its codes can be created together or not at all.
func storeRecoveryCodes(ctx context.Context, tx pgx.Tx, userID, by int64, codes []string) error {
	if _, err := tx.Exec(ctx,
		`DELETE FROM panel_recovery_codes WHERE user_id = $1 AND used_at IS NULL`,
		userID); err != nil {
		return fmt.Errorf("panel: clear old recovery codes: %w", err)
	}
	var issuer any
	if by != 0 {
		issuer = by
	}
	for _, code := range codes {
		if _, err := tx.Exec(ctx, `
			INSERT INTO panel_recovery_codes (user_id, sha256, created_by)
			VALUES ($1, $2, $3)`,
			userID, hashToken(code), issuer); err != nil {
			return fmt.Errorf("panel: store recovery code: %w", err)
		}
	}
	return nil
}

// CountRecoveryCodes is how many unused codes a user has left, for the
// account page to show. Somebody down to their last code should be told
// so before it is the last thing standing between them and a support
// call.
func (s *Store) CountRecoveryCodes(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM panel_recovery_codes WHERE user_id = $1 AND used_at IS NULL`,
		userID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("panel: count recovery codes: %w", err)
	}
	return n, nil
}

// RecoveryResult is what a successful redemption did.
type RecoveryResult struct {
	User User
	// Remaining is how many codes are left afterwards, so the page can
	// say it rather than making the reader count.
	Remaining int
	// SecondFactorCleared reports whether the account's TOTP secret was
	// removed as part of this reset.
	SecondFactorCleared bool
}

// UseRecoveryCode consumes one code and sets a new password.
//
// # The second factor is kept unless asked for
//
// clearSecondFactor exists because "I forgot my password" and "I lost my
// phone" arrive at the same page and are not the same request. Clearing
// the second factor by default would quietly downgrade every account
// that ever reset a password, and a recovery code is already enough to
// take an account over - it does not also need to strip the thing
// standing behind it.
//
// # Why the address is checked second
//
// The code is looked up by digest alone and the address compared after.
// A query that filtered on the address would do less work for an address
// that does not exist, and the difference is measurable from outside:
// it would answer, to anyone, which addresses have accounts here.
func (s *Store) UseRecoveryCode(ctx context.Context, email, typed, newPasswordHash string,
	clearSecondFactor bool, from netip.Addr) (RecoveryResult, error) {

	code := NormalizeRecoveryCode(typed)
	if email == "" || code == "" || newPasswordHash == "" {
		return RecoveryResult{}, ErrRecoveryInvalid
	}

	var result RecoveryResult
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// Consume first, by digest only. A row another request has
		// already taken does not match.
		var userID int64
		err := tx.QueryRow(ctx, `
			UPDATE panel_recovery_codes
			   SET used_at = now(), used_from = $2
			 WHERE sha256 = $1 AND used_at IS NULL
			RETURNING user_id`,
			hashToken(code), addrOrNull(from),
		).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrRecoveryInvalid
		}
		if err != nil {
			return fmt.Errorf("panel: consume recovery code: %w", err)
		}

		// The code was real. Whose is it, and did they say so?
		user, err := scanUser(tx.QueryRow(ctx,
			`SELECT `+userColumns+` FROM panel_users WHERE id = $1`, userID))
		if err != nil {
			return fmt.Errorf("panel: read the account a recovery code belongs to: %w", err)
		}
		if !strings.EqualFold(strings.TrimSpace(email), user.Email) {
			// Somebody holding a valid code for one account cannot use
			// it against another by naming a different address. The
			// consumption above is rolled back with this error, so a
			// mistyped address does not burn the code.
			return ErrRecoveryInvalid
		}
		if user.Disabled {
			// A disabled account is disabled. Returning the same error
			// keeps that fact off the internet as well.
			return ErrRecoveryInvalid
		}

		set := `password_hash = $2`
		if clearSecondFactor {
			set += `, totp_secret = '', totp_last_step = 0`
		}
		user, err = scanUser(tx.QueryRow(ctx,
			`UPDATE panel_users SET `+set+` WHERE id = $1 RETURNING `+userColumns,
			userID, newPasswordHash))
		if err != nil {
			return fmt.Errorf("panel: set the new password: %w", err)
		}

		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM panel_recovery_codes WHERE user_id = $1 AND used_at IS NULL`,
			userID).Scan(&result.Remaining); err != nil {
			return fmt.Errorf("panel: count remaining recovery codes: %w", err)
		}
		result.User = user
		result.SecondFactorCleared = clearSecondFactor
		return nil
	})
	if err != nil {
		return RecoveryResult{}, err
	}
	return result, nil
}
