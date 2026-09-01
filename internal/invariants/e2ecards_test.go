package invariants

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// dashboardFetchHome is the one file in e2e/ allowed to build the URL of
// a site's dashboard.
const dashboardFetchHome = "shared_test.go"

// TestOneWayToReadADashboard.
//
// # What this is guarding
//
// e2e/ holds two suites for two deployments - the release tarball and
// the shipped compose file - and they ask the same question of both:
// does a real request become a number on the dashboard? They are
// deliberately separate, because the two deployments have already broken
// differently, and that separation is what let one of them learn
// something the other did not.
//
// The dashboard is fed by two processes with different clocks. The
// beacon flushes every two seconds; the collector summarises its rate
// store on a ten-second ticker. So between a proxied request and the two
// traffic cards there is up to a full interval in which the page is
// correct, complete, and says the site has no connection records.
//
// The tarball suite never noticed, because it holds a superuser
// connection and waits on the row in traffic_snapshots before it opens
// the panel at all. The container suite has no such connection - the
// shipped compose file publishes no database port, on purpose - so the
// rendered page is the only thing it can watch, and it was reading it
// exactly once.
//
// Measured on the nightly of 2026-09-01: four cards carried numbers and
// two read "Bu site için henüz hiç bağlantı kaydı yok". Nothing was
// broken. The page was read before the tick.
//
// # Why a file check and not a list of suites
//
// The same shape as every other mirror here: one side is read out of the
// source, so a third deployment path cannot be added without meeting it.
// A list of "suites that must wait" would need the new suite's name
// added by the person least likely to know why - the one writing the
// third path, for whom this failure is history.
//
// Fetching the page is what is policed rather than the waiting, because
// fetching is what a new suite will certainly do and the wait is what it
// will certainly forget. Route it through e2e's dashboard helper and the
// wait comes with it.
func TestOneWayToReadADashboard(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "e2e")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading e2e/: %v", err)
	}

	var carriers []string
	seen := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		seen++
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		if strings.Contains(string(body), `"/site/"`) {
			carriers = append(carriers, e.Name())
		}
	}

	if seen == 0 {
		t.Fatal("no Go files in e2e/, so this test is checking nothing; the directory " +
			"has moved and this check has to move with it")
	}
	if len(carriers) == 0 {
		t.Fatalf("nothing in e2e/ builds a dashboard URL any more.\n"+
			"If %s stopped spelling it \"/site/\", this check is now blind and has to "+
			"be taught the new spelling", dashboardFetchHome)
	}
	for _, name := range carriers {
		if name == dashboardFetchHome {
			continue
		}
		t.Errorf("%s fetches a site's dashboard itself.\n"+
			"Both end-to-end suites must go through dashboard() in %s, which polls "+
			"until the cards fill.\n"+
			"The collector writes on a ten-second ticker, so a page read once, "+
			"straight after the request, reports an empty dashboard on a working "+
			"stack - measured on the nightly of 2026-09-01, where four cards had "+
			"numbers and two said the site had no connection records.",
			name, dashboardFetchHome)
	}
}
