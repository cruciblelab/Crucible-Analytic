package panel

import (
	"context"
	"fmt"
	"time"
)

// Housekeeping trims the panel's own tables.
//
// # Why this file exists
//
// Two purge functions were written before it - PurgeOldLoginAttempts and
// PurgeOldDevAccess - and neither had a caller anywhere in the
// repository, tests included. They were correct, documented, dead code,
// and the tables they bound had been growing without limit since the day
// they were created.
//
// The cause was structural rather than an oversight in either: the panel
// had no periodic work at all. The collector and the beacon each run a
// retention ticker, so a sweep written for them has somewhere to be
// called from; a sweep written for the panel had nowhere. Adding a third
// purge without fixing that would have produced a third piece of dead
// code.
//
// So the entry point comes first and the sweeps hang off it, and
// TestEveryPurgeIsCalledByHousekeeping fails if a future one is written
// and not added here. A sweep nothing calls is the failure that stays
// green: the table grows, every test passes, and the first symptom is a
// full disk.
//
// # Why these are constants rather than settings
//
// panel_login_attempts and panel_dev_access already chose that, and
// these two follow. The tree's retention is a setting because an
// operator has a real reason to want a year of security logs; nobody has
// a reason to want a year of the panel's own diagnostic chatter, and a
// settable retention on a table that can hold personal data is a way to
// extend how long that data is kept.

const (
	// logRetention is how long a panel_logs row is kept.
	//
	// Fourteen days, matching the log tree's ordinary retention, because
	// this table is a subset of the tree and outliving it would be odd.
	// The tree keeps the copy anybody debugging a month-old problem
	// actually wants.
	logRetention = 14 * 24 * time.Hour

	// operationRetention is how long a panel_operations row is kept.
	//
	// Thirty days, matching panel_dev_access. An operation record is
	// read "usually within the hour" - a customer says something went
	// wrong while they were changing a setting - and a month is long
	// enough for the slow version of that conversation. The audit log,
	// which answers the question that has to survive, is untouched by
	// any of this.
	operationRetention = 30 * 24 * time.Hour
)

// Report is what one housekeeping pass removed.
//
// Returned rather than logged from inside, so the caller decides whether
// a quiet pass is worth a line. Every pass logging "deleted 0 rows"
// four times an hour is how a log becomes unreadable.
type Report struct {
	LoginAttempts int64
	DevAccess     int64
	Logs          int64
	Operations    int64
}

// Total is how many rows the pass removed.
func (r Report) Total() int64 {
	return r.LoginAttempts + r.DevAccess + r.Logs + r.Operations
}

// Housekeeping runs every sweep and reports what went.
//
// One failing sweep does not stop the others. They are independent
// tables and a permission problem on one is not a reason to let the
// other three grow - the same reason the health page's three sections
// fail separately.
func (s *Store) Housekeeping(ctx context.Context) (Report, error) {
	var rep Report
	var firstErr error

	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	var err error
	rep.LoginAttempts, err = s.PurgeOldLoginAttempts(ctx)
	note(err)
	rep.DevAccess, err = s.PurgeOldDevAccess(ctx)
	note(err)
	rep.Logs, err = s.PurgeOldLogs(ctx)
	note(err)
	rep.Operations, err = s.PurgeOldOperations(ctx)
	note(err)

	return rep, firstErr
}

// PurgeOldLogs trims panel_logs.
//
// This is the sweep panel_logs_sweep exists for, and it is the reason
// that policy is not redundant: the write policy is FOR ALL and bounded
// by `service = current_user`, so under it alone the panel would delete
// the lines it wrote itself and silently leave the collector's, the
// beacon's and the API's. Measured, not assumed - see the schema and
// logsink.TestTheSweepRemovesEveryServicesLinesAndNotOnlyItsOwn.
func (s *Store) PurgeOldLogs(ctx context.Context) (int64, error) {
	// Bound passed as a parameter rather than pasted into the SQL. The
	// values here are constants today, so this is not what stops an
	// injection; it is what stops the next edit from being the one that
	// does, if somebody makes the retention configurable and reaches for
	// the nearest pattern. seconds() is devaccess.go's, reused rather
	// than copied - two renderers of the same cast are two that can
	// drift.
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM panel_logs WHERE at < now() - $1::interval`, seconds(logRetention))
	if err != nil {
		return 0, fmt.Errorf("panel: purge logs: %w", err)
	}
	return tag.RowsAffected(), nil
}

// PurgeOldOperations trims panel_operations.
//
// Keyed on started_at rather than finished_at, because an operation that
// never finished has no finished_at - and an operation that never
// finished is exactly the kind this table exists to record, so a sweep
// that skipped them would keep the boring rows forever and lose the
// interesting ones.
func (s *Store) PurgeOldOperations(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM panel_operations WHERE started_at < now() - $1::interval`,
		seconds(operationRetention))
	if err != nil {
		return 0, fmt.Errorf("panel: purge operations: %w", err)
	}
	return tag.RowsAffected(), nil
}
