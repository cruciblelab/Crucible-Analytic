package panel

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Member is one user's membership of one site.
type Member struct {
	SiteID    string
	UserID    int64
	Role      Role
	Email     string
	Name      string
	Disabled  bool
	CreatedAt time.Time
	CreatedBy *int64
}

// SiteAccess is a site as it appears in one user's own site list.
type SiteAccess struct {
	SiteID string
	Role   Role
	// ViaSuperadmin marks a site reachable because the principal is the
	// operator rather than because anyone granted them a membership.
	// Surfaced in the UI so it is obvious when you are looking at a
	// customer's data as staff.
	ViaSuperadmin bool
}

// Sites lists what a principal may see.
//
// A superadmin gets the union of every site anyone has a membership for
// and every site passed in as known - the caller supplies the latter
// from the analytics API, because a site can be collecting data before
// anyone has been given a membership on it, and the operator needs to
// see that it exists in order to grant one.
func (s *Store) Sites(ctx context.Context, p Principal, known []string) ([]SiteAccess, error) {
	if p.Superadmin {
		return s.allSites(ctx, known)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT site_id, role FROM panel_site_members
		WHERE user_id = $1 ORDER BY site_id`, p.UserID)
	if err != nil {
		return nil, fmt.Errorf("panel: list sites: %w", err)
	}
	defer rows.Close()

	sites := []SiteAccess{}
	for rows.Next() {
		var sa SiteAccess
		if err := rows.Scan(&sa.SiteID, &sa.Role); err != nil {
			return nil, fmt.Errorf("panel: scan site access: %w", err)
		}
		sites = append(sites, sa)
	}
	return sites, rows.Err()
}

func (s *Store) allSites(ctx context.Context, known []string) ([]SiteAccess, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT site_id FROM panel_site_members ORDER BY site_id`)
	if err != nil {
		return nil, fmt.Errorf("panel: list all sites: %w", err)
	}
	defer rows.Close()

	seen := map[string]bool{}
	sites := []SiteAccess{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("panel: scan site: %w", err)
		}
		seen[id] = true
		sites = append(sites, SiteAccess{SiteID: id, ViaSuperadmin: true})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Sites that have data but no members yet. Without these the
	// operator could never grant the first membership on a new site,
	// because they could not see that it existed.
	for _, id := range known {
		if id != "" && !seen[id] {
			sites = append(sites, SiteAccess{SiteID: id, ViaSuperadmin: true})
		}
	}
	return sites, nil
}

// AccessFor resolves what a principal may do on one specific site.
//
// This is the single choke point every per-site handler goes through -
// see Server.siteHandler - so no handler can reach a site's data without
// an authorization decision having been made about it.
func (s *Store) AccessFor(ctx context.Context, p Principal, siteID string) (Access, error) {
	access := Access{Principal: p, SiteID: siteID}

	var role Role
	err := s.pool.QueryRow(ctx,
		`SELECT role FROM panel_site_members WHERE site_id = $1 AND user_id = $2`,
		siteID, p.UserID).Scan(&role)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// No membership. For a superadmin that is normal and access
		// still follows; for anyone else Access.Can will now deny
		// everything, since roleCapabilities[""] is empty.
		return access, nil
	case err != nil:
		return Access{}, fmt.Errorf("panel: resolve access: %w", err)
	}

	if !role.Valid() {
		// A row whose role this build does not recognize - a downgrade,
		// or a hand-edited database. Treated as no access rather than
		// as some default, so an unknown value can never be permissive.
		return access, nil
	}
	access.Role, access.Member = role, true
	return access, nil
}

// Members lists a site's members, with the account details the UI needs.
func (s *Store) Members(ctx context.Context, siteID string) ([]Member, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT m.site_id, m.user_id, m.role, u.email, u.display_name, u.disabled, m.created_at, m.created_by
		FROM panel_site_members m
		JOIN panel_users u ON u.id = m.user_id
		WHERE m.site_id = $1
		-- Owners first, then admins, then viewers, so the person
		-- responsible for the site is at the top of the list rather
		-- than wherever their name happens to sort.
		ORDER BY CASE m.role WHEN 'owner' THEN 0 WHEN 'admin' THEN 1 ELSE 2 END, u.email`,
		siteID)
	if err != nil {
		return nil, fmt.Errorf("panel: list members: %w", err)
	}
	defer rows.Close()

	members := []Member{}
	for rows.Next() {
		var m Member
		var displayName string
		if err := rows.Scan(&m.SiteID, &m.UserID, &m.Role, &m.Email, &displayName, &m.Disabled, &m.CreatedAt, &m.CreatedBy); err != nil {
			return nil, fmt.Errorf("panel: scan member: %w", err)
		}
		m.Name = displayName
		if m.Name == "" {
			m.Name = m.Email
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// AddMember grants a role, or changes it if a membership already exists.
//
// actor is the person doing it, and nil means the deployment itself -
// first-run setup, the installer, an owner claim - which has no role to
// check and no authority that could have been taken away since. Anybody
// else is checked against their live authority inside the transaction
// that does the writing, because this call can lower a role as well as
// raise one and the two need different questions asked. See
// mayActOnMember.
func (s *Store) AddMember(ctx context.Context, siteID string, userID int64, role Role, actor *int64) error {
	if !role.Valid() {
		return fmt.Errorf("panel: invalid role %q", role)
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		st, err := lockMembership(ctx, tx, siteID, userID)
		if err != nil {
			return err
		}
		if err := mayActOnMember(ctx, tx, actor, siteID, st.current, role); err != nil {
			return err
		}
		// Re-granting is the third way to demote somebody, and until this
		// was written it was the way that skipped both other checks: an
		// administrator typing an owner's address with "viewer" beside it
		// left the site with no owner at all. Measured, on a real
		// database, before it was fixed.
		if role != RoleOwner && st.lastOwnerIs(userID) {
			return ErrLastOwner
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO panel_site_members (site_id, user_id, role, created_by)
			VALUES ($1,$2,$3,$4)
			ON CONFLICT (site_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
			siteID, userID, string(role), actor); err != nil {
			return fmt.Errorf("panel: add member: %w", err)
		}
		return nil
	})
}

// ErrLastOwner is returned when removing or demoting the only owner a
// site has.
var ErrLastOwner = errors.New("panel: a site must keep at least one owner")

// ErrNotPermitted is returned when the person making a membership change
// is not allowed to make that particular change - as distinct from not
// being allowed on the page at all, which never gets this far.
var ErrNotPermitted = errors.New("panel: not permitted to change that membership")

// RemoveMember revokes a membership, refusing to remove the last owner.
//
// Both checks happen inside one transaction. Counting owners and then
// deleting in two separate statements would let two administrators each
// see "there are 2 owners" and each remove one, leaving a site nobody
// can administer - a small race, but the kind that only shows up in
// production and cannot be undone from the UI afterwards.
func (s *Store) RemoveMember(ctx context.Context, siteID string, userID int64, actor *int64) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		st, err := lockMembership(ctx, tx, siteID, userID)
		if err != nil {
			return err
		}
		if err := mayActOnMember(ctx, tx, actor, siteID, st.current, ""); err != nil {
			return err
		}
		if st.lastOwnerIs(userID) {
			return ErrLastOwner
		}
		tag, err := tx.Exec(ctx, `DELETE FROM panel_site_members WHERE site_id = $1 AND user_id = $2`, siteID, userID)
		if err != nil {
			return fmt.Errorf("panel: remove member: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetMemberRole changes an existing membership's role, with the same
// last-owner protection as removal: demoting the only owner leaves a
// site without one just as surely as deleting them.
func (s *Store) SetMemberRole(ctx context.Context, siteID string, userID int64, role Role, actor *int64) error {
	if !role.Valid() {
		return fmt.Errorf("panel: invalid role %q", role)
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		st, err := lockMembership(ctx, tx, siteID, userID)
		if err != nil {
			return err
		}
		if err := mayActOnMember(ctx, tx, actor, siteID, st.current, role); err != nil {
			return err
		}
		if role != RoleOwner && st.lastOwnerIs(userID) {
			return ErrLastOwner
		}
		tag, err := tx.Exec(ctx,
			`UPDATE panel_site_members SET role = $3 WHERE site_id = $1 AND user_id = $2`,
			siteID, userID, string(role))
		if err != nil {
			return fmt.Errorf("panel: set member role: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// membershipState is what one locked read tells a membership write: who
// owns this site, and what the person being changed holds today.
type membershipState struct {
	// current is the target's role, empty when they are not a member.
	current Role
	owners  []int64
}

// lastOwnerIs reports whether removing or demoting userID would leave
// the site with nobody who owns it.
func (st membershipState) lastOwnerIs(userID int64) bool {
	return len(st.owners) == 1 && st.owners[0] == userID
}

// lockMembership takes the row locks every membership write needs and
// reads both facts those writes depend on.
//
// One statement rather than two, and the same statement on every path.
// Two transactions that lock the same rows in different orders deadlock;
// here every writer asks for the owner rows and its own target together,
// so the shared rows are always taken in the same scan order. It also
// closes a gap the previous shape had: promoting somebody to owner took
// no lock at all, so a promotion and a demotion could both believe they
// were leaving an owner standing.
func lockMembership(ctx context.Context, tx pgx.Tx, siteID string, userID int64) (membershipState, error) {
	rows, err := tx.Query(ctx, `
		SELECT user_id, role FROM panel_site_members
		 WHERE site_id = $1 AND (role = 'owner' OR user_id = $2)
		 FOR UPDATE`, siteID, userID)
	if err != nil {
		return membershipState{}, fmt.Errorf("panel: lock memberships: %w", err)
	}
	defer rows.Close()

	st := membershipState{owners: []int64{}}
	for rows.Next() {
		var id int64
		var role Role
		if err := rows.Scan(&id, &role); err != nil {
			return membershipState{}, fmt.Errorf("panel: scan membership: %w", err)
		}
		if role == RoleOwner {
			st.owners = append(st.owners, id)
		}
		if id == userID {
			st.current = role
		}
	}
	return st, rows.Err()
}

// mayActOnMember decides whether actor may make this change, against the
// authority actor holds right now rather than when the page was drawn.
//
// next is the role being granted, or empty for a removal.
//
// Asked here rather than only in the handler for the reason the
// invitation gives: a decision read before the write and applied after it
// is a decision about a state that may no longer exist. The handler asks
// too, because that is where the sentence the customer reads comes from -
// but the handler's answer is the message and this one is the rule.
func mayActOnMember(ctx context.Context, tx pgx.Tx, actor *int64, siteID string,
	current, next Role) error {

	if actor == nil {
		// The deployment itself. Not a person, so there is no role to
		// read and nothing that could have been revoked; every path that
		// does have a person passes their id.
		return nil
	}
	access, ok := liveAccess(ctx, tx, *actor, siteID)
	if !ok {
		return ErrNotPermitted
	}
	if !access.CanManageMember(current) {
		return ErrNotPermitted
	}
	if next != "" && !access.CanAssign(next) {
		return ErrNotPermitted
	}
	return nil
}

// liveAccess reads one person's authority over one site inside a
// transaction, from the rows as they stand there.
//
// Built rather than fabricated: a Principal assembled by hand is one
// field away from being a fabricated superadmin, so both fields come from
// the database - and a disabled account gets no authority at all,
// whatever its session still believes.
func liveAccess(ctx context.Context, tx pgx.Tx, userID int64, siteID string) (Access, bool) {
	var superadmin bool
	var current *string
	err := tx.QueryRow(ctx, `
		SELECT u.is_superadmin,
		       (SELECT m.role FROM panel_site_members m
		         WHERE m.user_id = u.id AND m.site_id = $2)
		  FROM panel_users u
		 WHERE u.id = $1 AND NOT u.disabled`, userID, siteID).
		Scan(&superadmin, &current)
	if err != nil {
		return Access{}, false
	}
	access := Access{Principal: Principal{UserID: userID, Superadmin: superadmin}, SiteID: siteID}
	if current != nil {
		access.Role = Role(*current)
		access.Member = true
	}
	return access, true
}

// inTx runs fn in a transaction, rolling back on error.
func (s *Store) inTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("panel: begin transaction: %w", err)
	}
	// Rollback after a successful Commit is a no-op that returns
	// ErrTxClosed, so this is safe to defer unconditionally and removes
	// every early-return path's chance of leaking the transaction.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("panel: commit: %w", err)
	}
	return nil
}
