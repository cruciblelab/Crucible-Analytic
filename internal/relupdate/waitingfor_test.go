package relupdate

import (
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
)

// Which processes a restart waits for, when a role runs more than once
// (W3). The other three roles are back in every case, so what is missing
// is the collector's alone.
func TestARestartWaitsForEveryInstanceThatWasAlive(t *testing.T) {
	since := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	before := since.Add(-10 * time.Second)                        // alive when the restart began
	gone := since.Add(-3*heartbeat.DefaultInterval - time.Second) // quiet before it
	after := since.Add(5 * time.Second)
	row := func(instance string, at time.Time) heartbeat.Beat {
		return heartbeat.Beat{Service: "collector", Instance: instance, BeatAt: at}
	}
	for _, c := range []struct {
		name string
		rows []heartbeat.Beat
		want string
	}{
		{"both instances back", []heartbeat.Beat{row("a", after), row("b", after)}, ""},
		{"one of two back: the other is named", []heartbeat.Beat{row("a", after), row("b", before)}, "collector@b"},
		{"neither back: both named", []heartbeat.Beat{row("a", before), row("b", before)}, "collector@a,collector@b"},
		{"one back, one quiet before the restart: not waited for", []heartbeat.Beat{row("a", after), row("b", gone)}, ""},
		{"only a quiet row: the role did not come back", []heartbeat.Beat{row("b", gone)}, "collector"},
		{"no row at all", nil, "collector"},
		{"rolled back: the role's own row is back, the instances never will be",
			[]heartbeat.Beat{row("", after), row("a", before), row("b", before)}, ""},
		{"the role's own row alone, not back", []heartbeat.Beat{row("", before)}, "collector"},
	} {
		rows := append([]heartbeat.Beat(nil), c.rows...)
		for _, role := range HealthServices[1:] {
			rows = append(rows, heartbeat.Beat{Service: role, BeatAt: after})
		}
		if got := strings.Join(waitingFor(rows, since), ","); got != c.want {
			t.Errorf("%s: waiting for %q, want %q", c.name, got, c.want)
		}
	}
}
