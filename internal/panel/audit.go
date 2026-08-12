package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"time"
)

// Audit actions. Constants rather than free-form strings so the log can
// be filtered reliably and a typo cannot create a second spelling of an
// event that already exists.
const (
	ActionSetupCompleted = "setup.completed"
	ActionLoginSucceeded = "login.succeeded"
	ActionLoginFailed    = "login.failed"
	ActionLoginThrottled = "login.throttled"
	ActionLogout         = "logout"

	// Developer access, in the order it happens: the developer asks from
	// the server, the owner decides in the panel, the link is redeemed.
	// Each step is its own entry so the record shows who consented to
	// what, not merely that somebody got in.
	ActionDevAccessRequested = "dev_access.requested"
	ActionDevAccessApproved  = "dev_access.approved"
	ActionDevAccessDenied    = "dev_access.denied"
	ActionDevAccessRedeemed  = "dev_access.redeemed"
	ActionDevAccessRejected  = "dev_access.rejected"
	// ActionDevAccessBootstrap marks a session granted because nobody
	// owned the deployment yet. Distinct from an approved one because
	// nobody consented to it - there was nobody to ask - and that
	// difference should be visible forever afterwards.
	ActionDevAccessBootstrap = "dev_access.bootstrap"

	ActionPasswordChanged  = "account.password_changed"
	ActionTOTPEnabled      = "account.totp_enabled"
	ActionTOTPDisabled     = "account.totp_disabled"
	ActionDeveloperModeOn  = "account.developer_mode_on"
	ActionDeveloperModeOff = "account.developer_mode_off"

	ActionUserCreated  = "user.created"
	ActionUserDisabled = "user.disabled"
	ActionUserEnabled  = "user.enabled"

	ActionMemberAdded   = "member.added"
	ActionMemberRemoved = "member.removed"
	ActionMemberRerole  = "member.role_changed"

	ActionTokenCreated = "token.created"
	ActionTokenRevoked = "token.revoked"
)

// AuditEntry is one recorded action.
type AuditEntry struct {
	ID         int64
	Time       time.Time
	ActorKind  PrincipalKind
	ActorID    *int64
	ActorLabel string
	Action     string
	SiteID     string
	Target     string
	Detail     map[string]any
	IP         *netip.Addr
	UserAgent  string
}

// Record appends an audit entry.
//
// The table grants the panel role INSERT and SELECT but not UPDATE or
// DELETE, so this is append-only at the database level rather than by
// convention: a compromised panel process cannot erase what it did.
//
// It takes the principal's label by value rather than joining to
// panel_users at read time, so the log still names who acted after that
// account is renamed or removed.
func (s *Store) Record(ctx context.Context, e AuditEntry) error {
	if e.Detail == nil {
		e.Detail = map[string]any{}
	}
	detail, err := json.Marshal(e.Detail)
	if err != nil {
		// Never let an unserializable detail field lose the whole entry:
		// what happened matters more than the extra context.
		detail = []byte(`{"_error":"detail could not be encoded"}`)
	}

	var actorID any
	if e.ActorID != nil {
		actorID = *e.ActorID
	}
	var ip any
	if e.IP != nil {
		ip = *e.IP
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO panel_audit_log
		  (actor_kind, actor_id, actor_label, action, site_id, target, detail, ip, user_agent)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		string(e.ActorKind), actorID, e.ActorLabel, e.Action, e.SiteID, e.Target, detail, ip, e.UserAgent)
	if err != nil {
		return fmt.Errorf("panel: record audit entry: %w", err)
	}
	return nil
}

// RecordFor is Record with the actor filled in from a principal, which
// is what almost every call site wants.
func (s *Store) RecordFor(ctx context.Context, p Principal, e AuditEntry) error {
	e.ActorKind = p.Kind
	e.ActorLabel = p.Label
	if p.UserID != 0 {
		id := p.UserID
		e.ActorID = &id
	}
	return s.Record(ctx, e)
}

// AuditFilter narrows a log query.
type AuditFilter struct {
	// SiteID limits to one site; empty means every site the caller is
	// entitled to, which the handler must already have decided.
	SiteID string
	// Sites, when non-nil, limits to this set - the form used for a
	// non-superadmin, who may read the log only for sites they
	// administer. An empty non-nil slice therefore matches nothing,
	// which is the correct answer for someone who administers none.
	Sites  []string
	Limit  int
	Offset int
}

// Audit reads the log, newest first, with the total for paging.
func (s *Store) Audit(ctx context.Context, f AuditFilter) ([]AuditEntry, int, error) {
	// A single predicate shared by both queries, so the count and the
	// page can never disagree about what they are looking at.
	const where = `
		WHERE ($1::text = '' OR site_id = $1)
		  AND ($2::text[] IS NULL OR site_id = ANY($2))`

	var sites any
	if f.Sites != nil {
		sites = f.Sites
	}

	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM panel_audit_log`+where, f.SiteID, sites).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("panel: count audit: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, time, actor_kind, actor_id, actor_label, action, site_id, target, detail, ip, user_agent
		FROM panel_audit_log`+where+`
		ORDER BY time DESC, id DESC
		LIMIT $3 OFFSET $4`, f.SiteID, sites, f.Limit, f.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("panel: read audit: %w", err)
	}
	defer rows.Close()

	entries := []AuditEntry{}
	for rows.Next() {
		var (
			e      AuditEntry
			kind   string
			detail []byte
			ip     *netip.Addr
		)
		if err := rows.Scan(&e.ID, &e.Time, &kind, &e.ActorID, &e.ActorLabel, &e.Action,
			&e.SiteID, &e.Target, &detail, &ip, &e.UserAgent); err != nil {
			return nil, 0, fmt.Errorf("panel: scan audit entry: %w", err)
		}
		e.ActorKind = PrincipalKind(kind)
		e.IP = ip
		if len(detail) > 0 {
			_ = json.Unmarshal(detail, &e.Detail)
		}
		entries = append(entries, e)
	}
	return entries, total, rows.Err()
}
