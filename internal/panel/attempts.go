package panel

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// Login throttling limits. Two independent counters, because they stop
// different attacks: the per-account one stops guessing at one known
// address, and the per-IP one stops spraying one common password across
// many addresses - which the per-account counter would never notice,
// since no single account accumulates failures.
const (
	// LoginThrottleWindow is how far back failures are counted, and so
	// the longest a lock lasts: it lifts by itself once the failures that
	// make it up have left the window. Exported for the pages that have
	// to say "fifteen minutes" and should not type it.
	LoginThrottleWindow = 15 * time.Minute
	// maxFailuresPerEmail is the account's budget inside the window.
	//
	// Five, from the owner's decision (2026-09-28): "three, four or five,
	// you choose". The largest of the three, because the budget is shared
	// by every door into the account - the password, the second-factor
	// code and the recovery code - and is reset only when a sign-in
	// completes (see ClearLoginFailures). Somebody with an authenticator
	// who mistypes their password twice and whose phone's clock is off
	// once is three failures in; with a budget of three they would be
	// waiting fifteen minutes for a mistake that proves nothing.
	//
	// What five gives an attacker: five guesses per account per window,
	// 480 a day. Against a password ValidatePassword accepted, nothing.
	// Against the second factor, for somebody who already has the
	// password, three codes are valid at any moment in a million, so about
	// 0.14% a day - with the account's owner locked out for every minute
	// of it, which is how they find out.
	maxFailuresPerEmail = 5
	// maxFailuresPerIP is higher because one address can legitimately
	// carry several people: an office, or anything behind CGNAT, which
	// is most Turkish mobile traffic.
	maxFailuresPerIP = 30
)

// windowSQL and lockThreshold are the two constants above as SQL text.
//
// Written from the constants rather than typed, so the counter that
// enforces a lock and the page that shows it cannot be counting different
// windows or different budgets. Both are integers formatted by this
// package, never input.
var (
	windowSQL     = fmt.Sprintf("interval '%d seconds'", int(LoginThrottleWindow.Seconds()))
	lockThreshold = strconv.Itoa(maxFailuresPerEmail)
)

// accountFailures is the condition the per-account counter counts: the
// failed attempts on one address inside the window.
//
// One definition for the three places that read it - the sign-in forms,
// which enforce the lock; the members page, which shows it; and
// UnlockLogin, which lifts it. A rule written three times is a rule with
// three chances to disagree, and the dangerous disagreement is the quiet
// one: a page offering to lift a lock the form is not applying, or saying
// nothing about one it is.
func accountFailures(alias, email string) string {
	return alias + `.email = ` + email + ` AND NOT ` + alias + `.success AND ` +
		alias + `.at > now() - ` + windowSQL
}

// accountLock is the per-account lock as one row, for an address
// expression: whether it holds, how many failures make it up, and when it
// lifts by itself.
//
// It lifts when the failure that brought the count to the budget leaves
// the window - the budget-th newest. Blocked attempts are never recorded
// as failures, so waiting out a lock is never lengthened by trying during
// it.
func accountLock(email string) string {
	return `SELECT count(*) >= ` + lockThreshold + ` AS locked,
	               count(*) AS failures,
	               (array_agg(a.at ORDER BY a.at DESC))[` + lockThreshold + `] + ` + windowSQL + ` AS lifts_at
	          FROM panel_login_attempts a
	         WHERE ` + accountFailures("a", email)
}

// liftedWithinWindow is when an owner last lifted this address's lock, if
// that was inside the window, and NULL otherwise.
//
// Read from the audit log, where UnlockLogin writes the lift inside the
// same transaction as the lift itself: the record cannot be missing for a
// lift that happened, and the rule "once per window" cannot be satisfied
// by a lift that left no record.
func liftedWithinWindow(email string) string {
	return `(SELECT max(l.time) FROM panel_audit_log l
	          WHERE l.action = '` + ActionLoginUnlocked + `' AND l.target = ` + email + `
	            AND l.time > now() - ` + windowSQL + `)`
}

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

	var ipArg any
	if ip.IsValid() {
		ipArg = ip
	}

	var emailLocked bool
	var ipFailures int
	err := s.pool.QueryRow(ctx, `
		SELECT
		  (SELECT f.locked FROM (`+accountLock("$1")+`) f),
		  (SELECT count(*) FROM panel_login_attempts a
		     WHERE NOT a.success AND $2::inet IS NOT NULL AND a.ip = $2
		       AND a.at > now() - `+windowSQL+`)`,
		email, ipArg).Scan(&emailLocked, &ipFailures)
	if err != nil {
		return Throttle{}, fmt.Errorf("panel: check login throttle: %w", err)
	}

	switch {
	case emailLocked:
		return Throttle{Blocked: true, RetryAfter: LoginThrottleWindow, Reason: "email"}, nil
	case ipFailures >= maxFailuresPerIP:
		return Throttle{Blocked: true, RetryAfter: LoginThrottleWindow, Reason: "ip"}, nil
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

// ClearLoginFailures forgets an account's failures once a sign-in has
// completed, so somebody who mistyped their password four times and then
// got in starts the next session with a clean slate rather than four
// strikes already against them.
//
// Completed, not half-way. The password step used to call this the
// moment the password was right, and that reset the budget the second
// factor is counted against: with the password in hand, seven wrong codes,
// the password again, seven more - seventy codes checked in 1.6 seconds
// against a limit of eight per fifteen minutes, measured on 2026-09-28. A
// six-digit code falls to that in about two hours. The only caller is now
// the step that establishes the session.
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

// Lifting a lock (catalogue #25).
//
// The owner's decision (2026-09-28): a lock lifts by itself within the
// window, and a site's owner may lift a member's lock sooner. Only the
// per-account lock - the per-address one is somebody's office, not
// somebody's account, and lifting it would be lifting it for whoever else
// is behind that address. Recorded in the audit log.
//
// # Once per window
//
// A lift gives whoever is at the form a fresh budget, and an owner of
// one site may be at the form themselves: somebody who belongs to two
// sites can be locked out by guesses from an owner of the first who wants
// into the second. Unlimited lifts would make the per-account counter
// decoration against exactly that person - five guesses, lift, five more,
// bounded only by the per-address counter and by how many addresses they
// have. One lift per account per window keeps the worst case at twice the
// budget, ten guesses in fifteen minutes, which is about where the old
// budget of eight stood.

var (
	// ErrMayNotLift is a lift asked for by somebody who may not: not an
	// owner of this site by a live membership of their own, or the
	// person whose lock it is.
	ErrMayNotLift = errors.New("panel: only another owner of this site may lift that sign-in lock")
	// ErrNotLocked is a lift of an account whose lock has already gone -
	// lifted by itself, or by another owner a moment earlier.
	ErrNotLocked = errors.New("panel: that account's sign-in is not locked")
	// ErrLiftedRecently is a second lift inside one window.
	ErrLiftedRecently = errors.New("panel: that account's lock was lifted inside the current window")
)

// MayLiftLoginLocks reports whether this access may lift a member's
// sign-in lock: an owner of the site, by a membership of their own.
//
// Superadmin authority is not enough, by the owner's choice - the
// alternative on the table was putting this behind the developer password,
// and it was declined. Role is only ever set from a live membership row
// (AccessFor, liveAccess), so a superadmin without one - a developer
// session included - has none and is refused here. UnlockLogin asks this
// same question of the rows as they stand inside its transaction.
func (a Access) MayLiftLoginLocks() bool {
	return a.Role == RoleOwner
}

// LoginLock is one member's per-account sign-in lock as an owner sees it.
type LoginLock struct {
	UserID   int64
	Email    string
	Failures int
	// LiftsIn is how long until it lifts by itself, by the database's
	// clock - the one the forms count with.
	LiftsIn time.Duration
	// Lifted reports that an owner already lifted this account's lock
	// inside the window, so it cannot be lifted again until it lifts by
	// itself.
	Lifted bool
}

// LoginLocks lists the live members of one site whose sign-in is locked
// right now, keyed by account.
//
// Only locks that hold. A count below the budget is not shown: it is not
// something anybody can act on, and "three failed attempts" beside a
// colleague's name reads as an accusation the panel has no grounds for.
func (s *Store) LoginLocks(ctx context.Context, siteID string) (map[int64]LoginLock, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.email, f.failures,
		       ceil(extract(epoch FROM f.lifts_at - now()))::bigint,
		       `+liftedWithinWindow("u.email")+` IS NOT NULL
		  FROM panel_site_members m
		  JOIN panel_users u ON u.id = m.user_id
		 CROSS JOIN LATERAL (`+accountLock("u.email")+`) f
		 WHERE m.site_id = $1 AND `+liveMembership("m")+` AND f.locked`, siteID)
	if err != nil {
		return nil, fmt.Errorf("panel: read sign-in locks: %w", err)
	}
	defer rows.Close()

	locks := map[int64]LoginLock{}
	for rows.Next() {
		var l LoginLock
		var seconds int64
		if err := rows.Scan(&l.UserID, &l.Email, &l.Failures, &seconds, &l.Lifted); err != nil {
			return nil, fmt.Errorf("panel: scan sign-in lock: %w", err)
		}
		l.LiftsIn = time.Duration(seconds) * time.Second
		locks[l.UserID] = l
	}
	return locks, rows.Err()
}

// Unlocked is what a lift did.
type Unlocked struct {
	Email string
	// Failures is how many failed attempts stopped counting.
	Failures int64
}

// UnlockLogin lifts one member's per-account sign-in lock, as the owner
// of one site they belong to.
//
// Everything is decided inside one transaction, against the rows as they
// stand there: the actor's authority (read again, as every membership
// write does), the target's membership, the lock, and whether it was
// already lifted in this window. The account row is locked first and the
// rest read in later statements, so two owners pressing at once take
// turns and the second one reads the first one's record - which is what
// refuses it.
//
// The lift forgets the failures inside the window, which is what a
// completed sign-in does too, and writes its audit entry inside the same
// transaction: a lift without its record cannot commit.
func (s *Store) UnlockLogin(ctx context.Context, siteID string, actor Principal, userID int64) (Unlocked, error) {
	var done Unlocked
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		// Not your own: you are signed in, so it is not in your way, and
		// lifting it hands whoever caused it a fresh budget.
		if actor.UserID == userID {
			return ErrMayNotLift
		}
		// Read from the account row, so a principal with no account - a
		// developer session, the system - finds none and is refused.
		access, ok := liveAccess(ctx, tx, actor.UserID, siteID)
		if !ok || !access.MayLiftLoginLocks() {
			return ErrMayNotLift
		}

		// The target, by a live membership of this site. An owner of one
		// site has no business with the lock of somebody who does not
		// belong to it, and an expired membership belongs to nothing.
		err := tx.QueryRow(ctx, `
			SELECT u.email FROM panel_users u
			  JOIN panel_site_members m ON m.user_id = u.id
			 WHERE m.site_id = $1 AND u.id = $2 AND `+liveMembership("m")+`
			   FOR UPDATE OF u`, siteID, userID).Scan(&done.Email)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("panel: lock the account being unlocked: %w", err)
		}

		var locked, lifted bool
		if err := tx.QueryRow(ctx, `
			SELECT f.locked, `+liftedWithinWindow("$1")+` IS NOT NULL
			  FROM (`+accountLock("$1")+`) f`, done.Email).Scan(&locked, &lifted); err != nil {
			return fmt.Errorf("panel: read the sign-in lock: %w", err)
		}
		if !locked {
			return ErrNotLocked
		}
		if lifted {
			return ErrLiftedRecently
		}

		tag, err := tx.Exec(ctx,
			`DELETE FROM panel_login_attempts a WHERE `+accountFailures("a", "$1"), done.Email)
		if err != nil {
			return fmt.Errorf("panel: forget the failures: %w", err)
		}
		done.Failures = tag.RowsAffected()

		actorID := actor.UserID
		if _, err := recordIn(ctx, tx, AuditEntry{
			ActorKind: PrincipalUser, ActorID: &actorID, ActorLabel: actor.Label,
			Action: ActionLoginUnlocked, SiteID: siteID, Target: done.Email,
			Detail: map[string]any{"user_id": userID, "failures": done.Failures},
		}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return Unlocked{}, err
	}
	return done, nil
}
