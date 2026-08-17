package panel

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5"
)

// Developer access defaults.
const (
	// DefaultRequestTTL is how long an unapproved request stays open.
	// Long enough for the owner to read a message and decide, short
	// enough that a forgotten request is not a door left ajar.
	DefaultRequestTTL = 2 * time.Hour
	// DefaultSessionTTL is how long a redeemed developer session lasts.
	// Approving grants a visit, not a standing key.
	DefaultSessionTTL = 2 * time.Hour
)

// Errors from the developer access flow.
var (
	// ErrDevAccessInvalid covers every way redemption can fail:
	// unknown, expired, denied, already used, or approved only for a
	// deployment that has since acquired an owner. One error for all of
	// them, because a legitimate developer does the same thing in every
	// case - ask again - and distinguishing them would confirm to
	// anybody else that a guessed token had once been real.
	ErrDevAccessInvalid = errors.New("panel: developer access link is not valid")
	// ErrDevAccessDecided is returned when approving or denying a
	// request that somebody has already decided, or that has expired.
	ErrDevAccessDecided = errors.New("panel: that request has already been decided or has expired")
)

// DevAccessRequest is one request for developer access, as the owner
// sees it.
type DevAccessRequest struct {
	ID               int64
	Reason           string
	RequestedAt      time.Time
	RequestExpiresAt time.Time
	SessionTTL       time.Duration

	ApprovedAt    *time.Time
	ApprovedLabel string
	AutoApproved  bool
	DeniedAt      *time.Time
	UsedAt        *time.Time
	UsedFrom      *netip.Addr
}

// Pending reports whether this request is still awaiting a decision.
func (r DevAccessRequest) Pending() bool {
	return r.ApprovedAt == nil && r.DeniedAt == nil && time.Now().Before(r.RequestExpiresAt)
}

// DevAccessGrant is what a successful redemption yields.
type DevAccessGrant struct {
	ID        int64
	Reason    string
	ExpiresAt time.Time
	// Bootstrap marks a session granted because nobody owned the
	// deployment yet, rather than because an owner said yes. The UI
	// says so, since the two are very different things to be looking at
	// somebody's data under.
	Bootstrap bool
}

// RequestDevAccess creates a request and returns the raw token, which is
// not recoverable afterwards - only its hash is stored.
//
// The token is issued immediately even though it is not yet usable. It
// stays inert until approved, so handing it over early costs nothing and
// means the developer does not have to go back to the server after the
// owner says yes.
//
// If no account exists yet, the request is approved on the spot: there
// is nobody to ask, and installing the system is exactly what this is
// for. That grant is marked auto_approved and, crucially, stops working
// the instant an account exists - see RedeemDevAccess.
func (s *Store) RequestDevAccess(ctx context.Context, reason string, requestTTL, sessionTTL time.Duration) (string, DevAccessRequest, error) {
	if requestTTL <= 0 {
		requestTTL = DefaultRequestTTL
	}
	if sessionTTL <= 0 {
		sessionTTL = DefaultSessionTTL
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", DevAccessRequest{}, fmt.Errorf("panel: draw developer access token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	var req DevAccessRequest
	// The "is anyone here yet" test is a subquery inside the INSERT
	// rather than a separate SELECT, so an account being created at the
	// same moment cannot land between the check and the write.
	err := s.pool.QueryRow(ctx, `
		INSERT INTO panel_dev_access
		  (sha256, reason, request_expires_at, session_ttl_seconds, approved_at, auto_approved)
		SELECT $1, $2, now() + $3::interval, $4,
		       CASE WHEN NOT EXISTS (SELECT 1 FROM panel_users) THEN now() END,
		       NOT EXISTS (SELECT 1 FROM panel_users)
		RETURNING id, reason, requested_at, request_expires_at, session_ttl_seconds, approved_at, auto_approved`,
		hashToken(token), reason, seconds(requestTTL), int(sessionTTL.Seconds()),
	).Scan(&req.ID, &req.Reason, &req.RequestedAt, &req.RequestExpiresAt, &req.SessionTTL, &req.ApprovedAt, &req.AutoApproved)
	if err != nil {
		return "", DevAccessRequest{}, fmt.Errorf("panel: store developer access request: %w", err)
	}
	req.SessionTTL *= time.Second
	return token, req, nil
}

// PendingDevAccess lists requests still awaiting a decision, newest
// first. The panel shows these to owners as a banner they cannot miss.
func (s *Store) PendingDevAccess(ctx context.Context) ([]DevAccessRequest, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, reason, requested_at, request_expires_at, session_ttl_seconds,
		       approved_at, approved_label, auto_approved, denied_at, used_at, used_from
		FROM panel_dev_access
		WHERE approved_at IS NULL AND denied_at IS NULL AND used_at IS NULL
		  AND request_expires_at > now()
		ORDER BY requested_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("panel: list pending developer access: %w", err)
	}
	defer rows.Close()
	return scanDevAccess(rows)
}

// RecentDevAccess lists the last n requests whatever their outcome, so
// an owner can see who asked for access and what was decided.
func (s *Store) RecentDevAccess(ctx context.Context, limit int) ([]DevAccessRequest, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, reason, requested_at, request_expires_at, session_ttl_seconds,
		       approved_at, approved_label, auto_approved, denied_at, used_at, used_from
		FROM panel_dev_access
		ORDER BY requested_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("panel: list developer access: %w", err)
	}
	defer rows.Close()
	return scanDevAccess(rows)
}

func scanDevAccess(rows pgx.Rows) ([]DevAccessRequest, error) {
	requests := []DevAccessRequest{}
	for rows.Next() {
		var (
			r       DevAccessRequest
			seconds int64
		)
		if err := rows.Scan(&r.ID, &r.Reason, &r.RequestedAt, &r.RequestExpiresAt, &seconds,
			&r.ApprovedAt, &r.ApprovedLabel, &r.AutoApproved, &r.DeniedAt, &r.UsedAt, &r.UsedFrom); err != nil {
			return nil, fmt.Errorf("panel: scan developer access request: %w", err)
		}
		r.SessionTTL = time.Duration(seconds) * time.Second
		requests = append(requests, r)
	}
	return requests, rows.Err()
}

// ApproveDevAccess records an owner's consent.
//
// The conditions are in the UPDATE's own WHERE clause rather than in a
// preceding SELECT, so two owners deciding simultaneously - one
// approving, one denying - cannot both succeed.
func (s *Store) ApproveDevAccess(ctx context.Context, id int64, by User) error {
	return s.decideDevAccess(ctx, `
		UPDATE panel_dev_access
		SET approved_at = now(), approved_by = $2, approved_label = $3
		WHERE id = $1 AND approved_at IS NULL AND denied_at IS NULL AND used_at IS NULL
		  AND request_expires_at > now()`,
		id, by.ID, by.Email)
}

// DenyDevAccess records a refusal. A denied request can never be
// approved afterwards - the developer has to ask again, which means the
// owner sees a fresh request rather than a decision they already made
// being quietly reversed.
func (s *Store) DenyDevAccess(ctx context.Context, id int64, by User) error {
	return s.decideDevAccess(ctx, `
		UPDATE panel_dev_access
		SET denied_at = now(), denied_by = $2
		WHERE id = $1 AND approved_at IS NULL AND denied_at IS NULL AND used_at IS NULL
		  AND request_expires_at > now()`,
		id, by.ID)
}

func (s *Store) decideDevAccess(ctx context.Context, sql string, args ...any) error {
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("panel: decide developer access: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrDevAccessDecided
	}
	return nil
}

// RedeemDevAccess consumes an approved link and starts a session.
//
// Every condition is in one statement, for the same reason as before:
// this grants operator authority, so "usually single-use" is not good
// enough.
//
// The last condition is the one that carries the policy. A grant that
// was auto-approved during bootstrap requires that there still be no
// accounts - so the moment the site's owner finishes setup, any
// outstanding installer link is dead, even though it has not expired
// and was never used. Access during installation does not quietly
// become access afterwards.
func (s *Store) RedeemDevAccess(ctx context.Context, token string, from netip.Addr) (DevAccessGrant, error) {
	var (
		grant DevAccessGrant
		fromA any
	)
	if from.IsValid() {
		fromA = from
	}

	err := s.pool.QueryRow(ctx, `
		UPDATE panel_dev_access
		SET used_at = now(),
		    used_from = $2,
		    session_expires_at = now() + (session_ttl_seconds || ' seconds')::interval
		WHERE sha256 = $1
		  AND used_at IS NULL
		  AND denied_at IS NULL
		  AND approved_at IS NOT NULL
		  AND request_expires_at > now()
		  AND (NOT auto_approved OR NOT EXISTS (SELECT 1 FROM panel_users))
		RETURNING id, reason, session_expires_at, auto_approved`,
		hashToken(token), fromA).Scan(&grant.ID, &grant.Reason, &grant.ExpiresAt, &grant.Bootstrap)
	if errors.Is(err, pgx.ErrNoRows) {
		return DevAccessGrant{}, ErrDevAccessInvalid
	}
	if err != nil {
		return DevAccessGrant{}, fmt.Errorf("panel: redeem developer access: %w", err)
	}

	// Recorded here rather than in the handler, beside the rule that
	// decided it. The row in panel_dev_access already carries used_at
	// and used_from, but that table is a work list - it is purged after
	// a month, and it answers "which links exist" rather than "what
	// happened on this deployment". Somebody reading the audit log a
	// year later, asking who was in here and when, should not have to
	// know a second table existed.
	//
	// A bootstrap redemption gets its own action, because "granted
	// because nobody owned this yet" and "granted because the owner said
	// yes" are very different things to have been looking at somebody's
	// data under, and a single line saying "redeemed" would flatten them.
	action := ActionDevAccessRedeemed
	if grant.Bootstrap {
		action = ActionDevAccessBootstrap
	}
	entry := AuditEntry{
		ActorKind:  PrincipalDeveloper,
		ActorLabel: DeveloperLabel,
		Action:     action,
		Target:     fmt.Sprintf("dev_access:%d", grant.ID),
		Detail: map[string]any{
			"reason":     grant.Reason,
			"expires_at": grant.ExpiresAt,
		},
	}
	if from.IsValid() {
		entry.IP = &from
	}
	if err := s.Record(ctx, entry); err != nil {
		// The session is already minted and the row already marked used;
		// failing the redemption now would leave a link that is spent
		// and a developer who cannot get in. Report it and continue -
		// the row in panel_dev_access is still there to be read.
		s.logAuditFailure("dev access redemption", err)
	}
	return grant, nil
}

// logAuditFailure reports an audit write that failed without taking the
// operation down with it.
//
// Deliberately not silent: an append-only log that quietly stops
// appending is worse than no log, because everyone goes on trusting it.
func (s *Store) logAuditFailure(what string, err error) {
	slog.Default().Error("panel: audit write failed", "what", what, "err", err)
}

// PurgeOldDevAccess deletes decided or expired requests older than a
// month.
//
// Not immediately: a recently expired or denied row is evidence that
// somebody asked for access and what happened, which is worth keeping
// around long enough to be noticed.
func (s *Store) PurgeOldDevAccess(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM panel_dev_access WHERE requested_at < now() - interval '30 days'`)
	if err != nil {
		return 0, fmt.Errorf("panel: purge developer access: %w", err)
	}
	return tag.RowsAffected(), nil
}

// seconds renders a duration for a Postgres interval cast.
func seconds(d time.Duration) string {
	return fmt.Sprintf("%d seconds", int(d.Seconds()))
}

// hashToken returns the hex-encoded SHA-256 of a raw token - the same
// storage form api.HashToken uses, so the two credential types are
// handled identically and neither is ever stored in the clear.
func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
