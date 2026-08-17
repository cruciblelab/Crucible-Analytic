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
// Whether the caller may grant this particular role is decided by
// Access.CanAssign before this is reached.
func (s *Store) AddMember(ctx context.Context, siteID string, userID int64, role Role, grantedBy *int64) error {
	if !role.Valid() {
		return fmt.Errorf("panel: invalid role %q", role)
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO panel_site_members (site_id, user_id, role, created_by)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (site_id, user_id) DO UPDATE SET role = EXCLUDED.role`,
		siteID, userID, string(role), grantedBy)
	if err != nil {
		return fmt.Errorf("panel: add member: %w", err)
	}
	return nil
}

// ErrLastOwner is returned when removing or demoting the only owner a
// site has.
var ErrLastOwner = errors.New("panel: a site must keep at least one owner")

// RemoveMember revokes a membership, refusing to remove the last owner.
//
// Both checks happen inside one transaction. Counting owners and then
// deleting in two separate statements would let two administrators each
// see "there are 2 owners" and each remove one, leaving a site nobody
// can administer - a small race, but the kind that only shows up in
// production and cannot be undone from the UI afterwards.
func (s *Store) RemoveMember(ctx context.Context, siteID string, userID int64) error {
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if err := ensureNotLastOwner(ctx, tx, siteID, userID); err != nil {
			return err
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
func (s *Store) SetMemberRole(ctx context.Context, siteID string, userID int64, role Role) error {
	if !role.Valid() {
		return fmt.Errorf("panel: invalid role %q", role)
	}
	return s.inTx(ctx, func(tx pgx.Tx) error {
		if role != RoleOwner {
			if err := ensureNotLastOwner(ctx, tx, siteID, userID); err != nil {
				return err
			}
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

// ensureNotLastOwner fails if userID is the site's only owner. The
// SELECT takes a row lock so a concurrent transaction removing the other
// owner has to wait for this one to finish.
func ensureNotLastOwner(ctx context.Context, tx pgx.Tx, siteID string, userID int64) error {
	rows, err := tx.Query(ctx,
		`SELECT user_id FROM panel_site_members WHERE site_id = $1 AND role = 'owner' FOR UPDATE`, siteID)
	if err != nil {
		return fmt.Errorf("panel: lock owners: %w", err)
	}
	defer rows.Close()

	owners := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return fmt.Errorf("panel: scan owner: %w", err)
		}
		owners = append(owners, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if len(owners) == 1 && owners[0] == userID {
		return ErrLastOwner
	}
	return nil
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
