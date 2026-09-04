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
	// ActionDevAccessPolicySet records the owner deciding what happens
	// to future requests, as opposed to deciding one of them.
	//
	// Its own action because it is the only entry in this group written
	// by somebody who is not answering a particular request - and
	// because "why was I refused in March" is a question the individual
	// refusals cannot answer on their own. A policy nobody can see the
	// history of is a policy the owner cannot be held to, or defended by.
	ActionDevAccessPolicySet = "dev_access.policy_set"

	ActionPasswordChanged  = "account.password_changed"
	ActionTOTPEnabled      = "account.totp_enabled"
	ActionTOTPDisabled     = "account.totp_disabled"
	ActionDeveloperModeOn  = "account.developer_mode_on"
	ActionDeveloperModeOff = "account.developer_mode_off"

	// ActionTechnicalDoorOpened records an owner confirming the warning
	// in front of the technical wizard. Its own action rather than a
	// detail on something else, because the question it answers -
	// "who went in there, and when" - is the first one asked when a
	// working installation stops working.
	ActionTechnicalDoorOpened = "setup.technical_door_opened"

	// The outgoing mail account.
	//
	// Three actions rather than one "mail.changed", because they answer
	// different questions and the useful one is usually the third.
	// "Who pointed our mail at that server" and "when did verification
	// last succeed" are the two things somebody asks when invitations
	// stop arriving, and flattening them into one row loses the second.
	//
	// The detail carries the host, the port and the diagnosis - never
	// the username and never anything derived from the password. The
	// audit log's own comment says never credentials, and a mail server
	// address is not one; the account name on it is close enough to be
	// worth leaving out.
	ActionMailSaved    = "mail.saved"
	ActionMailVerified = "mail.verified"
	ActionMailDeleted  = "mail.deleted"

	ActionUserCreated  = "user.created"
	ActionUserDisabled = "user.disabled"
	ActionUserEnabled  = "user.enabled"

	ActionMemberAdded   = "member.added"
	ActionMemberRemoved = "member.removed"
	ActionMemberRerole  = "member.role_changed"

	ActionTokenCreated = "token.created"
	ActionTokenRevoked = "token.revoked"

	// Recovery codes. Issuing is recorded because it mints credentials
	// that outlive the session that asked for them, and using one is
	// recorded because it is the one way into an account that goes round
	// both the password and the second factor. "Who got in, how, and
	// from where" has to be answerable afterwards - that is the whole
	// price of having a way in at all.
	ActionRecoveryCodesIssued = "recovery.issued"
	ActionRecoveryCodeUsed    = "recovery.used"

	// Settings changes. Recorded with the old and the new value, because
	// "who set the retention to 3650 days" is a question that gets asked
	// months later, when the only thing anybody remembers is that it
	// used to be different.
	ActionSettingChanged = "setting.changed"
	ActionSettingReset   = "setting.reset"
	// ActionSettingMigrated marks a value copied out of a config file
	// into the settings table by the migration command.
	//
	// Its own action rather than a "changed" entry with a note, because
	// the question it answers is different. "Somebody decided this" and
	// "this is what the file said when we moved it" are not the same
	// fact, and a year later the difference is exactly what somebody
	// wants to know about a value nobody remembers setting.
	ActionSettingMigrated = "setting.migrated"
	// ActionUpgradeRequested marks somebody pressing the upgrade button.
	//
	// The request row records what happened to the migration; this
	// records that a person asked for one, which is the half that has to
	// survive after the request row is swept.
	ActionUpgradeRequested = "upgrade.requested"
	// ActionReleaseRequested marks somebody asking for a new version of
	// the binaries.
	//
	// Separate from ActionUpgradeRequested because the two answer
	// different questions after the fact. "Who migrated the database" and
	// "who replaced the collector" are the two things somebody
	// investigating an outage asks in that order, and a single action
	// with a field to distinguish them is one grep away from being read
	// as the wrong one.
	ActionReleaseRequested = "release.requested"

	// ActionRangeRefreshRequested marks somebody pressing "refresh the
	// IP datasets now".
	//
	// Recorded for the same reason as the line above: the request row is
	// swept, and the fact that a person asked has to outlive it. It also
	// answers the question a repeated press raises - whether one person
	// pressed six times or six people pressed once.
	ActionRangeRefreshRequested = "rangerefresh.requested"

	// Developer password attempts, on the settings that carry legal
	// weight. Both outcomes, because a record of failures alone cannot
	// show that the successful attempt at 03:00 followed an hour of
	// failures from the same address.
	ActionDevPasswordGranted = "dev_password.granted"
	ActionDevPasswordRefused = "dev_password.refused"
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
	_, err := s.recordReturningID(ctx, e)
	return err
}

// recordReturningID is Record, plus the id of the row it wrote.
//
// A separate entry point rather than a wider signature on Record: the
// id is wanted at exactly one call site - the one that ties a settings
// change to the operation record - and thirty other callers do not need
// to start ignoring a value.
func (s *Store) recordReturningID(ctx context.Context, e AuditEntry) (int64, error) {
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

	var id int64
	err = s.pool.QueryRow(ctx, `
		INSERT INTO panel_audit_log
		  (actor_kind, actor_id, actor_label, action, site_id, target, detail, ip, user_agent)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id`,
		string(e.ActorKind), actorID, e.ActorLabel, e.Action, e.SiteID, e.Target, detail, ip, e.UserAgent).
		Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("panel: record audit entry: %w", err)
	}
	return id, nil
}

// RecordFor is Record with the actor filled in from a principal, which
// is what almost every call site wants.
func (s *Store) RecordFor(ctx context.Context, p Principal, e AuditEntry) error {
	_, err := s.recordForReturningID(ctx, p, e)
	return err
}

func (s *Store) recordForReturningID(ctx context.Context, p Principal, e AuditEntry) (int64, error) {
	e.ActorKind = p.Kind
	e.ActorLabel = p.Label
	if p.UserID != 0 {
		id := p.UserID
		e.ActorID = &id
	}
	return s.recordReturningID(ctx, e)
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
