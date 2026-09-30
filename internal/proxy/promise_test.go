package proxy_test

import (
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/fullproxy"
	"github.com/cruciblelab/crucible-analytic/internal/proxy"
	"github.com/cruciblelab/crucible-analytic/internal/relupdate"
)

// One product, two modes, one promise about a restart: a request in
// flight gets the same time in both. Until Z7 the passthrough mode gave
// it forever and the full proxy ten seconds, and nothing compared them.
func TestBothModesWaitAsLongForARequestInFlight(t *testing.T) {
	if proxy.DrainTimeout != fullproxy.ShutdownGrace {
		t.Errorf("passthrough waits %s for a busy connection at shutdown and the full proxy %s; "+
			"the two modes of one collector should restart the same way",
			proxy.DrainTimeout, fullproxy.ShutdownGrace)
	}
}

// A collector that waits out its whole drain still comes back inside the
// window the panel's update gives it. Nightly run 43 measured the
// alternative on a real systemd: the collector took 90 seconds to stop,
// the upgrader gave up at 30, and the update was undone.
//
// The five seconds are the start and the first heartbeat, which
// relupdate.HealthWindow's own comment puts at one or two.
func TestADrainedCollectorStillComesBackInsideTheUpdatesWindow(t *testing.T) {
	const startAndFirstBeat = 5 * time.Second
	if proxy.DrainTimeout+startAndFirstBeat > relupdate.HealthWindow {
		t.Errorf("a shutdown may wait %s and a start takes about %s, and the update waits %s "+
			"for the collector to report back", proxy.DrainTimeout, startAndFirstBeat,
			relupdate.HealthWindow)
	}
}
