package panel

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
)

// C7.2's second net (B3h): the operator issues a one-time recovery code
// for a member who has lost both their phone and their codes.
//
// # Why it is here now
//
// It was decided with the codes themselves (C7.2, 2026-08-26: "the
// operator link, the second net for somebody who has lost their codes
// too") and written up as done: the commit said "the operator
// regenerates the codes and hands one over", KURULUM said "regenerate
// their codes from the member list", and the second-factor page told
// people to ask the site's owner to reset their second factor. None of it
// existed - GenerateRecoveryCodes had one caller, the account page, for
// the person themselves - and somebody who lost both could not get back
// in at all. Measured on 2026-09-28.
//
// # One code, not a set
//
// "Hands one over" is taken literally. A set of eight shown to the
// operator leaves seven live credentials to somebody else's account in
// whatever the operator copied them into, long after the person is back
// in - and the operator's own access ends with the developer session,
// while the codes would not. One code, used once, leaves nothing behind:
// the person signs in with it, has no codes left, and is sent to the
// account page that says so and mints their own.
//
// # Who, and behind what
//
// The operator: superadmin authority, which in practice is a developer
// session - no path in the product creates a superadmin account, and a
// developer session exists only once an owner has approved it (or the
// owner's standing policy admits it). Not the site's owner: the code
// opens the account, and the account may belong to other sites too; an
// owner of one who could mint it could walk into the others.
//
// And behind the developer password, asked every time, like every other
// operation that mints something a session could not take back: a
// session somebody else is holding - a shared machine, a stolen cookie -
// must not be able to print a working credential for another person's
// account. It is the reason the account page asks for the current
// password before issuing one's own.

// RecoveryIssueGateAction is the developer password's action for issuing
// somebody else's recovery code.
const RecoveryIssueGateAction = "recovery:issue"

var (
	// ErrIssueNeedsOperator is a request from somebody without the
	// operator's authority.
	ErrIssueNeedsOperator = errors.New("panel: only the operator may issue a recovery code for somebody else")
	// ErrIssueNeedsPassword is a request without the developer password.
	ErrIssueNeedsPassword = errors.New("panel: issuing somebody's recovery code needs the developer password")
)

// Issued is somebody's one-time recovery code, in the clear - the only
// time it exists in readable form.
type Issued struct {
	Email string
	Code  string
}

// IssueRecoveryCode replaces one member's unused recovery codes with a
// single fresh one, for the operator to hand over.
//
// The member must hold a live membership of this site: the operator acts
// from a site's member list, and a site's list is not a way to reach
// somebody who does not belong to it. The old codes stop working the
// moment this commits, which is the point - one of them may be why the
// person is asking.
//
// The audit entry is written inside the same transaction: a code minted
// for another person's account with no record of who minted it cannot
// commit.
func (s *Store) IssueRecoveryCode(ctx context.Context, siteID string, actor Principal,
	auth devgate.Authorization, userID int64) (Issued, error) {

	if !actor.Superadmin {
		return Issued{}, ErrIssueNeedsOperator
	}
	if !auth.Authorizes(RecoveryIssueGateAction) {
		return Issued{}, ErrIssueNeedsPassword
	}

	code, err := newRecoveryCode()
	if err != nil {
		return Issued{}, err
	}
	out := Issued{Code: code}
	err = s.inTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			SELECT u.email FROM panel_users u
			  JOIN panel_site_members m ON m.user_id = u.id
			 WHERE m.site_id = $1 AND u.id = $2 AND `+liveMembership("m")+`
			   FOR UPDATE OF u`, siteID, userID).Scan(&out.Email)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("panel: find the member: %w", err)
		}
		// created_by names an account, and the operator has none: a
		// developer session is not a row in panel_users. It stays NULL
		// and the audit entry below says who issued it.
		if err := storeRecoveryCodes(ctx, tx, userID, 0, []string{code}); err != nil {
			return err
		}
		entry := AuditEntry{
			ActorKind: actor.Kind, ActorLabel: actor.Label,
			Action: ActionRecoveryCodesIssued, SiteID: siteID, Target: out.Email,
			Detail: map[string]any{"for": out.Email, "count": 1, "self": false},
		}
		if actor.UserID != 0 {
			id := actor.UserID
			entry.ActorID = &id
		}
		_, err = recordIn(ctx, tx, entry)
		return err
	})
	if err != nil {
		return Issued{}, err
	}
	return out, nil
}
