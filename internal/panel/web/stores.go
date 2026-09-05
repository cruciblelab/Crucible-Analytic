package web

import (
	"context"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/rangerefresh"
	"github.com/cruciblelab/crucible-analytic/internal/relupdate"
	"github.com/cruciblelab/crucible-analytic/internal/upgrade"
)

// What each area of the panel is allowed to ask the database for.
//
// # The measurement this file exists because of
//
// *panel.Store has 99 methods. No file in this package uses more than
// eleven of them and the median is three. The seams were already there;
// what was missing was anything that made them true. A field typed
// *panel.Store gives every handler the whole surface, so "which parts of
// the database does the backup page touch" was a question you answered by
// reading the file rather than by reading a declaration - and "what else
// breaks if I change RequestBackup" was a question with no mechanical
// answer at all.
//
// Each interface below is one area's entire database surface, and the
// accessor beside it is the only way that area reaches it. The compiler
// enforces the rest: backup.go cannot call UserByID, because backupStore
// does not have it.
//
// # What this buys, exactly
//
//   - The blast radius of a change is a compiler error with a name on it.
//     Change RequestBackup and the failure is backupStore, not 30 files.
//   - Each area's logic runs with no database at all. A fake with three
//     methods stands in for the whole Store, so a refusal path can be
//     tested against the case that produces it rather than against a
//     database somebody has to arrange into that state first.
//   - Reading one area tells you what it can touch. Five lines, not 99.
//
// # Why each area is split in two
//
// Every section here is drawn on GET and pressed on POST, and the two
// paths are different in a way worth making mechanical: drawing must not
// queue anything. So the reader interface carries only the status query
// and the *StatusFor functions take that, while the *Post functions take
// the whole thing.
//
// The property that buys is stronger than a convention: the function the
// GET path calls cannot request a backup, a release, an upgrade or a
// refresh, because the type it was handed has no method that does.
//
// # What this is not
//
// It is not an injection point on Server and there is no override field.
// The store travels as an argument, because a field nothing in production
// ever sets is a field that goes stale without anybody noticing. Tests
// call the area functions directly and pass their own.
//
// The accessors are also the compile-time proof that *panel.Store
// satisfies every interface here, which is why there are no `var _`
// assertions: an interface nothing returns is an interface nothing
// checks.

// operationStore is the audit record every button opens before it works.
//
// Shared rather than repeated in each area's interface: it is the same
// method for the same reason everywhere - the operation id has to exist
// before the work starts so the log lines the work produces can carry it.
type operationStore interface {
	BeginOperation(ctx context.Context, a panel.Access, action, target, site string) (*panel.Operation, error)
}

// backupReader is what drawing the backup section may ask for.
type backupReader interface {
	BackupStatus(ctx context.Context, a panel.Access) (panel.BackupStatus, error)
}

// backupStore adds what pressing the button may ask for.
type backupStore interface {
	backupReader
	operationStore
	RequestBackup(ctx context.Context, a panel.Access, operationID string, sets []string) (*backup.Request, error)
}

// diskStore is what the storage section may ask for.
//
// One method, and the narrowest interface here for a reason worth saying
// out loud: everything else on that page is measured from the filesystem
// rather than read from the database. The section is a disk report, and
// the one number it takes from Postgres is how big Postgres is.
type diskStore interface {
	DatabaseBytes(ctx context.Context) (int64, error)
}

// rangeRefreshReader is what drawing the IP-dataset section may ask for.
type rangeRefreshReader interface {
	RangeRefreshStatus(ctx context.Context, a panel.Access) (panel.RangeRefreshStatus, error)
}

// rangeRefreshStore adds what pressing the button may ask for.
type rangeRefreshStore interface {
	rangeRefreshReader
	operationStore
	RequestRangeRefresh(ctx context.Context, a panel.Access, operationID string) (*rangerefresh.Request, error)
}

// releaseReader is what drawing the version section may ask for.
type releaseReader interface {
	ReleaseStatus(ctx context.Context, a panel.Access, current string) (panel.ReleaseStatus, error)
}

// releaseStore adds what pressing the button may ask for.
type releaseStore interface {
	releaseReader
	operationStore
	RequestRelease(ctx context.Context, a panel.Access, auth devgate.Authorization,
		operationID, current, toVersion string) (*relupdate.Request, error)
}

// upgradeReader is what drawing the schema section may ask for.
type upgradeReader interface {
	UpgradeStatus(ctx context.Context, a panel.Access) (panel.UpgradeStatus, error)
}

// upgradeStore adds what pressing the button may ask for.
type upgradeStore interface {
	upgradeReader
	operationStore
	RequestUpgrade(ctx context.Context, a panel.Access, auth devgate.Authorization,
		operationID string) (*upgrade.Request, error)
}

// backups narrows the store to the backup section's surface.
func (s *Server) backups() backupStore { return s.Store }

// database narrows the store to the storage section's surface.
func (s *Server) database() diskStore { return s.Store }

// ranges narrows the store to the refresh section's surface.
func (s *Server) ranges() rangeRefreshStore { return s.Store }

// releases narrows the store to the version section's surface.
func (s *Server) releases() releaseStore { return s.Store }

// upgrades narrows the store to the schema section's surface.
func (s *Server) upgrades() upgradeStore { return s.Store }
