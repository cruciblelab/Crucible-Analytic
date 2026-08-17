// Package panel is the management dashboard: the part of this system a
// customer actually logs into.
//
// Two design constraints shape everything here.
//
// First, the audience. The people who own the sites being measured are
// shop owners and bloggers, not engineers. The default panel shows
// visitors, pages, sources and devices, and says nothing about TLS
// fingerprints, autonomous system numbers or bot scores. All of that
// lives behind a developer-mode toggle that is off until someone with
// the authority to use it turns it on.
//
// Second, the tenancy. A site is the boundary: one server can host
// several customers, and there is deliberately no cross-site aggregate
// anywhere in the panel. Access is resolved per site on every request -
// see roles.go.
//
// The panel's database role writes only to the tables in schema.sql. It
// has no access at all to traffic_snapshots or beacon_events; it reads
// analytics through the read-only HTTP API, the same way an external
// panel would. That keeps one component from being both the thing the
// whole internet can reach and the thing with broad database rights.
package panel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when a lookup finds nothing. Callers should
// generally not distinguish it from "wrong credentials" in anything the
// user sees - see the login handler.
var ErrNotFound = errors.New("panel: not found")

// ErrEmailTaken is returned when creating an account whose address
// already exists.
var ErrEmailTaken = errors.New("panel: that email address is already registered")

// Store is the panel's database access. Safe for concurrent use.
type Store struct {
	// ipTokenKeyConfigured is whether the deployment has an IP token key
	// on disk. Set once at wiring time - see SetIPTokenKeyConfigured.
	ipTokenKeyConfigured bool
	pool                 *pgxpool.Pool
}

// NewStore opens a pool to databaseURL and verifies it is reachable -
// the same startup contract as every other store in this project. It
// never runs DDL; apply schema.sql once, separately.
func NewStore(ctx context.Context, databaseURL string) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("panel: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("panel: ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

// Pool exposes the underlying pool so the session store can share it.
// Not for query use elsewhere in the package.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases the pool. Safe to call once.
func (s *Store) Close() { s.pool.Close() }

// User is one panel account.
type User struct {
	ID            int64
	Email         string
	DisplayName   string
	PasswordHash  string
	TOTPSecret    string
	IsSuperadmin  bool
	DeveloperMode bool
	Disabled      bool
	CreatedAt     time.Time
	LastLoginAt   *time.Time
}

// HasTOTP reports whether two-factor authentication is set up.
func (u User) HasTOTP() bool { return u.TOTPSecret != "" }

// Name returns the display name, falling back to the email so a user
// created without one is never rendered as a blank row.
func (u User) Name() string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Email
}

// MaxDisplayNameLength bounds the name a person may give themselves.
//
// The column is unbounded TEXT, which is right for storage and wrong for
// input: a display name is rendered in the header of every page and
// beside every audit entry, so an unbounded one is a way to make those
// unreadable for everybody else on the deployment. Counted in runes, so
// a Turkish name is measured the way it is read.
const MaxDisplayNameLength = 80

// NormalizeEmail lowercases and trims an address.
//
// Normalizing on the way in rather than comparing case-insensitively on
// the way out is what makes the UNIQUE constraint in schema.sql do the
// work: without it, Ahmet@example.com and ahmet@example.com would be two
// accounts, and the second would be a very quiet way to impersonate the
// first.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// userColumns is the select list every user query shares, so a new
// column cannot be added to one scan path and forgotten in another.
const userColumns = `id, email, display_name, password_hash, totp_secret,
	is_superadmin, developer_mode, disabled, created_at, last_login_at`

func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.DisplayName, &u.PasswordHash, &u.TOTPSecret,
		&u.IsSuperadmin, &u.DeveloperMode, &u.Disabled, &u.CreatedAt, &u.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("panel: scan user: %w", err)
	}
	return u, nil
}

// CountUsers reports how many accounts exist. Used to decide whether the
// deployment still needs first-run setup.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM panel_users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("panel: count users: %w", err)
	}
	return n, nil
}

// CreateUser inserts an account. email is normalized here rather than
// trusted from the caller, so no path can insert a differently-cased
// duplicate.
func (s *Store) CreateUser(ctx context.Context, email, displayName, passwordHash string, superadmin bool) (User, error) {
	email = NormalizeEmail(email)
	if email == "" {
		return User{}, fmt.Errorf("panel: email is required")
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO panel_users (email, display_name, password_hash, is_superadmin)
		VALUES ($1, $2, $3, $4)
		RETURNING `+userColumns,
		email, strings.TrimSpace(displayName), passwordHash, superadmin)

	u, err := scanUser(row)
	if err != nil {
		// 23505 is unique_violation; the only unique constraint on this
		// table is the email one, so this is unambiguous.
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
			return User{}, ErrEmailTaken
		}
		return User{}, err
	}
	return u, nil
}

// UserByEmail looks an account up for login.
func (s *Store) UserByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM panel_users WHERE email = $1`, NormalizeEmail(email)))
}

// UserByID reloads an account from its session.
func (s *Store) UserByID(ctx context.Context, id int64) (User, error) {
	return scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM panel_users WHERE id = $1`, id))
}

// ListUsers returns every account, oldest first. Superadmin-only in the
// UI; the store does not enforce that, the handler does.
func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+userColumns+` FROM panel_users ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("panel: list users: %w", err)
	}
	defer rows.Close()

	users := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// SetPasswordHash replaces an account's password.
func (s *Store) SetPasswordHash(ctx context.Context, userID int64, hash string) error {
	return s.exec(ctx, "set password", `UPDATE panel_users SET password_hash = $2 WHERE id = $1`, userID, hash)
}

// SetDeveloperMode records the per-user preference. Whether the user is
// *allowed* to turn it on is a role question the handler answers first -
// see Access.Can(CapUseDeveloperMode).
func (s *Store) SetDeveloperMode(ctx context.Context, userID int64, on bool) error {
	return s.exec(ctx, "set developer mode", `UPDATE panel_users SET developer_mode = $2 WHERE id = $1`, userID, on)
}

// SetTOTPSecret enables two-factor authentication, or disables it when
// secret is empty.
func (s *Store) SetTOTPSecret(ctx context.Context, userID int64, secret string) error {
	return s.exec(ctx, "set totp secret", `UPDATE panel_users SET totp_secret = $2 WHERE id = $1`, userID, secret)
}

// SetDisplayName updates the name shown in the UI and recorded in the
// audit log.
func (s *Store) SetDisplayName(ctx context.Context, userID int64, name string) error {
	return s.exec(ctx, "set display name", `UPDATE panel_users SET display_name = $2 WHERE id = $1`, userID, strings.TrimSpace(name))
}

// SetDisabled locks or unlocks an account. Disabled accounts keep their
// memberships and their audit history - deleting a user to revoke their
// access would take their history with them.
func (s *Store) SetDisabled(ctx context.Context, userID int64, disabled bool) error {
	return s.exec(ctx, "set disabled", `UPDATE panel_users SET disabled = $2 WHERE id = $1`, userID, disabled)
}

// TouchLastLogin records a successful sign-in.
func (s *Store) TouchLastLogin(ctx context.Context, userID int64) error {
	return s.exec(ctx, "touch last login", `UPDATE panel_users SET last_login_at = now() WHERE id = $1`, userID)
}

// exec runs a statement that must affect exactly one row, turning "no
// such id" into ErrNotFound rather than a silent success. Every update
// above goes through it, so none of them can quietly do nothing.
func (s *Store) exec(ctx context.Context, what, sql string, args ...any) error {
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("panel: %s: %w", what, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
