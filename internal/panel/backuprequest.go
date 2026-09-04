package panel

import (
	"context"
	"errors"
	"fmt"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
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

// ErrBackupInFlight is returned when one is already queued or running.
var ErrBackupInFlight = errors.New("panel: a backup is already in flight")

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
func (s *Store) RequestBackup(ctx context.Context, a Access, operationID string,
	sets []string) (*backup.Request, error) {

	if !a.Can(CapManageSettings) {
		return nil, fmt.Errorf("%w (backup)", ErrSettingNotWritable)
	}

	// Checked here as well as in backup.Ask, and the duplication is
	// deliberate: this one produces the sentence somebody reads, the one
	// in the queue is the guarantee that no row can exist naming a set
	// nothing could ever copy. A check only at the surface is a check a
	// second caller does not get.
	if _, err := backup.TablesFor(sets); err != nil {
		return nil, err
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
