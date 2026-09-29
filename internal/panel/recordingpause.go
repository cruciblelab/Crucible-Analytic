package panel

import (
	"context"
	"time"
)

// PauseSpan is one stretch in which a site's recording was paused (PLAN
// §4, #3), as the panel recorded it.
type PauseSpan struct {
	From, Until time.Time
}

// RecordingPauses is every pause of site's recording that overlaps
// [from, to), oldest first, read back from the audit log - including one
// still running, whose Until is ahead.
//
// # Derived, not stored
//
// ApplySetting and ClearSetting write the audit entry in the same path as
// the value, with its moment and the value itself. That is the whole of a
// pause's history, and it is the record somebody reads to ask who paused.
// A second store of the same fact would be a second place for it to be
// wrong, and a table the schema does not have.
//
// # Whose moments
//
// The panel's. A service applies a change at its next settings poll - a
// minute by default - so recording stopped up to that much after From,
// and a pause lifted by hand went on up to that much after Until. A pause
// whose end simply came ends exactly at Until: both writers compare it
// against their own clock.
//
// # The rule, entry by entry
//
// A running pause whose end came before the entry had ended there. Then:
// a value ahead of the entry's moment pauses - from that moment, or, if a
// pause is still running, it moves that pause's end: the same stretch,
// not a second one. Anything else lifts a running pause at the entry's
// moment.
func (s *Store) RecordingPauses(ctx context.Context, site string, from, to time.Time) ([]PauseSpan, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT time, coalesce(detail->>'to', '')
		FROM panel_audit_log
		WHERE site_id = $1 AND target = $2 AND action IN ($3, $4)
		ORDER BY time, id`,
		site, string(KeyCollectionPausedUntil), ActionSettingChanged, ActionSettingReset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var all []PauseSpan
	var running *PauseSpan
	for rows.Next() {
		var at time.Time
		var value string
		if err := rows.Scan(&at, &value); err != nil {
			return nil, err
		}
		if running != nil && !running.Until.After(at) {
			all = append(all, *running)
			running = nil
		}
		end, paused := UntilOf(value, at)
		switch {
		case paused && running != nil:
			running.Until = end
		case paused:
			running = &PauseSpan{From: at, Until: end}
		case running != nil:
			running.Until = at
			all = append(all, *running)
			running = nil
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if running != nil {
		all = append(all, *running)
	}

	var out []PauseSpan
	for _, span := range all {
		if span.From.Before(to) && span.Until.After(from) {
			out = append(out, span)
		}
	}
	return out, nil
}
