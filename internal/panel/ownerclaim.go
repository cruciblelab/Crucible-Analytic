package panel

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
)

// Handover: turning a finished technical installation into an account
// somebody owns.
//
// Until this exists there is no way to create the first account at all.
// The technical wizard does not make one, the developer link does not
// make one, and the sign-in form cannot sign anybody into an account
// that was never created. This is the missing link in that chain, and
// it is deliberately the only one: every owner arrives through an
// invitation somebody with a shell or a finished wizard minted.
//
// The invitation is a row, not a user. A user with no usable password
// and a flag saying "not yet claimed" is two pieces of state that have
// to agree, and the failure when they disagree is either an account
// nobody can sign in to or an account anybody can. An invitation that
// has not been accepted is not a user; it is an invitation.

// DefaultOwnerClaimTTL is how long an invitation stays usable.
//
// A week rather than hours: this link is handed over by phone, by
// message, or on a piece of paper, and the person receiving it may be
// the customer rather than an administrator waiting at a terminal. Long
// enough to survive a weekend, short enough that a forgotten one is not
// a standing key to the deployment.
const DefaultOwnerClaimTTL = 7 * 24 * time.Hour

// ErrClaimInvalid covers every way claiming can fail: unknown, expired,
// or already used.
//
// One error for all of them, for the same reason the developer links
// have one: distinguishing "this link expired" from "this link never
// existed" tells anybody guessing that a guess was once real.
var ErrClaimInvalid = errors.New("panel: that invitation link is not valid")

// OwnerClaim is one invitation.
type OwnerClaim struct {
	ID           int64
	Email        string
	DisplayName  string
	CreatedAt    time.Time
	CreatedLabel string
	ExpiresAt    time.Time
	UsedAt       *time.Time
}

// Open reports whether this invitation can still be accepted.
func (c OwnerClaim) Open() bool {
	return c.UsedAt == nil && time.Now().Before(c.ExpiresAt)
}

// CreateOwnerClaim mints an invitation and returns the raw token.
//
// The token is returned once and never again: only its SHA-256 is
// stored, so whoever can read this table cannot thereby use what is in
// it. Losing it means minting another, which is what
// `panel -owner-link` is for.
func (s *Store) CreateOwnerClaim(ctx context.Context, email, displayName string, by Principal, ttl time.Duration) (string, OwnerClaim, error) {
	email = NormalizeEmail(email)
	if email == "" {
		return "", OwnerClaim{}, errors.New("panel: an invitation needs an email address")
	}
	if ttl <= 0 {
		ttl = DefaultOwnerClaimTTL
	}

	// Refused early when the address already has an account. Not a
	// security property - the transaction below would fail on the unique
	// index anyway - but the person handing over deserves to find out
	// now rather than after passing a link to somebody it cannot help.
	if _, err := s.UserByEmail(ctx, email); err == nil {
		return "", OwnerClaim{}, ErrEmailTaken
	} else if !errors.Is(err, ErrNotFound) {
		return "", OwnerClaim{}, err
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", OwnerClaim{}, fmt.Errorf("panel: draw invitation token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	var actorID any
	if by.UserID != 0 {
		actorID = by.UserID
	}

	var claim OwnerClaim
	err := s.pool.QueryRow(ctx, `
		INSERT INTO panel_owner_claims
		  (sha256, email, display_name, created_by, created_label, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + $6::interval)
		RETURNING id, email, display_name, created_at, created_label, expires_at, used_at`,
		hashToken(token), email, displayName, actorID, by.Label,
		fmt.Sprintf("%d seconds", int(ttl.Seconds())),
	).Scan(&claim.ID, &claim.Email, &claim.DisplayName, &claim.CreatedAt,
		&claim.CreatedLabel, &claim.ExpiresAt, &claim.UsedAt)
	if err != nil {
		return "", OwnerClaim{}, fmt.Errorf("panel: create invitation: %w", err)
	}
	return token, claim, nil
}

// LookupOwnerClaim reads an open invitation by its raw token.
//
// Read-only, for rendering the page that asks for a password. It proves
// nothing on its own - RedeemOwnerClaim re-checks everything inside the
// transaction that consumes it - because a check here and a write later
// is a gap two tabs can both fit through.
func (s *Store) LookupOwnerClaim(ctx context.Context, token string) (OwnerClaim, error) {
	if token == "" {
		return OwnerClaim{}, ErrClaimInvalid
	}
	var claim OwnerClaim
	err := s.pool.QueryRow(ctx, `
		SELECT id, email, display_name, created_at, created_label, expires_at, used_at
		  FROM panel_owner_claims
		 WHERE sha256 = $1 AND used_at IS NULL AND expires_at > now()`,
		hashToken(token),
	).Scan(&claim.ID, &claim.Email, &claim.DisplayName, &claim.CreatedAt,
		&claim.CreatedLabel, &claim.ExpiresAt, &claim.UsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OwnerClaim{}, ErrClaimInvalid
	}
	if err != nil {
		return OwnerClaim{}, fmt.Errorf("panel: look up invitation: %w", err)
	}
	return claim, nil
}

// RedeemOwnerClaim accepts an invitation: it creates the account, makes
// it an owner of every configured site, and consumes the invitation -
// all in one transaction.
//
// One transaction because each half is useless without the other. A
// consumed invitation with no account is a customer locked out with no
// second chance; an account with no owner membership is a customer
// signed in to a panel that shows them nothing; and an account created
// twice from two tabs is two owners where the invitation promised one.
//
// The consuming UPDATE carries `used_at IS NULL` in its WHERE clause and
// runs first, so the race is decided by the database rather than by the
// order two requests happen to arrive in.
func (s *Store) RedeemOwnerClaim(ctx context.Context, token, passwordHash string, sites []string, from netip.Addr) (User, error) {
	if token == "" || passwordHash == "" {
		return User{}, ErrClaimInvalid
	}

	var user User
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var claimID int64
		var email, displayName string
		// Consume first. A row that another transaction has already
		// taken does not match, so the second caller gets no rows and
		// stops here rather than creating a second owner.
		err := tx.QueryRow(ctx, `
			UPDATE panel_owner_claims
			   SET used_at = now(), used_from = $2
			 WHERE sha256 = $1 AND used_at IS NULL AND expires_at > now()
			RETURNING id, email, display_name`,
			hashToken(token), addrOrNull(from),
		).Scan(&claimID, &email, &displayName)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrClaimInvalid
		}
		if err != nil {
			return fmt.Errorf("panel: consume invitation: %w", err)
		}

		// The account. Not a superadmin: owning a site and running the
		// deployment are different jobs, and the customer is the first
		// of those. A superadmin is created deliberately, by somebody
		// with a shell, and never by accepting an invitation.
		user, err = scanUser(tx.QueryRow(ctx, `
			INSERT INTO panel_users (email, display_name, password_hash, is_superadmin)
			VALUES ($1, $2, $3, FALSE)
			RETURNING `+userColumns,
			email, displayName, passwordHash))
		if err != nil {
			return fmt.Errorf("panel: create owner: %w", err)
		}

		// Owner on every site the deployment is configured for. Without
		// this the customer signs in to an empty panel and has no way to
		// give themselves access to their own data.
		for _, site := range sites {
			if site == "" {
				continue
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO panel_site_members (site_id, user_id, role, created_by)
				VALUES ($1, $2, 'owner', NULL)
				ON CONFLICT (site_id, user_id) DO UPDATE SET role = 'owner'`,
				site, user.ID); err != nil {
				return fmt.Errorf("panel: grant ownership of %q: %w", site, err)
			}
		}

		if _, err := tx.Exec(ctx,
			`UPDATE panel_owner_claims SET used_by = $2 WHERE id = $1`,
			claimID, user.ID); err != nil {
			return fmt.Errorf("panel: record invitation use: %w", err)
		}
		return nil
	})
	if err != nil {
		return User{}, err
	}
	return user, nil
}

// OpenOwnerClaims lists invitations nobody has accepted yet.
//
// The handover page shows these so a second link is a deliberate choice
// rather than an accident: minting one every time somebody reloads the
// page would leave several live invitations to the same deployment, each
// of which creates an owner.
func (s *Store) OpenOwnerClaims(ctx context.Context) ([]OwnerClaim, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, email, display_name, created_at, created_label, expires_at, used_at
		  FROM panel_owner_claims
		 WHERE used_at IS NULL AND expires_at > now()
		 ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("panel: list invitations: %w", err)
	}
	defer rows.Close()

	claims := []OwnerClaim{}
	for rows.Next() {
		var c OwnerClaim
		if err := rows.Scan(&c.ID, &c.Email, &c.DisplayName, &c.CreatedAt,
			&c.CreatedLabel, &c.ExpiresAt, &c.UsedAt); err != nil {
			return nil, fmt.Errorf("panel: scan invitation: %w", err)
		}
		claims = append(claims, c)
	}
	return claims, rows.Err()
}

// RevokeOwnerClaim withdraws an invitation nobody has accepted.
//
// Expiring it rather than deleting the row: the fact that an invitation
// was issued and then withdrawn is exactly the kind of thing somebody
// reads an audit trail to find out.
func (s *Store) RevokeOwnerClaim(ctx context.Context, id int64) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE panel_owner_claims SET expires_at = now()
		  WHERE id = $1 AND used_at IS NULL AND expires_at > now()`, id)
	if err != nil {
		return fmt.Errorf("panel: revoke invitation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// addrOrNull turns an invalid address into a SQL NULL.
//
// An invalid netip.Addr is what a request with no usable peer address
// produces, and writing its zero value would record the claim as having
// come from "invalid IP" - a fact that is not true and that a reader
// would spend time on.
func addrOrNull(a netip.Addr) any {
	if a.IsValid() {
		return a.String()
	}
	return nil
}
