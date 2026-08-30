package logsink

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/logging"
)

// PanelLevel has to hold two facts at once and dropping either is a real
// failure with a real cost:
//
//   - drop the WARN floor and this table becomes the largest in the
//     database, which is the disk-full failure by a second road
//   - drop the verbose switch and a customer with no shell has no way to
//     see detail during the one conversation where they need it
//
// A pure function, so it is tested as one rather than through a database.

func controlsAt(t *testing.T, configured slog.Level) *logging.Controls {
	t.Helper()
	_, controls, closeLogs, err := logging.Setup("test", logging.Config{
		Level: strings.ToLower(configured.String()),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeLogs)
	return controls
}

func TestTheFloorIsWarnWhenTheTreeIsChattier(t *testing.T) {
	controls := controlsAt(t, slog.LevelInfo)

	if got := PanelLevel(controls).Level(); got != slog.LevelWarn {
		t.Errorf("level = %v, want WARN. The tree keeps INFO on disk where it is cheap; "+
			"this table shares a database with the customer's analytics", got)
	}
}

func TestTheVerboseSwitchReachesTheTable(t *testing.T) {
	controls := controlsAt(t, slog.LevelInfo)

	// What the per-site verbose switch does: a temporary raise with an
	// expiry, which is the thing that stops it from being how the disk
	// fills.
	until := time.Now().Add(time.Hour).Format(time.RFC3339)
	controls.Apply(slog.LevelInfo, until, time.Now())

	if got := PanelLevel(controls).Level(); got != slog.LevelDebug {
		t.Errorf("level = %v, want DEBUG while a raise is in force. "+
			"\"Turn on detail, reproduce it, look\" is the whole reason a customer "+
			"opens the log page, and they have no shell to look anywhere else", got)
	}

	// And it comes back down when the raise expires, without a restart.
	expired := time.Now().Add(-time.Hour).Format(time.RFC3339)
	controls.Apply(slog.LevelInfo, expired, time.Now())

	if got := PanelLevel(controls).Level(); got != slog.LevelWarn {
		t.Errorf("level = %v after the raise expired, want WARN", got)
	}
}

// TestTheSinkIsNeverLouderThanTheTree.
//
// The case that is easy to miss. An operator who configured the tree to
// ERROR has said they want less; a table that then carried WARNs the
// tree itself discarded would be a subset that is bigger than its
// superset, and the customer would see lines the operator cannot find
// on disk.
func TestTheSinkIsNeverLouderThanTheTree(t *testing.T) {
	controls := controlsAt(t, slog.LevelError)

	if got := PanelLevel(controls).Level(); got != slog.LevelError {
		t.Errorf("level = %v with the tree at ERROR, want ERROR", got)
	}
}

// TestNoControlsStillHasAFloor.
//
// A nil Controls is a wiring mistake, and the failure mode of guessing
// "write everything" is the disk-full one. It guesses the quiet way.
func TestNoControlsStillHasAFloor(t *testing.T) {
	if got := PanelLevel(nil).Level(); got != slog.LevelWarn {
		t.Errorf("level = %v with no controls, want WARN", got)
	}
}
