//go:build integration

package panel

import (
	"context"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/profile"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// Two collectors on one host, one per site, at different profiles (W3):
// each site's page reads its own, and the role's own row - one collector,
// or a build before instances - only for a site no instance names.
func TestEachSiteReadsItsOwnCollector(t *testing.T) {
	beats := newHeartbeatFixture(t)
	store := newTestStoreAt(t, "w3collecting", beats.dsn)
	beats.reaches(t, store.Pool())
	ctx := context.Background()
	collector := func(site string) profile.Level {
		t.Helper()
		c, err := store.Collecting(ctx, site)
		if err != nil {
			t.Fatal(err)
		}
		return c.Collector
	}

	beats.Silence()
	beats.Runs(testdb.Collector+"@site-a", "tam", "{}")
	beats.Runs(testdb.Collector+"@site-b", "hafif", "{}")
	if got := collector("site-a"); got != profile.LevelFull {
		t.Errorf("site-a reads %q, want its own collector's %q", got, profile.LevelFull)
	}
	if got := collector("site-b"); got != profile.LevelOff {
		t.Errorf("site-b reads %q, want its own collector's %q", got, profile.LevelOff)
	}
	if got := collector("site-c"); got != "" {
		t.Errorf("a site no collector names reads %q - another site's collector", got)
	}

	// The role's own row beside them, read before an instance and after
	// one (Read returns the newest beat first): the instance wins either
	// way, so the order of the rows decides nothing.
	beats.Runs(testdb.Collector, "dengeli", "{}")
	if got := collector("site-a"); got != profile.LevelFull {
		t.Errorf("site-a, its instance read after the role's row, reads %q", got)
	}
	beats.Runs(testdb.Collector+"@site-a", "tam", "{}")
	if got := collector("site-a"); got != profile.LevelFull {
		t.Errorf("site-a, its instance read before the role's row, reads %q - the role's row overwrote it", got)
	}
	if got := collector("site-c"); got != profile.LevelCountry {
		t.Errorf("site-c reads %q, want the role's own row's %q", got, profile.LevelCountry)
	}
}

// The refusals by country are every collector's, summed: a deployment's
// block list is one, and each collector refuses its own share.
func TestGeoRefusalsAddUpEveryCollector(t *testing.T) {
	beats := newHeartbeatFixture(t)
	store := newTestStoreAt(t, "w3georet", beats.dsn)
	beats.reaches(t, store.Pool())
	beats.Silence()
	beats.Runs(testdb.Collector+"@site-a", "tam", `{"`+heartbeat.CounterGeoBlocked+`": 4}`)
	beats.Runs(testdb.Collector+"@site-b", "tam", `{"`+heartbeat.CounterGeoBlocked+`": 3}`)
	beats.Runs(testdb.Beacon, "tam", `{"`+heartbeat.CounterGeoBlocked+`": 100}`)
	g, err := store.GeoRefusals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !g.Known || g.Refused != 7 {
		t.Errorf("refusals %+v, want 4 + 3 from the two collectors and nothing from the beacon", g)
	}
}

// The token-key report is one per role, in the state of its instance
// furthest from ready (W3): full mode needs every collector to tokenise,
// and the sentence names the role, never the site an instance is named
// after - the setup checks are drawn for every reader of the health page.
func TestATokenKeyReportIsTheRolesWorstInstance(t *testing.T) {
	beats := newHeartbeatFixture(t)
	store := newTestStoreAt(t, "w3tokenkey", beats.dsn)
	beats.reaches(t, store.Pool())
	ctx := context.Background()
	collectorSays := func() heartbeat.TokenKeyState {
		t.Helper()
		reports, err := store.TokenKeyReports(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var state heartbeat.TokenKeyState
		n := 0
		for _, r := range reports {
			if strings.Contains(r.Service, heartbeat.InstanceSeparator) {
				t.Errorf("a report names an instance: %q", r.Service)
			}
			if r.Service == testdb.Collector {
				state, n = r.State, n+1
			}
		}
		if n != 1 {
			t.Fatalf("%d reports for the collector, want one for the role", n)
		}
		return state
	}

	beats.Silence()
	beats.says(testdb.Collector+"@site-a", heartbeat.TokenKeyPresent)
	beats.says(testdb.Collector+"@site-b", heartbeat.TokenKeyPresent)
	if got := collectorSays(); got != heartbeat.TokenKeyPresent {
		t.Errorf("both instances hold a key and the role reads %q", got)
	}
	beats.says(testdb.Collector+"@site-b", heartbeat.TokenKeyUnknown)
	if got := collectorSays(); got != heartbeat.TokenKeyUnknown {
		t.Errorf("one instance too old to say and the role reads %q", got)
	}
	beats.says(testdb.Collector+"@site-a", heartbeat.TokenKeyAbsent)
	// And the unknown row beating last, so Read returns it first: absent
	// must win from either side, not by arriving first.
	beats.says(testdb.Collector+"@site-b", heartbeat.TokenKeyUnknown)
	if got := collectorSays(); got != heartbeat.TokenKeyAbsent {
		t.Errorf("one instance without a key and the role reads %q - absent is the fix to make first", got)
	}
	beats.says(testdb.Collector+"@site-a", heartbeat.TokenKeyPresent)
	beats.says(testdb.Collector+"@site-b", heartbeat.TokenKeyAbsent)
	if got := collectorSays(); got != heartbeat.TokenKeyAbsent {
		t.Errorf("the order of the rows decided the role's state: %q", got)
	}
}
