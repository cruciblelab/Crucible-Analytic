package panel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// TOTP parameters. The defaults every authenticator app assumes: a
// 30-second step, six digits, SHA-1. Not a security choice so much as an
// interoperability one - an app that cannot read the QR code is a
// two-factor feature nobody turns on.
const (
	totpPeriod  = 30
	totpDigits  = otp.DigitsSix
	totpAlgo    = otp.AlgorithmSHA1
	totpIssuer  = "Crucible Analytics"
	totpSkewOne = 1 // accept the neighbouring step on each side
)

// ErrTOTPReplayed is returned when a code is correct but has already
// been used. See VerifyTOTP for why that is refused rather than
// accepted.
var ErrTOTPReplayed = errors.New("panel: that code has already been used")

// ErrTOTPInvalid is returned for a code that does not match any
// acceptable step.
var ErrTOTPInvalid = errors.New("panel: incorrect code")

// NewTOTPSecret generates a secret and the otpauth:// URL an
// authenticator app scans.
func NewTOTPSecret(account string) (*otp.Key, error) {
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      totpIssuer,
		AccountName: account,
		Period:      totpPeriod,
		Digits:      totpDigits,
		Algorithm:   totpAlgo,
	})
	if err != nil {
		return nil, fmt.Errorf("panel: generate totp secret: %w", err)
	}
	return key, nil
}

// checkTOTP validates a code against a secret at a given moment and
// returns the time step that matched.
//
// Each acceptable step is generated and compared explicitly rather than
// calling a validate-with-skew helper, because the *which* matters: the
// matched step is what makes replay protection possible, and a helper
// that only answers yes or no cannot provide it.
//
// The neighbouring steps are accepted because phone clocks drift and
// because a code typed at the very end of its window would otherwise
// fail on arrival. That tolerance is also exactly why replay protection
// is needed: a code stays valid for up to 90 seconds.
func checkTOTP(secret, code string, now time.Time) (int64, bool) {
	code = strings.TrimSpace(code)
	if secret == "" || code == "" {
		return 0, false
	}

	opts := totp.ValidateOpts{Period: totpPeriod, Skew: 0, Digits: totpDigits, Algorithm: totpAlgo}
	for _, offset := range []int64{0, -totpSkewOne, totpSkewOne} {
		at := now.Add(time.Duration(offset) * totpPeriod * time.Second)
		want, err := totp.GenerateCodeCustom(secret, at, opts)
		if err != nil {
			return 0, false
		}
		// A plain comparison is adequate here and a constant-time one
		// would be theatre: the attacker already knows the code they
		// typed, and the six-digit space is defended by throttling, not
		// by comparison timing.
		if want == code {
			return at.Unix() / totpPeriod, true
		}
	}
	return 0, false
}

// VerifyTOTP checks a code for one user and, on success, records the
// step it used so the same code cannot be presented twice.
//
// Refusing replays matters more than it looks. Codes are valid across
// three 30-second steps, so without this a code observed over the
// user's shoulder, in a phishing proxy, or in a screenshot stays usable
// for up to a minute and a half - which is ample for an attacker who
// already has the password and is waiting for exactly that. The check
// and the record are one statement, so two simultaneous submissions of
// the same code cannot both pass.
func (s *Store) VerifyTOTP(ctx context.Context, userID int64, secret, code string, now time.Time) error {
	step, ok := checkTOTP(secret, code, now)
	if !ok {
		return ErrTOTPInvalid
	}

	tag, err := s.pool.Exec(ctx,
		`UPDATE panel_users SET totp_last_step = $2 WHERE id = $1 AND totp_last_step < $2`,
		userID, step)
	if err != nil {
		return fmt.Errorf("panel: record totp step: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// The row exists (the caller just loaded it), so the only way to
		// affect nothing is the step already being at or past this one.
		return ErrTOTPReplayed
	}
	return nil
}
