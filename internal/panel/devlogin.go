package panel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
)

// DefaultDevLoginTTL is how long a minted developer link stays usable.
//
// Fifteen minutes: long enough to paste into a browser, short enough
// that a link left in shell history or a scrollback buffer is dead by
// the time anyone finds it.
const DefaultDevLoginTTL = 15 * time.Minute

// ErrDevLoginInvalid covers every way a developer link can fail to
// redeem - unknown, expired, or already used.
//
// One error rather than three on purpose: the three are indistinguishable
// to a legitimate user (all mean "get a new link") and telling them apart
// would confirm to an attacker that a guessed token had once been real.
var ErrDevLoginInvalid = errors.New("panel: developer login link is invalid, expired, or already used")

// MintDevLogin creates a single-use developer login and returns the raw
// token, which is not recoverable afterwards - only its hash is stored,
// the same rule the API tokens follow.
//
// This is deliberately not reachable from the web UI. It is minted by
// running a command on the server, and that is the entire authorization
// model: someone with shell access can already read the config file and
// the database password, so this grants nothing new. What it adds is a
// way in that expires, cannot be reused, and is filed under a visibly
// separate identity in the audit log - all of which a shared
// "developer" account would not be.
func (s *Store) MintDevLogin(ctx context.Context, label string, ttl time.Duration) (string, error) {
	if ttl <= 0 {
		ttl = DefaultDevLoginTTL
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("panel: draw developer login token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO panel_dev_logins (sha256, label, expires_at)
		VALUES ($1, $2, now() + $3::interval)`,
		hashToken(token), label, fmt.Sprintf("%d seconds", int(ttl.Seconds()))); err != nil {
		return "", fmt.Errorf("panel: store developer login: %w", err)
	}
	return token, nil
}

// RedeemDevLogin consumes a developer link, returning its label.
//
// The check and the consumption are one statement. Reading the row,
// deciding it is unused, and then marking it used would let two
// simultaneous requests with the same token both pass - the classic
// single-use race - and this is a token that grants operator authority,
// so "usually single-use" is not good enough. The UPDATE's own WHERE
// clause does the checking, so whichever request commits first is the
// only one that gets a row back.
func (s *Store) RedeemDevLogin(ctx context.Context, token string, from netip.Addr) (string, error) {
	var (
		id    int64
		label string
	)
	var fromArg any
	if from.IsValid() {
		fromArg = from
	}

	err := s.pool.QueryRow(ctx, `
		UPDATE panel_dev_logins
		SET used_at = now(), used_from = $2
		WHERE sha256 = $1 AND used_at IS NULL AND expires_at > now()
		RETURNING id, label`,
		hashToken(token), fromArg).Scan(&id, &label)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrDevLoginInvalid
	}
	if err != nil {
		return "", fmt.Errorf("panel: redeem developer login: %w", err)
	}
	return label, nil
}

// PurgeExpiredDevLogins deletes links that expired more than a day ago.
//
// Not immediately on expiry: a recently expired row is evidence that
// somebody tried to use a stale link, and keeping it for a day means
// that shows up in an investigation rather than vanishing.
func (s *Store) PurgeExpiredDevLogins(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM panel_dev_logins WHERE expires_at < now() - interval '1 day'`)
	if err != nil {
		return 0, fmt.Errorf("panel: purge developer logins: %w", err)
	}
	return tag.RowsAffected(), nil
}

// hashToken returns the hex-encoded SHA-256 of a raw token - the same
// storage form api.HashToken uses, so the two credential types are
// handled identically and neither is ever stored in the clear.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
