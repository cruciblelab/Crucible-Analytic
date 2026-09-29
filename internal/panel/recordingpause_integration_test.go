//go:build integration

package panel

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// TestRecordingPausesAreReadBackFromTheAuditLog (PLAN §4, #3): every way
// a pause ends, read back from the entries the settings page writes.
//
//   - lifted by hand: ends at the lift;
//   - ended by itself: ends at its end, and a later entry does not move it;
//   - extended while running: one stretch, from the first press;
//   - lifted by the reset button while running: ends there too;
//   - still running: its end is ahead;
//   - and neither another site's pause nor another key's change is part
//     of this site's history - the site name, changed inside a pause,
//     would read as a lift if the key were not asked.
func TestRecordingPausesAreReadBackFromTheAuditLog(t *testing.T) {
	const ns = "duraklama"
	store := newTestStore(t, ns)
	ctx := context.Background()
	admin := testdb.Admin(t)
	const site, other = "duraklama-a", "duraklama-b"
	clear := func() {
		if _, err := admin.Exec(context.Background(),
			`DELETE FROM panel_settings WHERE site_id LIKE '%duraklama%'`); err != nil {
			t.Logf("clearing settings: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)
	owner := customerAccess()

	// at applies one change and moves its audit entry to when.
	at := func(when time.Time, s string, key Key, value any) {
		t.Helper()
		var err error
		if value == nil {
			err = store.ClearSetting(ctx, owner, key, s, devgate.Authorization{}, nil)
		} else {
			err = store.ApplySetting(ctx, owner, key, s, value, devgate.Authorization{}, nil)
		}
		if err != nil {
			t.Fatalf("%s %s = %v: %v", s, key, value, err)
		}
		tag, err := admin.Exec(ctx, `UPDATE panel_audit_log SET time = $1
			WHERE id = (SELECT max(id) FROM panel_audit_log WHERE site_id = $2 AND target = $3)`,
			when, s, string(key))
		if err != nil || tag.RowsAffected() != 1 {
			t.Fatalf("moving the entry for %s %s: %v (%d rows)", s, key, err, tag.RowsAffected())
		}
	}
	moment := func(t time.Time) string { return t.UTC().Format(time.RFC3339) }

	now := time.Now().Truncate(time.Second)
	t0 := now.AddDate(0, 0, -10)
	h := time.Hour
	day := 24 * h

	at(t0, site, KeyCollectionPausedUntil, moment(t0.Add(6*h)))
	at(t0.Add(h), site, KeySiteName, "Dükkân")
	at(t0.Add(2*h), site, KeyCollectionPausedUntil, "")

	at(t0.Add(day), site, KeyCollectionPausedUntil, moment(t0.Add(day+h)))

	at(t0.Add(2*day), site, KeyCollectionPausedUntil, moment(t0.Add(2*day+h)))
	at(t0.Add(2*day+30*time.Minute), site, KeyCollectionPausedUntil, moment(t0.Add(2*day+3*h)))
	at(t0.Add(2*day+4*h), site, KeyCollectionPausedUntil, nil)

	at(t0.Add(3*day), site, KeyCollectionPausedUntil, moment(t0.Add(3*day+6*h)))
	at(t0.Add(3*day+h), site, KeyCollectionPausedUntil, nil)

	at(t0.Add(12*h), other, KeyCollectionPausedUntil, moment(t0.Add(18*h)))

	at(now.Add(-h), site, KeyCollectionPausedUntil, moment(now.Add(5*h)))

	want := []PauseSpan{
		{t0, t0.Add(2 * h)},
		{t0.Add(day), t0.Add(day + h)},
		{t0.Add(2 * day), t0.Add(2*day + 3*h)},
		{t0.Add(3 * day), t0.Add(3*day + h)},
		{now.Add(-h), now.Add(5 * h)},
	}
	got, err := store.RecordingPauses(ctx, site, t0.Add(-day), now.Add(day))
	if err != nil {
		t.Fatal(err)
	}
	if describe(got) != describe(want) {
		t.Errorf("read back\n%s\nwant\n%s", describe(got), describe(want))
	}

	// The range, from both sides of both edges: the first stretch ends
	// where the range begins, the second begins where it ends - neither is
	// in it - and a second either way takes each in.
	got, err = store.RecordingPauses(ctx, site, t0.Add(2*h), t0.Add(day))
	if err != nil || len(got) != 0 {
		t.Errorf("a range touching two stretches at their edges read %s (%v)", describe(got), err)
	}
	got, err = store.RecordingPauses(ctx, site, t0.Add(2*h-time.Second), t0.Add(day+time.Second))
	if err != nil || describe(got) != describe(want[:2]) {
		t.Errorf("a range overlapping two stretches by a second read %s (%v)", describe(got), err)
	}
}

func describe(spans []PauseSpan) string {
	out := ""
	for _, s := range spans {
		out += fmt.Sprintf("[%s, %s) ", s.From.UTC().Format(time.RFC3339), s.Until.UTC().Format(time.RFC3339))
	}
	return out
}
