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

// Inviting a colleague: the gap the panel has been admitting to.
//
// Adding a member has always required an account that already existed,
// and the page said so in as many words - "davet e-postası henüz
// eklenmedi". So the only way anybody got an account was an owner claim
// minted at a shell, which is right for the person who owns the
// deployment and absurd for the third person in a marketing team.
//
// # Why this is its own table and its own token, and why neither is new
//
// The shape is panel_owner_claims': a row, a SHA-256, an expiry, a
// single atomic redemption. That pattern is written four times in this
// schema already (API tokens, developer access, owner claims, recovery
// codes) and a fifth spelling of it would be a fifth thing to get
// wrong. What differs is what is being granted - one site, one role,
// decided by somebody who already has authority over that site.
//
// # What the invitee cannot do
//
// Choose their role, their site, or their address. All three live on the
// row and are read from it at redemption; nothing the accepting form
// sends is consulted for any of them. The one thing they choose is their
// password, and their display name.
//
// *İstemciye güvenme, sadece sunucuya güven.*
//
// # The hole this closes that a first draft would not have
//
// Authority is asked for twice: when the invitation is minted, and again
// when it is used. Without the second, an admin mints an owner
// invitation, is demoted, and the link still produces an owner - a
// privilege escalation that survives the demotion that was supposed to
// end it. See RedeemMemberInvite, and RevokeMemberInvitesBy for the
// other half.

// DefaultMemberInviteTTL is how long an invitation stays usable.
//
// A week, the same as an owner claim and for the same reason: this link
// travels by mail or by message to somebody who is not sitting at a
// terminal waiting for it, and it has to survive a weekend without
// becoming a standing key.
const DefaultMemberInviteTTL = 7 * 24 * time.Hour

// ErrInviteInvalid covers every way redeeming can fail: unknown,
// expired, withdrawn, already used, or minted by somebody who has since
// lost the authority to have minted it.
//
// One error for all of them, like ErrClaimInvalid: telling a guesser
// that a guess was once real is telling them something.
var ErrInviteInvalid = errors.New("panel: that invitation link is not valid")

// MemberInvite is one invitation to one site.
type MemberInvite struct {
	ID           int64
	SiteID       string
	Role         Role
	Email        string
	CreatedAt    time.Time
	CreatedLabel string
	ExpiresAt    time.Time
	RevokedAt    *time.Time
	UsedAt       *time.Time
}

// Open reports whether this invitation can still be accepted.
func (i MemberInvite) Open() bool {
	return i.UsedAt == nil && i.RevokedAt == nil && time.Now().Before(i.ExpiresAt)
}

const memberInviteColumns = `id, site_id, role, email, created_at, created_label,
	expires_at, revoked_at, used_at`

func scanMemberInvite(row pgx.Row) (MemberInvite, error) {
	var i MemberInvite
	err := row.Scan(&i.ID, &i.SiteID, &i.Role, &i.Email, &i.CreatedAt,
		&i.CreatedLabel, &i.ExpiresAt, &i.RevokedAt, &i.UsedAt)
	return i, err
}

// CreateMemberInvite mints an invitation and returns the raw token.
//
// The token is returned once and never again: only its SHA-256 is
// stored. Losing it means minting another, which the members page offers
// beside the invitation it replaces.
//
// The caller has already been asked whether it may assign this role -
// see Access.CanAssign - and RedeemMemberInvite asks again.
func (s *Store) CreateMemberInvite(ctx context.Context, siteID string, email string,
	role Role, by Principal, ttl time.Duration) (string, MemberInvite, error) {

	if siteID == "" {
		return "", MemberInvite{}, errors.New("panel: an invitation needs a site")
	}
	if !role.Valid() {
		return "", MemberInvite{}, fmt.Errorf("panel: %q is not a role", role)
	}
	email = NormalizeEmail(email)
	if email == "" {
		return "", MemberInvite{}, errors.New("panel: an invitation needs an email address")
	}
	if ttl <= 0 {
		ttl = DefaultMemberInviteTTL
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", MemberInvite{}, fmt.Errorf("panel: draw invitation token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	var actorID any
	if by.UserID != 0 {
		actorID = by.UserID
	}

	invite, err := scanMemberInvite(s.pool.QueryRow(ctx, `
		INSERT INTO panel_member_invites
		  (sha256, site_id, role, email, created_by, created_label, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, now() + $7::interval)
		RETURNING `+memberInviteColumns,
		hashToken(token), siteID, string(role), email, actorID, by.Label,
		fmt.Sprintf("%d seconds", int(ttl.Seconds()))))
	if err != nil {
		return "", MemberInvite{}, fmt.Errorf("panel: create invitation: %w", err)
	}
	return token, invite, nil
}

// LookupMemberInvite reads an open invitation by its raw token.
//
// Read-only, for rendering the page that asks for a password. It proves
// nothing on its own - RedeemMemberInvite re-checks everything inside
// the transaction that consumes it - because a check here and a write
// later is a gap two tabs can both fit through.
func (s *Store) LookupMemberInvite(ctx context.Context, token string) (MemberInvite, error) {
	if token == "" {
		return MemberInvite{}, ErrInviteInvalid
	}
	invite, err := scanMemberInvite(s.pool.QueryRow(ctx, `
		SELECT `+memberInviteColumns+`
		  FROM panel_member_invites
		 WHERE sha256 = $1`, hashToken(token)))
	if errors.Is(err, pgx.ErrNoRows) {
		return MemberInvite{}, ErrInviteInvalid
	}
	if err != nil {
		return MemberInvite{}, fmt.Errorf("panel: read invitation: %w", err)
	}
	if !invite.Open() {
		return MemberInvite{}, ErrInviteInvalid
	}
	return invite, nil
}

// RedeemMemberInvite turns an invitation into an account and a
// membership, in one transaction.
//
// # The order, and why it is this order
//
// Consume first. A row another transaction has already taken does not
// match the UPDATE, so the second caller gets no rows and stops before
// creating anything - which is how two tabs opened at once produce one
// member rather than two.
//
// # Authority is asked again here
//
// The role on the row was checked when it was minted. That was true
// then. Between then and now the person who minted it may have been
// demoted or removed, and an invitation that still grants what they
// could no longer grant is a privilege escalation that outlives the
// demotion. So the inviter's authority over this site is re-read inside
// the transaction, and an invitation minted by somebody who has lost it
// is refused.
//
// The one exception is an inviter whose account is gone entirely
// (created_by is NULL after ON DELETE SET NULL). Refused as well: an
// invitation nobody can be asked about is an invitation nobody
// authorised.
func (s *Store) RedeemMemberInvite(ctx context.Context, token, displayName, passwordHash string,
	from netip.Addr) (User, MemberInvite, error) {

	if token == "" || passwordHash == "" {
		return User{}, MemberInvite{}, ErrInviteInvalid
	}

	var (
		user   User
		invite MemberInvite
	)
	err := s.inTx(ctx, func(tx pgx.Tx) error {
		var inviterID *int64
		err := tx.QueryRow(ctx, `
			UPDATE panel_member_invites
			   SET used_at = now(), used_from = $2
			 WHERE sha256 = $1
			   AND used_at IS NULL AND revoked_at IS NULL AND expires_at > now()
			RETURNING `+memberInviteColumns+`, created_by`,
			hashToken(token), addrOrNull(from),
		).Scan(&invite.ID, &invite.SiteID, &invite.Role, &invite.Email, &invite.CreatedAt,
			&invite.CreatedLabel, &invite.ExpiresAt, &invite.RevokedAt, &invite.UsedAt,
			&inviterID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInviteInvalid
		}
		if err != nil {
			return fmt.Errorf("panel: consume invitation: %w", err)
		}

		if !inviterMayStillGrant(ctx, tx, inviterID, invite.SiteID, invite.Role) {
			// Rolled back with the rest of the transaction, so the
			// invitation is not consumed by a refusal - somebody can
			// restore the inviter's role and the link still works.
			return ErrInviteInvalid
		}

		// The account, or the one that already exists. Both are ordinary:
		// an invitation to a colleague who already signs in here is a
		// grant of one more site, and there is nothing to create.
		// scanUser turns "no rows" into ErrNotFound, so that is what is
		// asked for here. Reading pgx.ErrNoRows instead would never
		// match and every redemption would fail as an unexpected error -
		// measured, by writing it that way first.
		user, err = scanUser(tx.QueryRow(ctx,
			`SELECT `+userColumns+` FROM panel_users WHERE email = $1`, invite.Email))
		if errors.Is(err, ErrNotFound) {
			user, err = scanUser(tx.QueryRow(ctx, `
				INSERT INTO panel_users (email, display_name, password_hash, is_superadmin)
				VALUES ($1, $2, $3, FALSE)
				RETURNING `+userColumns,
				invite.Email, displayName, passwordHash))
			if err != nil {
				return fmt.Errorf("panel: create member: %w", err)
			}
		} else if err != nil {
			return fmt.Errorf("panel: look up invited address: %w", err)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO panel_site_members (site_id, user_id, role, created_by)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (site_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
			invite.SiteID, user.ID, string(invite.Role), inviterID); err != nil {
			return fmt.Errorf("panel: grant membership: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`UPDATE panel_member_invites SET used_by = $2 WHERE id = $1`,
			invite.ID, user.ID); err != nil {
			return fmt.Errorf("panel: record invitation use: %w", err)
		}
		return nil
	})
	if err != nil {
		return User{}, MemberInvite{}, err
	}
	return user, invite, nil
}

// inviterMayStillGrant answers whether the person who minted an
// invitation could mint it again today.
//
// Read inside the redeeming transaction, against the same rule the
// members page uses: a superadmin may assign anything, and a member may
// assign what their own role allows.
func inviterMayStillGrant(ctx context.Context, tx pgx.Tx, inviterID *int64, siteID string, role Role) bool {
	if inviterID == nil {
		return false
	}
	var superadmin bool
	var current *string
	err := tx.QueryRow(ctx, `
		SELECT u.is_superadmin,
		       (SELECT m.role FROM panel_site_members m
		         WHERE m.user_id = u.id AND m.site_id = $2)
		  FROM panel_users u
		 WHERE u.id = $1 AND NOT u.disabled`, *inviterID, siteID).
		Scan(&superadmin, &current)
	if err != nil {
		return false
	}
	access := Access{Principal: Principal{UserID: *inviterID, Superadmin: superadmin}, SiteID: siteID}
	if current != nil {
		access.Role = Role(*current)
		access.Member = true
	}
	return access.CanAssign(role)
}

// OpenMemberInvites lists the invitations for one site that nobody has
// accepted yet.
//
// Shown under the members themselves so a second invitation to the same
// address is a deliberate choice rather than a surprise, and so
// withdrawing one is possible without a shell.
func (s *Store) OpenMemberInvites(ctx context.Context, siteID string) ([]MemberInvite, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+memberInviteColumns+`
		  FROM panel_member_invites
		 WHERE site_id = $1
		   AND used_at IS NULL AND revoked_at IS NULL AND expires_at > now()
		 ORDER BY created_at DESC`, siteID)
	if err != nil {
		return nil, fmt.Errorf("panel: list invitations: %w", err)
	}
	defer rows.Close()

	var out []MemberInvite
	for rows.Next() {
		invite, err := scanMemberInvite(rows)
		if err != nil {
			return nil, fmt.Errorf("panel: read invitation: %w", err)
		}
		out = append(out, invite)
	}
	return out, rows.Err()
}

// RevokeMemberInvite withdraws one invitation.
//
// Marked rather than deleted: "this was withdrawn on that date" is a
// fact somebody may need, and a row that vanishes cannot state it.
func (s *Store) RevokeMemberInvite(ctx context.Context, id int64, siteID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE panel_member_invites SET revoked_at = now()
		 WHERE id = $1 AND site_id = $2 AND used_at IS NULL AND revoked_at IS NULL`,
		id, siteID)
	if err != nil {
		return fmt.Errorf("panel: withdraw invitation: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeMemberInvitesBy withdraws every open invitation one person
// minted for one site.
//
// Called when they are demoted or removed. Redemption re-checks the
// inviter's authority anyway, so this is not what makes the rule true -
// it is what makes it visible. Without it the members page would keep
// listing invitations that are already dead, and the invitee would find
// out by clicking.
func (s *Store) RevokeMemberInvitesBy(ctx context.Context, userID int64, siteID string) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE panel_member_invites SET revoked_at = now()
		 WHERE created_by = $1 AND site_id = $2
		   AND used_at IS NULL AND revoked_at IS NULL`, userID, siteID)
	if err != nil {
		return 0, fmt.Errorf("panel: withdraw invitations: %w", err)
	}
	return tag.RowsAffected(), nil
}
