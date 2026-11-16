package panel

import (
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
)

// A restart request reaches every process of a role, and the role's state
// is the one furthest behind - among the processes that were there to
// hear it (W3).
func TestARolesRestartIsItsSlowestInstance(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	requested := now.Add(-30 * time.Second)
	restarted := requested.Add(2 * time.Second)
	later := requested.Add(9 * time.Second)
	alive := func(instance string, started time.Time) heartbeat.Beat {
		return heartbeat.Beat{Service: "collector", Instance: instance, StartedAt: started, BeatAt: now.Add(-time.Second)}
	}
	silent := func(instance string) heartbeat.Beat { // quiet long before the request
		return heartbeat.Beat{Service: "collector", Instance: instance,
			StartedAt: now.Add(-48 * time.Hour), BeatAt: requested.Add(-time.Hour)}
	}
	for _, c := range []struct {
		name    string
		beats   []heartbeat.Beat
		state   RestartState
		started time.Time
	}{
		{"both restarted: done, at the later start",
			[]heartbeat.Beat{alive("a", later), alive("b", restarted)}, RestartDone, later},
		{"one restarted, one still waiting: pending",
			[]heartbeat.Beat{alive("a", restarted), alive("b", now.Add(-time.Hour))}, RestartPending, restarted},
		{"the order of the rows decides nothing",
			[]heartbeat.Beat{alive("b", now.Add(-time.Hour)), alive("a", restarted)}, RestartPending, restarted},
		{"an instance silent before the request is not waited for",
			[]heartbeat.Beat{alive("a", restarted), silent("b")}, RestartDone, restarted},
		{"every instance silent: the newest row speaks",
			[]heartbeat.Beat{silent("a"), silent("b")}, RestartNotReporting, now.Add(-48 * time.Hour)},
	} {
		state, started := restartStateOfAll(true, c.beats, requested, now)
		if state != c.state || !started.Equal(c.started) {
			t.Errorf("%s: %v at %v, want %v at %v", c.name, state, started, c.state, c.started)
		}
	}
	if state, _ := restartStateOfAll(true, nil, requested, now); state != RestartNeverReported {
		t.Errorf("no row: %v, want never reported", state)
	}
}
