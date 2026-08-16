package logging

import (
	"log/slog"
	"testing"
	"time"
)

func newControls(base slog.Level) *Controls {
	v := &slog.LevelVar{}
	v.Set(base)
	return &Controls{level: v, base: base}
}

func TestControls_LevelChangeTakesEffectImmediately(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	tree := newTestTree(t, &now)

	controls := newControls(slog.LevelInfo)
	logger := slog.New(NewHandler(tree, HandlerConfig{Level: controls.level}))

	logger.Debug("before", In(CategoryApp))
	controls.SetLevel(slog.LevelDebug)
	logger.Debug("after", In(CategoryApp))

	// A LevelVar is read per record, so the first line is filtered and the
	// second is not - without a restart, which is the whole point.
	lines := readLines(t, tree.DayDir("2026-05-06")+"/app.log")
	if len(lines) != 1 {
		t.Fatalf("got %d records, want only the one written after the change", len(lines))
	}
	if lines[0]["msg"] != "after" {
		t.Errorf("kept %q, want the record written after the level changed", lines[0]["msg"])
	}
}

// "Turn on debug, reproduce it, turn it off" is the one log setting a
// support call reaches for, and forgetting the last step is how a disk
// fills. So it expires by itself.
func TestControls_VerboseRaiseExpiresOnItsOwn(t *testing.T) {
	controls := newControls(slog.LevelInfo)
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	until := now.Add(30 * time.Minute).Format(time.RFC3339)

	if got := controls.Apply(slog.LevelInfo, until, now); got != slog.LevelDebug {
		t.Errorf("during the window level = %v, want debug", got)
	}
	// Nothing has to remember to turn it off.
	after := now.Add(time.Hour)
	if got := controls.Apply(slog.LevelInfo, until, after); got != slog.LevelInfo {
		t.Errorf("after the window level = %v, want the configured info", got)
	}
}

// A service that restarts mid-window must come back still raised rather
// than silently dropping to info, or a reproduction attempt loses its
// logging halfway through.
func TestControls_ARestartMidWindowStaysVerbose(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	until := now.Add(30 * time.Minute).Format(time.RFC3339)

	fresh := newControls(slog.LevelInfo) // as a restarted process would be
	if got := fresh.Apply(slog.LevelInfo, until, now.Add(10*time.Minute)); got != slog.LevelDebug {
		t.Errorf("after a restart mid-window level = %v, want debug", got)
	}
}

// A malformed timestamp must not be able to pin a deployment at debug
// forever, which is how a disk fills.
func TestControls_AMalformedWindowIsTreatedAsNotRaised(t *testing.T) {
	controls := newControls(slog.LevelInfo)
	now := time.Now()
	for _, bad := range []string{"yarın", "2026-13-45", "0", "true"} {
		if got := controls.Apply(slog.LevelInfo, bad, now); got != slog.LevelInfo {
			t.Errorf("verbose_until=%q gave level %v, want the configured info", bad, got)
		}
	}
}

// "Verbose" that made logging quieter would be a surprising reading of
// the word.
func TestControls_VerboseOnlyEverLowersTheThreshold(t *testing.T) {
	controls := newControls(slog.LevelDebug)
	now := time.Now()
	until := now.Add(time.Hour).Format(time.RFC3339)
	if got := controls.Apply(slog.LevelDebug, until, now); got != slog.LevelDebug {
		t.Errorf("level = %v, want debug to stay debug", got)
	}
}

func TestVerboseActive_ReportsTheRemainingTime(t *testing.T) {
	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	until := now.Add(14 * time.Minute)

	active, left := VerboseActive(until.Format(time.RFC3339), now)
	if !active {
		t.Fatal("reported inactive during the window")
	}
	if left < 13*time.Minute || left > 15*time.Minute {
		t.Errorf("remaining = %v, want about 14 minutes", left)
	}

	if active, _ := VerboseActive(until.Format(time.RFC3339), now.Add(time.Hour)); active {
		t.Error("reported active after the window closed")
	}
	if active, _ := VerboseActive("", now); active {
		t.Error("reported active with nothing set")
	}
}

// A nil Controls must be usable, so a caller that never took one does not
// have to branch.
func TestControls_NilIsSafe(t *testing.T) {
	var controls *Controls
	controls.SetLevel(slog.LevelDebug)
	if got := controls.Level(); got != slog.LevelInfo {
		t.Errorf("nil Level() = %v, want info", got)
	}
	if got := controls.Apply(slog.LevelWarn, "", time.Now()); got != slog.LevelWarn {
		t.Errorf("nil Apply() = %v, want the configured level back", got)
	}
}
