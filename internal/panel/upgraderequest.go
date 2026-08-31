package panel

import (
	"context"
	"errors"
	"fmt"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
	"github.com/cruciblelab/crucible-analytic/internal/upgrade"
)

// The upgrade button's rules.
//
// # Why they are here and not in the handler
//
// D4a learned this the expensive way: a comment claiming the handler
// enforced a gate was wrong, because the store did. A rule that lives in
// an HTTP handler is a rule the next entry point does not have, and this
// one is reachable from a page today and will be reachable from a repair
// operation in B3.
//
// So the whole decision - may this actor ask, is the lock on, does the
// password open it - is made here, and the handler's only job is to turn
// the answer into a page.

// UpgradeGateAction is what an authorization must be minted for.
//
// Its own action rather than reusing a setting's, because the developer
// password authorizes one named thing at a time: a password typed to
// change the privacy mode must not also apply a schema migration.
const UpgradeGateAction = "upgrade:apply"

// ErrUpgradeLocked is returned when the lock is on and no valid
// authorization was supplied.
//
// Distinct from ErrDeveloperPasswordRequired, which is about a setting.
// The page says something different for each - "this deployment's
// upgrades are held by the developer" is not the same sentence as "this
// setting needs the password" - and a caller that could not tell them
// apart would have to guess.
var ErrUpgradeLocked = errors.New("panel: upgrades are locked to the developer password")

// ErrUpgradeNotNeeded is returned when the database already carries what
// this build expects.
//
// Refused rather than accepted-and-ignored: a request that queued, ran
// and changed nothing would still occupy the one in-flight slot and
// would read, afterwards, exactly like a real upgrade. Somebody looking
// at the history a month later could not tell which rows meant anything.
var ErrUpgradeNotNeeded = errors.New("panel: the schema is already the one this build expects")

// UpgradeStatus is what the page shows.
type UpgradeStatus struct {
	// Needed is whether this build expects a schema the database does
	// not have.
	Needed bool
	// Locked is whether the developer has taken the button.
	Locked bool
	// Allowed is whether the actor could press it right now with no
	// password. False with Locked true means the password would open it.
	Allowed bool
	// Latest is the most recent request, nil when there has never been
	// one.
	Latest *upgrade.Request
}

// UpgradeStatus reads everything the page needs in one call.
func (s *Store) UpgradeStatus(ctx context.Context, a Access) (UpgradeStatus, error) {
	var out UpgradeStatus

	state, err := schemaver.Read(ctx, s.pool)
	switch {
	case errors.Is(err, schemaver.ErrNoTable):
		// A database from before versioning. Ahead() reports true for an
		// unrecorded state, which is the honest answer: this build
		// expects something that database has never been told about.
		out.Needed = true
	case err != nil:
		return out, fmt.Errorf("panel: upgrade status: %w", err)
	default:
		out.Needed = !state.Matches()
	}

	locked, err := s.GetBoolSetting(ctx, KeyUpgradeLocked, "")
	if err != nil {
		return out, fmt.Errorf("panel: upgrade status: %w", err)
	}
	out.Locked = locked
	out.Allowed = a.Can(CapManageSettings) && !locked

	latest, err := upgrade.Latest(ctx, s.pool)
	if err != nil {
		return out, fmt.Errorf("panel: upgrade status: %w", err)
	}
	out.Latest = latest
	return out, nil
}

// RequestUpgrade asks for the schema to be brought up to date.
//
// # The lock, and why it is a password rather than a capability
//
// Unlocked by default, because the customer is meant to be able to press
// it: "işi bilmeyen normal müşteri de yapabilmeli". Locked, only the
// developer password opens it.
//
// A capability would not do. RoleOwner holds CapManageMembers and can
// therefore make somebody an admin, and RoleAdmin holds
// CapUseDeveloperMode - so a customer can reach any capability by
// granting it to themselves. That is fine for things that end in
// looking; for things that can make work for the developer, the only
// protection that holds is a password which comes from the
// configuration file. See SOZLUK.md §3.
func (s *Store) RequestUpgrade(ctx context.Context, a Access, auth devgate.Authorization,
	operationID string) (*upgrade.Request, error) {

	// Entitlement first, and before anything that costs work. A viewer
	// is refused on who they are, whatever they typed - so no password
	// they could supply changes the answer.
	if !a.Can(CapManageSettings) {
		return nil, fmt.Errorf("%w (upgrade)", ErrSettingNotWritable)
	}

	locked, err := s.GetBoolSetting(ctx, KeyUpgradeLocked, "")
	if err != nil {
		return nil, fmt.Errorf("panel: request upgrade: %w", err)
	}
	if locked && !auth.Authorizes(UpgradeGateAction) {
		return nil, ErrUpgradeLocked
	}

	// Checked before the row is written, so a request that could not
	// change anything never occupies the in-flight slot.
	state, err := schemaver.Read(ctx, s.pool)
	if err != nil && !errors.Is(err, schemaver.ErrNoTable) {
		return nil, fmt.Errorf("panel: request upgrade: %w", err)
	}
	if state.Matches() {
		return nil, ErrUpgradeNotNeeded
	}

	req, err := upgrade.Ask(ctx, s.pool, actorFor(a.Principal), operationID,
		state.Version, schemaver.Version, schemaver.Fingerprint)
	if err != nil {
		return nil, err
	}

	// Recorded after the row exists, so the audit log never claims a
	// request that was refused by the in-flight index. The reverse
	// ordering would be wrong in the direction nobody checks.
	if _, auditErr := s.recordForReturningID(ctx, a.Principal, AuditEntry{
		Action: ActionUpgradeRequested,
		Target: fmt.Sprintf("%d->%d", state.Version, schemaver.Version),
		Detail: map[string]any{
			"request_id":  req.ID,
			"was_locked":  locked,
			"fingerprint": schemaver.Fingerprint,
		},
	}); auditErr != nil {
		// The request is already queued and will be applied. Failing the
		// call now would tell the customer it did not happen, which is
		// the one answer that is definitely wrong.
		return req, nil
	}
	return req, nil
}

// actorFor converts a principal into the shape the queue records.
func actorFor(p Principal) upgrade.Actor {
	out := upgrade.Actor{Kind: string(p.Kind), Label: p.Label}
	if p.UserID != 0 {
		id := p.UserID
		out.ID = &id
	}
	return out
}
