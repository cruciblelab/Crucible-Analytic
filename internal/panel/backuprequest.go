package panel

import (
	"context"
	"errors"
	"fmt"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
	"github.com/cruciblelab/crucible-analytic/internal/devgate"
)

// The panel's end of the backup queue.
//
// # Who may press it, and why it is not the developer password
//
// The customer. Their disk, their data, and the thing a backup protects
// is theirs to protect.
//
// That is a different answer from the release button, which is locked by
// default, and the difference is what each one can do to somebody else.
// A release replaces the program in front of the customer's website. A
// backup writes a file, and the only harm it can do is fill a disk -
// which is refused before a byte is written, with the numbers, by the
// component that can see the disk. See internal/backup.Measure.
//
// Restoring is the other way round and always will be: it writes over
// live data, and the panel does not do it at all. See PLAN.md F1f.
//
// *Kaynak yetersizse engelleriz; onun dışında en iyi serbestliği
// veririz.*
//
// # And the one backup that is not like that
//
// The paragraph above is about the data. The configuration is a second
// artifact with a second answer, and it does need the developer
// password - see SecretsGateAction for why the reasoning above does not
// carry over to it.

// ErrBackupInFlight is returned when one is already queued or running.
var ErrBackupInFlight = errors.New("panel: a backup is already in flight")

// ErrSecretsPasswordRequired means the developer password was not
// supplied for a secrets backup.
var ErrSecretsPasswordRequired = errors.New(
	"panel: a secrets backup needs the developer password")

// SecretsGateAction is what an authorization must be minted for.
//
// # Why the configuration is the one backup with a second lock
//
// The paragraph above says the customer may take a backup because the
// data is theirs and the worst it can do is fill a disk. Both halves
// are still true of a data backup and neither is true of this one.
//
// A secrets backup is the deployment's credentials: the DSN for every
// role, the session key, and `ip_hash_key`. It is encrypted to the
// developer password, so producing one is not the risk - nobody can
// read it without that password anyway. What the password stops is a
// different thing: a file that only we can open, appearing on the
// customer's disk, taken by whoever got into the panel, at a moment
// nobody chose. That is a lever, and levers get pulled.
//
// So it is behind the password for the reason every other guarded
// operation is - the customer cannot grant it to themselves, because
// the password does not come from the database. *İstemciye güvenme,
// sadece sunucuya güven.*
const SecretsGateAction = "backup:secrets"

// BackupStatus is what the health page needs to draw the section.
type BackupStatus struct {
	// Allowed is whether this principal may ask for one.
	Allowed bool
	// Latest is the most recent request, nil when there has never been
	// one.
	Latest *backup.Request
	// Backups is the catalogue, newest first, without paths.
	//
	// Without paths because the panel's role is not granted that column;
	// see internal/backup/schema.sql. The page shows sizes and dates,
	// which is what somebody deciding whether to take another one needs.
	Backups []backup.Backup
}

// BackupStatus reads it.
func (s *Store) BackupStatus(ctx context.Context, a Access) (BackupStatus, error) {
	out := BackupStatus{Allowed: a.Can(CapManageSettings)}

	latest, err := backup.Latest(ctx, s.pool)
	if err != nil {
		return out, fmt.Errorf("panel: backup status: %w", err)
	}
	out.Latest = latest

	list, err := backup.List(ctx, s.pool)
	if err != nil {
		return out, fmt.Errorf("panel: backup status: %w", err)
	}
	out.Backups = list
	return out, nil
}

// RequestBackup queues one.
func (s *Store) RequestBackup(ctx context.Context, a Access, auth devgate.Authorization,
	operationID string, sets []string) (*backup.Request, error) {

	if !a.Can(CapManageSettings) {
		return nil, fmt.Errorf("%w (backup)", ErrSettingNotWritable)
	}

	// Which artifact this is, before anything else. KindOf is also what
	// refuses a request naming the configuration alongside the traffic
	// - the two may never share a file - and it runs again in the
	// upgrader, because a check only the asking side performs is a
	// check a compromised asking side skips.
	kind, err := backup.KindOf(sets)
	if err != nil {
		return nil, err
	}
	if kind == backup.KindSecrets && !auth.Authorizes(SecretsGateAction) {
		return nil, ErrSecretsPasswordRequired
	}

	// Checked here as well as in backup.Ask, and the duplication is
	// deliberate: this one produces the sentence somebody reads, the one
	// in the queue is the guarantee that no row can exist naming a set
	// nothing could ever copy. A check only at the surface is a check a
	// second caller does not get.
	if kind == backup.KindData {
		if _, err := backup.TablesFor(sets); err != nil {
			return nil, err
		}
	}

	req, err := backup.Ask(ctx, s.pool, backupActorFor(a.Principal), operationID, sets)
	switch {
	case errors.Is(err, backup.ErrAlreadyInFlight):
		return nil, ErrBackupInFlight
	case err != nil:
		return nil, err
	}

	// After the row exists, so the audit log never claims a request the
	// in-flight index refused.
	if _, auditErr := s.recordForReturningID(ctx, a.Principal, AuditEntry{
		Action: ActionBackupRequested,
		Target: joinSets(req.Sets),
		Detail: map[string]any{"request_id": req.ID},
	}); auditErr != nil {
		// The request is queued and will be picked up. Failing the call
		// now would tell the customer it did not happen, which is the
		// one answer that is definitely wrong.
		return req, nil
	}
	return req, nil
}

// backupActorFor turns a principal into the shape the queue records.
func backupActorFor(p Principal) backup.Actor {
	a := backup.Actor{Kind: string(p.Kind), Label: p.Label}
	if p.UserID != 0 {
		id := p.UserID
		a.ID = &id
	}
	return a
}

func joinSets(sets []string) string {
	out := ""
	for i, s := range sets {
		if i > 0 {
			out += "+"
		}
		out += s
	}
	return out
}
