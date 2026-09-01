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
	"unicode/utf8"

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
	// ErrReasonTooLong is returned rather than the reason being cut
	// short. A sentence that stops mid-word is one the owner might
	// decide differently on, and this is the text a decision is made
	// from - truncating it silently is the one handling that trades
	// somebody else's judgement for our convenience.
	ErrReasonTooLong = errors.New("panel: the reason for developer access is too long")
)

// MaxReasonRunes bounds the free text attached to a request.
//
// It is typed at a shell and rendered into an owner's page, which until
// C5 nothing ever did - so nothing had cause to bound it. Counted in
// runes rather than bytes because the text is Turkish as often as not
// and a byte limit would cut a shorter sentence than it promises.
const MaxReasonRunes = 500

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
	if utf8.RuneCountInString(reason) > MaxReasonRunes {
		return "", DevAccessRequest{}, ErrReasonTooLong
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", DevAccessRequest{}, fmt.Errorf("panel: draw developer access token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	// The owner's standing answer, read before the write. C8: a
	// deployment can refuse or admit without anybody being at the
	// keyboard.
	//
	// Read separately rather than folded into the INSERT below, unlike
	// the bootstrap test. That test races account creation and has to be
	// atomic with the write; this one races nothing - a policy changed in
	// the same millisecond is a policy the owner set for the next
	// request, and either answer is defensible.
	policy := s.DevAccessPolicyFor(ctx)

	var req DevAccessRequest
	// The "is anyone here yet" test is a subquery inside the INSERT
	// rather than a separate SELECT, so an account being created at the
	// same moment cannot land between the check and the write.
	//
	// Bootstrap still wins over the policy, and it has to: before an
	// account exists there is nobody the policy could belong to, and a
	// default of "ask" would leave the installer holding a request with
	// no one to answer it.
	//
	// An open policy approves without setting auto_approved, which is
	// not a detail. That column means "granted because nobody owned this
	// yet", and RedeemDevAccess kills such a row the moment an account
	// exists - so reusing it here would mint a link that is dead on
	// arrival, in the one case where the owner deliberately said yes.
	err := s.pool.QueryRow(ctx, `
		INSERT INTO panel_dev_access
		  (sha256, reason, request_expires_at, session_ttl_seconds,
		   approved_at, auto_approved, approved_label, denied_at)
		SELECT $1, $2, now() + $3::interval, $4,
		       CASE
		         WHEN NOT EXISTS (SELECT 1 FROM panel_users) THEN now()
		         WHEN $5 = 'open' THEN now()
		       END,
		       NOT EXISTS (SELECT 1 FROM panel_users),
		       CASE
		         WHEN EXISTS (SELECT 1 FROM panel_users) AND $5 = 'open'
		         THEN $6
		         ELSE ''
		       END,
		       CASE
		         WHEN EXISTS (SELECT 1 FROM panel_users) AND $5 = 'deny'
		         THEN now()
		       END
		RETURNING id, reason, requested_at, request_expires_at, session_ttl_seconds,
		          approved_at, auto_approved, denied_at`,
		hashToken(token), reason, seconds(requestTTL), int(sessionTTL.Seconds()),
		policy.Mode, devAccessPolicyLabel,
	).Scan(&req.ID, &req.Reason, &req.RequestedAt, &req.RequestExpiresAt, &req.SessionTTL,
		&req.ApprovedAt, &req.AutoApproved, &req.DeniedAt)
	if err != nil {
		return "", DevAccessRequest{}, fmt.Errorf("panel: store developer access request: %w", err)
	}
	req.SessionTTL *= time.Second

	// The first of the three entries the audit comment promises: the
	// developer asks from the server, the owner decides in the panel, the
	// link is redeemed. Without this one the record starts at "somebody
	// approved something", which reads as though the panel invented the
	// request.
	//
	// The actor is the developer, because whoever ran this had a shell on
	// the machine. That is the whole of what is known about them, and the
	// entry says exactly that rather than implying a name.
	entry := AuditEntry{
		ActorKind:  PrincipalDeveloper,
		ActorLabel: DeveloperLabel,
		Action:     ActionDevAccessRequested,
		Target:     fmt.Sprintf("dev_access:%d", req.ID),
		Detail: map[string]any{
			"reason":        req.Reason,
			"expires_at":    req.RequestExpiresAt,
			"auto_approved": req.AutoApproved,
			// What the deployment's standing answer did to this request,
			// recorded beside the request itself. Otherwise a reader a
			// year later sees a request that was denied within the same
			// second and no reason anywhere for it.
			"policy": policy.Mode,
		},
	}
	if err := s.Record(ctx, entry); err != nil {
		s.logAuditFailure("dev access request", err)
	}
	return token, req, nil
}

// CountPendingDevAccess counts requests still awaiting a decision.
//
// Separate from PendingDevAccess because the banner in the panel's
// chrome asks this on every page an owner loads, and the answer is
// almost always zero. Counting is one index scan; listing is rows,
// scans and allocations for a table that is usually empty - and the
// banner only ever needs the number, because the page that needs the
// rows is one click away.
//
// Asked only of somebody already found entitled to decide. The chrome
// resolves that once and shares it with the navigation, which needs the
// same answer; asking it twice would be two identical membership
// queries on every page in the panel.
func (s *Store) CountPendingDevAccess(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM panel_dev_access
		WHERE approved_at IS NULL AND denied_at IS NULL AND used_at IS NULL
		  AND request_expires_at > now()`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("panel: count pending developer access: %w", err)
	}
	return n, nil
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
	return s.decideDevAccess(ctx, ActionDevAccessApproved, id, by, `
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
	return s.decideDevAccess(ctx, ActionDevAccessDenied, id, by, `
		UPDATE panel_dev_access
		SET denied_at = now(), denied_by = $2
		WHERE id = $1 AND approved_at IS NULL AND denied_at IS NULL AND used_at IS NULL
		  AND request_expires_at > now()`,
		id, by.ID)
}

// decideDevAccess runs one decision and files it.
//
// The audit entry is written here rather than by the handler, for the
// same reason RedeemDevAccess writes its own: it belongs beside the rule
// that decided it, and a second caller - a future command-line approve,
// a support tool - would otherwise have to remember. It is written only
// when the UPDATE actually changed a row, so a decision that lost the
// race does not appear in the log as though it had happened.
func (s *Store) decideDevAccess(ctx context.Context, action string, id int64, by User,
	sql string, args ...any) error {

	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("panel: decide developer access: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrDevAccessDecided
	}

	actorID := by.ID
	entry := AuditEntry{
		ActorKind:  PrincipalUser,
		ActorLabel: by.Email,
		Action:     action,
		Target:     fmt.Sprintf("dev_access:%d", id),
	}
	if actorID != 0 {
		entry.ActorID = &actorID
	}
	if err := s.Record(ctx, entry); err != nil {
		// The decision is already made and the row already written.
		// Failing now would tell the owner their click did not land when
		// it did, which is the worse of the two wrong answers.
		s.logAuditFailure("dev access decision", err)
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
		s.recordRefusedRedemption(ctx, token, from)
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

// recordRefusedRedemption files a developer link that was presented and
// would not open.
//
// This is the most interesting event in the whole flow - a real link
// used late, used twice, used after being denied, or used after the
// deployment acquired an owner - and until now it produced a log line
// and nothing in the record an owner can read.
//
// **Only when the token matches a real row.** The redemption endpoint is
// reachable by anybody with the address, so filing an entry for every
// string presented would let a stranger write audit rows at the speed of
// their connection - a table this panel is not allowed to DELETE from.
// A token matching nothing is somebody guessing base64 of 32 random
// bytes, which teaches nobody anything; a token matching a row is a
// fact about a link this deployment actually issued.
//
// The token itself is never recorded, in any form. The row's id says
// which link it was, and the id is not a credential.
func (s *Store) recordRefusedRedemption(ctx context.Context, token string, from netip.Addr) {
	var (
		id     int64
		reason string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT id, reason FROM panel_dev_access WHERE sha256 = $1`,
		hashToken(token)).Scan(&id, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return
	}
	if err != nil {
		s.logAuditFailure("dev access refusal lookup", err)
		return
	}

	entry := AuditEntry{
		ActorKind:  PrincipalDeveloper,
		ActorLabel: DeveloperLabel,
		Action:     ActionDevAccessRejected,
		Target:     fmt.Sprintf("dev_access:%d", id),
		Detail:     map[string]any{"reason": reason},
	}
	if from.IsValid() {
		entry.IP = &from
	}
	if err := s.Record(ctx, entry); err != nil {
		s.logAuditFailure("dev access refusal", err)
	}
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

// devAccessPolicyLabel is what approved_label says when the policy let
// somebody in rather than a person did.
//
// A fixed marker rather than an owner's name, because no owner was
// asked. The page that lists past requests draws this, and writing a
// name there would put a person's consent on a decision they were not
// present for.
const devAccessPolicyLabel = "erişim politikası: açık"
