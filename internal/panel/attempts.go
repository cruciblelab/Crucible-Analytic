package panel

import (
	"context"
	"fmt"
	"net/netip"
	"time"
)

// Login throttling limits. Two independent counters, because they stop
// different attacks: the per-account one stops guessing at one known
// address, and the per-IP one stops spraying one common password across
// many addresses - which the per-account counter would never notice,
// since no single account accumulates failures.
const (
	// throttleWindow is how far back failures are counted.
	throttleWindow = 15 * time.Minute
	// maxFailuresPerEmail is generous enough that a person mistyping
	// their own password is not locked out, and small enough that
	// online guessing is hopeless - especially combined with argon2id
	// making each attempt cost tens of milliseconds.
	maxFailuresPerEmail = 8
	// maxFailuresPerIP is higher because one address can legitimately
	// carry several people: an office, or anything behind CGNAT, which
	// is most Turkish mobile traffic.
	maxFailuresPerIP = 30
)

// Throttle is the result of a throttling check.
type Throttle struct {
	// Blocked reports that the attempt should be refused without even
	// checking the password.
	Blocked bool
	// RetryAfter is roughly how long until the oldest counted failure
	// falls out of the window. Approximate on purpose - reporting it
	// exactly would let an attacker time their retries perfectly.
	RetryAfter time.Duration
	// Reason distinguishes which limit fired, for the audit log. Never
	// shown to the user: telling them "this address is blocked" versus
	// "this account is blocked" would confirm whether the account
	// exists.
	Reason string
}

// CheckLoginThrottle counts recent failures for an email and an address.
func (s *Store) CheckLoginThrottle(ctx context.Context, email string, ip netip.Addr) (Throttle, error) {
	email = NormalizeEmail(email)
	window := fmt.Sprintf("%d seconds", int(throttleWindow.Seconds()))

	var ipArg any
	if ip.IsValid() {
		ipArg = ip
	}

	var emailFailures, ipFailures int
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT count(*) FROM panel_login_attempts
		     WHERE NOT success AND email = $1 AND at > now() - $3::interval),
		  (SELECT count(*) FROM panel_login_attempts
		     WHERE NOT success AND $2::inet IS NOT NULL AND ip = $2 AND at > now() - $3::interval)`,
		email, ipArg, window).Scan(&emailFailures, &ipFailures)
	if err != nil {
		return Throttle{}, fmt.Errorf("panel: check login throttle: %w", err)
	}

	switch {
	case emailFailures >= maxFailuresPerEmail:
		return Throttle{Blocked: true, RetryAfter: throttleWindow, Reason: "email"}, nil
	case ipFailures >= maxFailuresPerIP:
		return Throttle{Blocked: true, RetryAfter: throttleWindow, Reason: "ip"}, nil
	}
	return Throttle{}, nil
}

// RecordLoginAttempt appends an attempt.
//
// Failures for addresses that have no account are recorded too. Those
// are the most interesting rows in the table: a run of them is somebody
// working through a list, which is invisible if only real accounts are
// counted.
func (s *Store) RecordLoginAttempt(ctx context.Context, email string, ip netip.Addr, success bool) error {
	var ipArg any
	if ip.IsValid() {
		ipArg = ip
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO panel_login_attempts (email, ip, success) VALUES ($1,$2,$3)`,
		NormalizeEmail(email), ipArg, success); err != nil {
		return fmt.Errorf("panel: record login attempt: %w", err)
	}
	return nil
}

// ClearLoginFailures forgets an account's recent failures after a
// successful login, so somebody who mistyped their password four times
// and then got it right starts the next session with a clean slate
// rather than four strikes already against them.
//
// Only the email's failures, never the address's: clearing those would
// let an attacker reset their own IP counter by successfully logging
// into any one account they do control.
func (s *Store) ClearLoginFailures(ctx context.Context, email string) error {
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM panel_login_attempts WHERE email = $1 AND NOT success`,
		NormalizeEmail(email)); err != nil {
		return fmt.Errorf("panel: clear login failures: %w", err)
	}
	return nil
}

// PurgeOldLoginAttempts trims the table. Kept far longer than the
// throttling window needs, because the rows are a record of who tried
// to get in, which is worth having when something goes wrong.
func (s *Store) PurgeOldLoginAttempts(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM panel_login_attempts WHERE at < now() - interval '90 days'`)
	if err != nil {
		return 0, fmt.Errorf("panel: purge login attempts: %w", err)
	}
	return tag.RowsAffected(), nil
}
