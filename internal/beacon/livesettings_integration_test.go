//go:build integration

// A5.1: does a setting that used to live in the config file actually
// come from the database now?
//
// Against a real settings table, because the thing being checked is the
// resolution order across two layers - and a stand-in for one of them
// would test the stand-in.

package beacon

import (
	"context"
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/limiter"
	"github.com/cruciblelab/crucible-analytic/internal/settings"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// storeSetting writes a deployment-wide row the way the panel does.
//
// Written with raw SQL rather than through internal/panel because this
// package must not import it: the beacon is the one process the whole
// internet can POST to, and the panel's data layer has no business in
// it. The dependency pointing one way is the property; a test that
// imported the panel to save five lines would quietly undo it.
// The pool is the panel's, opened here rather than passed in: writing a
// setting is what the panel does, and the beacon's role holds SELECT on
// panel_settings and nothing more. Reading it back is the beacon's job
// and uses the beacon's pool, which is the whole point of these tests -
// one process writes the row, a different one with different privileges
// picks it up.
func storeSetting(t *testing.T, key string, value any) {
	t.Helper()
	pool := testdb.Pool(t, testdb.Panel)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(context.Background(), `
		INSERT INTO panel_settings (scope, site_id, key, value)
		VALUES ('global', '', $1, $2)
		ON CONFLICT (scope, site_id, key) DO UPDATE SET value = EXCLUDED.value`,
		key, encoded)
	if err != nil {
		t.Fatalf("storing %s: %v", key, err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM panel_settings WHERE key = $1 AND site_id = ''`, key)
	})
}

// settingsPool opens the pool and registers its close as a cleanup.
//
// Not `defer pool.Close()`, and the difference is not style. Deferred
// calls run when the function returns; t.Cleanup functions run after
// that. So a deferred close happens *first*, and every row-deleting
// cleanup registered by storeSetting then runs against a closed pool -
// silently, because the error is discarded.
//
// That is exactly what happened while writing this file: four rows
// survived the suite, and the next test read one of them and failed
// claiming the two services shared a setting. A cleanup that quietly
// does nothing is worse than no cleanup, because the suite goes on
// looking tidy.
func settingsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	// The beacon's own role. It may read panel_settings and write
	// nothing there, which is exactly what settings.Source needs.
	return testdb.Pool(t, testdb.Beacon)
}

func liveSource(t *testing.T, pool *pgxpool.Pool) *settings.Source {
	t.Helper()
	src := settings.New(context.Background(), pool, settings.Config{Interval: time.Minute})
	if !src.Loaded() {
		t.Fatal("the settings source never loaded; is the panel schema applied?")
	}
	return src
}

// TestTrustedProxiesComeFromTheDatabaseWhenItHasThem is the phase's
// headline claim for the catalogue's top entry.
func TestTrustedProxiesComeFromTheDatabaseWhenItHasThem(t *testing.T) {
	pool := settingsPool(t)

	// The file says one proxy - the state a deployment upgrades in.
	cfg := Config{TrustedProxies: []string{"10.0.0.1/32"}}

	// ---- nothing stored: the file still decides ----
	fromFile, err := cfg.LiveTrustedProxies(liveSource(t, pool))
	if err != nil {
		t.Fatalf("LiveTrustedProxies: %v", err)
	}
	if len(fromFile) != 1 || fromFile[0].String() != "10.0.0.1/32" {
		t.Fatalf("with nothing stored the list is %v; the file's value must survive", fromFile)
	}

	// ---- stored: the panel decides ----
	storeSetting(t, settings.KeyTrustedProxies,
		[]string{"173.245.48.0/20", "2400:cb00::/32"})

	fromPanel, err := cfg.LiveTrustedProxies(liveSource(t, pool))
	if err != nil {
		t.Fatalf("LiveTrustedProxies: %v", err)
	}
	want := []netip.Prefix{
		netip.MustParsePrefix("173.245.48.0/20"),
		netip.MustParsePrefix("2400:cb00::/32"),
	}
	if len(fromPanel) != len(want) {
		t.Fatalf("the stored list did not take effect: %v", fromPanel)
	}
	for i := range want {
		if fromPanel[i] != want[i] {
			t.Errorf("entry %d = %v, want %v", i, fromPanel[i], want[i])
		}
	}

	// ---- and it actually changes who is believed ----
	// The point of the setting, rather than the value of a variable.
	resolver := ClientIPResolver{TrustedProxies: fromPanel}
	if !resolver.trusts(netip.MustParseAddr("173.245.48.9")) {
		t.Error("a proxy the panel named is still not believed")
	}
	if resolver.trusts(netip.MustParseAddr("198.51.100.9")) {
		t.Error("an address nobody named is believed")
	}
}

// TestAStoredProxyListThatIsNotNetworksIsRefusedWhole.
//
// A row can be edited by hand, or written by an older build. Half a list
// would mean trusting fewer proxies than the operator believes - the
// same failure as an empty list, in a smaller size and harder to see.
func TestAStoredProxyListThatIsNotNetworksIsRefusedWhole(t *testing.T) {
	pool := settingsPool(t)

	cfg := Config{TrustedProxies: []string{"10.0.0.1/32"}}
	storeSetting(t, settings.KeyTrustedProxies,
		[]string{"173.245.48.0/20", "not-a-network"})

	if _, err := cfg.LiveTrustedProxies(liveSource(t, pool)); err == nil {
		t.Fatal("a stored list containing something that is not a network was accepted")
	}
}

// TestLimitsComeFromTheDatabaseFieldByField.
//
// Field by field is the property: raising a ceiling during an incident
// must not require restating the policy, and a deployment that has set
// one value in the panel must keep the file's values for the rest.
func TestLimitsComeFromTheDatabaseFieldByField(t *testing.T) {
	pool := settingsPool(t)

	cfg := LimitsConfig{
		MaxConcurrentRequests: 100,
		MaxRequestsPerSecond:  200,
		OverloadPolicy:        "fail_closed",
		ThrottleQueueSize:     50,
	}

	// ---- nothing stored ----
	got := cfg.LiveLimits(liveSource(t, pool))
	if got != (limiter.Config{
		MaxConcurrentConnections: 100, MaxRequestsPerSecond: 200,
		Policy: limiter.PolicyFailClosed, ThrottleQueueSize: 50,
	}) {
		t.Fatalf("with nothing stored the limits are %+v; the file's values must survive", got)
	}

	// ---- one field stored ----
	storeSetting(t, settings.KeyBeaconMaxConcurrent, 4000)
	got = cfg.LiveLimits(liveSource(t, pool))
	if got.MaxConcurrentConnections != 4000 {
		t.Errorf("the stored ceiling did not take effect: %d", got.MaxConcurrentConnections)
	}
	if got.MaxRequestsPerSecond != 200 || got.Policy != limiter.PolicyFailClosed || got.ThrottleQueueSize != 50 {
		t.Errorf("setting one field disturbed the others: %+v", got)
	}

	// ---- the policy, which is the one that can stop traffic ----
	storeSetting(t, settings.KeyBeaconOverloadPolicy, "throttle")
	got = cfg.LiveLimits(liveSource(t, pool))
	if got.Policy != limiter.PolicyThrottle {
		t.Errorf("policy = %q, want throttle", got.Policy)
	}
}

// TestTheCollectorsLimitsAreNotTheBeacons.
//
// The first draft of this phase gave both services one shared family,
// which would have been a number that cannot mean what it says: the
// collector sees every connection to the site, the beacon only the
// visitors whose browser ran the snippet. This is the test that the
// split is real rather than only in the names.
func TestTheCollectorsLimitsAreNotTheBeacons(t *testing.T) {
	pool := settingsPool(t)

	storeSetting(t, settings.KeyCollectorMaxConcurrent, 9000)

	beaconLimits := LimitsConfig{MaxConcurrentRequests: 100}.LiveLimits(liveSource(t, pool))
	if beaconLimits.MaxConcurrentConnections != 100 {
		t.Errorf("the collector's ceiling reached the beacon: %d",
			beaconLimits.MaxConcurrentConnections)
	}
}

// TestAnUnreachableDatabaseKeepsTheFilesValues.
//
// The failure mode internal/settings promises, checked from the caller's
// side: a settings source that never loaded resolves to the file, not to
// the built-in defaults. A deployment whose database blinks must not
// quietly lose its tuning.
func TestAnUnreachableDatabaseKeepsTheFilesValues(t *testing.T) {
	pool, err := pgxpool.New(context.Background(),
		"postgres://nobody:nobody@127.0.0.1:1/nothing?connect_timeout=1")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	// A plain defer is right here: this pool is deliberately dead, no
	// rows are written through it, and nothing registers a cleanup that
	// would need it open.
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	src := settings.New(ctx, pool, settings.Config{Interval: time.Minute})
	if src.Loaded() {
		t.Fatal("the source loaded from a database that does not exist")
	}

	cfg := Config{TrustedProxies: []string{"10.0.0.1/32"}}
	prefixes, err := cfg.LiveTrustedProxies(src)
	if err != nil {
		t.Fatalf("LiveTrustedProxies: %v", err)
	}
	if len(prefixes) != 1 || prefixes[0].String() != "10.0.0.1/32" {
		t.Errorf("the file's proxy list did not survive a dead database: %v", prefixes)
	}

	limits := LimitsConfig{MaxConcurrentRequests: 100, OverloadPolicy: "throttle"}.LiveLimits(src)
	if limits.MaxConcurrentConnections != 100 || limits.Policy != limiter.PolicyThrottle {
		t.Errorf("the file's limits did not survive a dead database: %+v", limits)
	}
}
