//go:build integration

package panel

import (
	"context"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// TestGeoRefusalsAreTheCollectorsOwnCount walks what the heartbeat table
// can say, on a copy of it (testdb.HeartbeatCopy): nothing; a collector
// whose build does not count; one that counted zero; one that counted.
// And only the collector's row: the beacon refuses nothing by country.
func TestGeoRefusalsAreTheCollectorsOwnCount(t *testing.T) {
	beats := newHeartbeatFixture(t)
	store := newTestStoreAt(t, "georet", beats.dsn)
	beats.reaches(t, store.Pool())
	ctx := context.Background()
	read := func() GeoRefusals {
		t.Helper()
		g, err := store.GeoRefusals(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}

	beats.Silence()
	if g := read(); g.Known {
		t.Errorf("no row at all reads as known: %+v", g)
	}
	beats.Counts(testdb.Beacon, `{"`+heartbeat.CounterGeoBlocked+`": 5}`)
	if g := read(); g.Known {
		t.Errorf("the beacon's row was read as the collector's: %+v", g)
	}
	beats.Counts(testdb.Collector, `{"yazilan": 3}`)
	if g := read(); g.Known {
		t.Errorf("a collector that does not count reads as known: %+v", g)
	}
	beats.Counts(testdb.Collector, `{"`+heartbeat.CounterGeoBlocked+`": 0}`)
	if g := read(); !g.Known || g.Refused != 0 {
		t.Errorf("a collector that counted zero: %+v", g)
	}
	beats.Counts(testdb.Collector, `{"`+heartbeat.CounterGeoBlocked+`": 7}`)
	g := read()
	if !g.Known || g.Refused != 7 {
		t.Errorf("a collector that counted seven: %+v", g)
	}
	// Since is the process's start, not the beat: the count is since then.
	if ago := time.Since(g.Since); ago < 50*time.Minute || ago > 70*time.Minute {
		t.Errorf("Since is %s ago, want the start an hour back", ago)
	}
}
