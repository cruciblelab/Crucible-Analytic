package panel

import (
	"context"
	"errors"
	"fmt"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/relupdate"
)

// Asking for a new version of the binaries, from the panel.
//
// The schema upgrade's twin, and deliberately not the same code. What
// they share is a shape - a request row, an authorised applier, a lock
// on the developer password - and what they do not share is the risk.
//
// # Why this one is locked by default and the schema one is not
//
// A schema upgrade adds tables and columns to a database that is already
// running. The services keep serving throughout, and the worst outcome
// is a migration that fails and rolls back.
//
// Replacing the binaries replaces the collector, which stands in front
// of the customer's website. A package that does not start takes the
// site down, and the panel that would let somebody press "undo" may be
// down beside it. internal/relupdate spends most of its length on
// exactly that, and it still cannot make the operation as cheap as the
// schema one.
//
// So the two defaults differ, and the difference is the honest
// statement of which is riskier. KeyUpgradeLocked is off - press it.
// KeyReleaseUpdateLocked is on - the developer's password opens it, and
// a developer who wants the customer to be able to update without
// asking turns it off deliberately.
//
// # Why the version is typed rather than discovered
//
// The panel does not know what versions exist, and that is structural
// rather than unfinished. Packages come from a URL named in
// upgrader.toml and are verified against a public key held in the same
// file - never in the database, because a key an attacker with the
// database could change is a key that authorises nothing. The panel has
// neither.
//
// A panel that offered a list would have to fetch that list itself,
// which means the panel would need the URL; and a version it presented
// would carry the panel's word rather than the signature's. So it
// queues a version somebody names, the upgrader is the component that
// finds out whether that version exists and whether it is really ours,
// and a version that is neither fails there - before anything is
// replaced.
//
// The cost is real: this button is not the one-click the schema button
// is. That is the price of the panel not holding the key, and it is
// the right way round.

// ReleaseGateAction is what an authorization must be minted for.
//
// Its own action rather than reusing the schema upgrade's: a password
// typed to migrate a database should not also install binaries, and an
// authorization is scoped so that it cannot.
const ReleaseGateAction = "release:install"

// ErrReleaseLocked is returned when the lock is on and no valid
// developer password came with the request.
var ErrReleaseLocked = errors.New("panel: release updates are locked to the developer password")

// ErrReleaseBadVersion is returned when the version is not one this
// project could have produced.
var ErrReleaseBadVersion = errors.New("panel: that is not a version this project publishes")

// ReleaseStatus is what the page needs to draw the section.
type ReleaseStatus struct {
	// Current is the version this panel binary was built as, which is
	// the "from" of any update.
	Current string
	// Locked is KeyReleaseUpdateLocked.
	Locked bool
	// Allowed reports whether this principal could press the button as
	// things stand: entitled, and not stopped by the lock.
	Allowed bool
	// Latest is the most recent request, nil when there has never been
	// one.
	Latest *relupdate.Request

	// Available is what the upgrader last found when it asked the
	// release source which version is current. Its zero value means no
	// check has ever completed, which the page says out loud rather than
	// drawing as "up to date".
	Available relupdate.Available
}

// ReleaseStatus reads the queue and the lock.
//
// current is passed in rather than read here. The store has no stamped
// version - buildinfo.Version takes the string the linker wrote into
// the binary, and only the process that was built holds it. A store
// that guessed would record a version nothing is running.
func (s *Store) ReleaseStatus(ctx context.Context, a Access, current string) (ReleaseStatus, error) {
	out := ReleaseStatus{Current: current}

	locked, err := s.GetBoolSetting(ctx, KeyReleaseUpdateLocked, "")
	if err != nil {
		return out, fmt.Errorf("panel: release status: %w", err)
	}
	out.Locked = locked
	out.Allowed = a.Can(CapManageSettings) && !locked

	latest, err := relupdate.Latest(ctx, s.pool)
	if err != nil {
		return out, fmt.Errorf("panel: release status: %w", err)
	}
	out.Latest = latest

	// Read, never written. The policy on panel_release_available grants
	// INSERT and UPDATE to schema_admin alone, so this is enforced by the
	// database rather than by this function remembering - which matters,
	// because a panel that could write here could tell itself a version
	// exists, and the reason the upgrader does the asking is that the
	// upgrader is the one holding the key.
	available, err := relupdate.ReadAvailable(ctx, s.pool)
	if err != nil {
		return out, fmt.Errorf("panel: release status: %w", err)
	}
	out.Available = available
	return out, nil
}

// RequestRelease queues an update to a named version.
//
// The order of the checks is the order of what they cost, and it is
// also the order of what they reveal. Entitlement first: a viewer is
// refused on who they are, so no password they could supply changes the
// answer and none is read. The lock second. The version last, because
// it is the only one whose failure tells the person something they can
// fix by typing again.
func (s *Store) RequestRelease(ctx context.Context, a Access, auth devgate.Authorization,
	operationID, current, toVersion string) (*relupdate.Request, error) {

	if !a.Can(CapManageSettings) {
		return nil, fmt.Errorf("%w (release)", ErrSettingNotWritable)
	}

	locked, err := s.GetBoolSetting(ctx, KeyReleaseUpdateLocked, "")
	if err != nil {
		return nil, fmt.Errorf("panel: request release: %w", err)
	}
	if locked && !auth.Authorizes(ReleaseGateAction) {
		return nil, ErrReleaseLocked
	}

	// Checked here as well as in relupdate.Ask, and the duplication is
	// deliberate: this one produces a sentence the person reads, the one
	// in the queue is the guarantee that no row can exist with a version
	// nothing could ever install. A check that is only at the surface is
	// a check a second caller does not get.
	if !relupdate.ValidVersion(toVersion) {
		return nil, fmt.Errorf("%w: %q", ErrReleaseBadVersion, toVersion)
	}

	req, err := relupdate.Ask(ctx, s.pool, releaseActorFor(a.Principal), operationID,
		current, toVersion)
	if err != nil {
		return nil, err
	}

	// After the row exists, so the audit log never claims a request the
	// in-flight index refused. The reverse ordering is wrong in the
	// direction nobody checks.
	if _, auditErr := s.recordForReturningID(ctx, a.Principal, AuditEntry{
		Action: ActionReleaseRequested,
		Target: current + "->" + toVersion,
		Detail: map[string]any{
			"request_id": req.ID,
			"was_locked": locked,
		},
	}); auditErr != nil {
		// The request is queued and will be picked up. Failing the call
		// now would tell the customer it did not happen, which is the
		// one answer that is definitely wrong.
		return req, nil
	}
	return req, nil
}

// releaseActorFor converts a principal into the shape this queue
// records.
//
// Its own function rather than actorFor: the two queues have separate
// Actor types in separate packages, and a conversion that happened to
// compile for both would break silently the first time either changed.
func releaseActorFor(p Principal) relupdate.Actor {
	out := relupdate.Actor{Kind: string(p.Kind), Label: p.Label}
	if p.UserID != 0 {
		id := p.UserID
		out.ID = &id
	}
	return out
}
