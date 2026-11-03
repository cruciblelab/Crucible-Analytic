//go:build integration

package panel

import (
	"context"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/profile"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// TestTheHeartbeatReadersFindTheServicesByTheirRoles.
//
// The collector and the beacon write their rows under the roles they
// connect as. Collecting looked for the beacon as "beacon", which no
// install creates, and so never read its profile. Here: rows under the
// default roles are read; after the deployment names other roles, rows
// under those are read and rows under the defaults are not - the name
// comes from the store's configuration, not from a constant that happens
// to equal it.
func TestTheHeartbeatReadersFindTheServicesByTheirRoles(t *testing.T) {
	beats := newHeartbeatFixture(t)
	store := newTestStoreAt(t, "roller", beats.dsn)
	beats.reaches(t, store.Pool())
	ctx := context.Background()
	admin := testdb.Admin(t)
	full := profile.All()[len(profile.All())-1]
	row := func(service string) {
		t.Helper()
		if _, err := admin.Exec(ctx, `INSERT INTO ca_panel_heartbeat.service_heartbeat
			(service, version, started_at, beat_at, counters, profile)
			VALUES ($1, 'fixture', now(), now(), $2::jsonb, $3)`,
			service, `{"`+heartbeat.CounterGeoBlocked+`": 9}`, full.ID); err != nil {
			t.Fatal(err)
		}
	}

	// The defaults are the roles install.sh creates - the ones the
	// development cluster's services connect as.
	if DefaultServiceRoles.Collector != testdb.Collector || DefaultServiceRoles.Beacon != testdb.Beacon {
		t.Fatalf("DefaultServiceRoles %+v are not the installed roles %s/%s", DefaultServiceRoles, testdb.Collector, testdb.Beacon)
	}

	beats.Silence()
	row(testdb.Collector)
	row(testdb.Beacon)
	c, err := store.Collecting(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if c.Collector != full.Level || c.Beacon != full.Level {
		t.Errorf("under the default roles Collecting is %+v, want both %q", c, full.Level)
	}
	if g, _ := store.GeoRefusals(ctx); !g.Known || g.Refused != 9 {
		t.Errorf("under the default roles GeoRefusals is %+v", g)
	}

	store.SetServiceRoles(ServiceRoles{Collector: "toplayici", Beacon: "isaretci"})
	c, _ = store.Collecting(ctx)
	if c.Collector != "" || c.Beacon != "" {
		t.Errorf("roles renamed, and the default rows are still read: %+v", c)
	}
	if g, _ := store.GeoRefusals(ctx); g.Known {
		t.Errorf("roles renamed, and the default collector row is still read: %+v", g)
	}
	row("toplayici")
	row("isaretci")
	c, _ = store.Collecting(ctx)
	if c.Collector != full.Level || c.Beacon != full.Level {
		t.Errorf("under the renamed roles Collecting is %+v", c)
	}
	if g, _ := store.GeoRefusals(ctx); !g.Known || g.Refused != 9 {
		t.Errorf("under the renamed roles GeoRefusals is %+v", g)
	}

	// A section with one name left out keeps the other default.
	store.SetServiceRoles(ServiceRoles{Collector: "toplayici"})
	if got := store.serviceRoles(); got.Beacon != DefaultServiceRoles.Beacon {
		t.Errorf("an empty beacon role is %q, want the default", got.Beacon)
	}
}
