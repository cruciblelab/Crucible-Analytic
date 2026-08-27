//go:build integration

package settings

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The role the panel actually runs as.
//
// It was `collector` until an end-to-end run of the installed package
// showed why that mattered: the development database had been created by
// collector, so collector owned every table and this suite ran with
// authority no deployment grants it. Three real holes were hiding behind
// that - a retention feature that had never worked, two ungranted
// tables - and none of them could have been caught from here.
//
// A suite that tests a role-separated design has to connect as the role.
const testDatabaseURL = "postgres://panel_user:panel_user@localhost:5432/analytics"

// testKeyPrefix namespaces every row this suite writes.
//
// panel_settings is one table shared by every integration suite, and
// `go test ./...` runs packages in parallel - so this one and
// internal/panel's were writing the same real keys into the same rows at
// the same time, and each wiping the whole table on the way out. It
// failed intermittently, which is the worst way for it to fail: a suite
// that is red once a morning is a suite people stop reading.
//
// Nothing here depends on the real key names. Source takes keys as
// plain strings with caller-supplied bounds and defaults, so a
// namespaced key exercises exactly the same code - and now says out loud
// that this suite is not testing the panel's key list. The panel owns
// every unprefixed key; this suite owns "test.".
const testKeyPrefix = "test.settings."

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("pgxpool.New: %v (is the database up and installed? see internal/testdb)", err)
	}
	t.Cleanup(pool.Close)
	t.Cleanup(func() {
		fresh, err := pgxpool.New(context.Background(), testDatabaseURL)
		if err != nil {
			return
		}
		defer fresh.Close()
		// Scoped to this suite's own rows. A bare DELETE here would take
		// internal/panel's rows out from under it mid-run.
		_, _ = fresh.Exec(context.Background(),
			`DELETE FROM panel_settings WHERE key LIKE $1`, testKeyPrefix+"%")
	})
	return pool
}

func write(t *testing.T, pool *pgxpool.Pool, key, site, jsonValue string) {
	t.Helper()
	scope := "global"
	if site != "" {
		scope = "site"
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO panel_settings (scope, site_id, key, value)
		VALUES ($1, $2, $3, $4::jsonb)
		ON CONFLICT (scope, site_id, key) DO UPDATE SET value = EXCLUDED.value`,
		scope, site, key, jsonValue)
	if err != nil {
		t.Fatalf("writing %s: %v", key, err)
	}
}

func TestSource_ReadsWhatThePanelWrote(t *testing.T) {
	pool := testPool(t)
	write(t, pool, "test.settings.logs.retention_days", "", "30")

	src := New(context.Background(), pool, Config{})
	if got := src.Int("test.settings.logs.retention_days", "", 14, 1, 3650); got != 30 {
		t.Errorf("got %d, want the stored 30", got)
	}
	if !src.Loaded() {
		t.Error("Loaded() is false after a successful read")
	}
}

func TestSource_FallsBackWhenNothingIsStored(t *testing.T) {
	pool := testPool(t)
	src := New(context.Background(), pool, Config{})
	if got := src.Int("test.settings.logs.retention_days", "", 14, 1, 3650); got != 14 {
		t.Errorf("got %d, want the caller's fallback 14", got)
	}
}

// The same resolution the panel performs, or the panel would show one
// value and the service would use another.
func TestSource_SiteValueOverridesTheGlobalOne(t *testing.T) {
	pool := testPool(t)
	write(t, pool, "test.settings.logs.retention_days", "", "90")
	write(t, pool, "test.settings.logs.retention_days", "site-a", "30")

	src := New(context.Background(), pool, Config{})
	if got := src.Int("test.settings.logs.retention_days", "site-a", 90, 1, 3650); got != 30 {
		t.Errorf("site-a got %d, want its own 30", got)
	}
	if got := src.Int("test.settings.logs.retention_days", "site-b", 90, 1, 3650); got != 90 {
		t.Errorf("site-b got %d, want the global 90", got)
	}
}

// A row written by an older build, or edited by hand, must not hand a
// running service a value outside what it was written against.
func TestSource_RefusesAnOutOfBoundsStoredValue(t *testing.T) {
	pool := testPool(t)
	write(t, pool, "test.settings.logs.retention_days", "", "99999")

	src := New(context.Background(), pool, Config{})
	if got := src.Int("test.settings.logs.retention_days", "", 14, 1, 3650); got != 14 {
		t.Errorf("got %d, want the fallback 14 for an out-of-bounds row", got)
	}
}

func TestSource_RefusesAnEnumValueOutsideItsSet(t *testing.T) {
	pool := testPool(t)
	write(t, pool, "test.settings.logs.level", "", `"verbose"`)

	src := New(context.Background(), pool, Config{})
	got := src.String("test.settings.logs.level", "", "info", []string{"debug", "info", "warn", "error"})
	if got != "info" {
		t.Errorf("got %q, want the fallback for a value outside the enum", got)
	}
}

// The decided failure mode: keep the last known values, never reset to
// defaults. A service that reset a customer's tuning during a database
// blip would be worse than one running on a stale value, and far harder
// to notice.
func TestSource_KeepsLastKnownValuesWhenTheDatabaseGoesAway(t *testing.T) {
	pool := testPool(t)
	write(t, pool, "test.settings.logs.retention_days", "", "45")

	src := New(context.Background(), pool, Config{})
	if got := src.Int("test.settings.logs.retention_days", "", 14, 1, 3650); got != 45 {
		t.Fatalf("got %d before the outage, want 45", got)
	}

	// Simulate the database becoming unreachable.
	pool.Close()
	if err := src.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh succeeded against a closed pool")
	}

	if got := src.Int("test.settings.logs.retention_days", "", 14, 1, 3650); got != 45 {
		t.Errorf("got %d after the outage; the last known value must survive, "+
			"not be reset to the default", got)
	}
	if src.Failures() != 1 {
		t.Errorf("Failures() = %d, want 1", src.Failures())
	}
}

// A read that fails halfway must not leave some settings current and
// some stale, because the combination is a state nobody designed.
func TestSource_AFailedRefreshLeavesTheCacheWhole(t *testing.T) {
	pool := testPool(t)
	write(t, pool, "test.settings.logs.retention_days", "", "20")
	write(t, pool, "test.settings.logs.archive_after_days", "", "3")

	src := New(context.Background(), pool, Config{})
	pool.Close()
	_ = src.Refresh(context.Background())

	if got := src.Int("test.settings.logs.retention_days", "", 14, 1, 3650); got != 20 {
		t.Errorf("retention = %d, want 20", got)
	}
	if got := src.Int("test.settings.logs.archive_after_days", "", 7, 1, 3650); got != 3 {
		t.Errorf("archive = %d, want 3", got)
	}
}

// The point of the whole package: a change made in the panel reaches a
// running service without a restart.
func TestSource_PicksUpAChangeWithoutRestarting(t *testing.T) {
	pool := testPool(t)
	write(t, pool, "test.settings.logs.retention_days", "", "10")

	src := New(context.Background(), pool, Config{Interval: 50 * time.Millisecond})
	if got := src.Int("test.settings.logs.retention_days", "", 14, 1, 3650); got != 10 {
		t.Fatalf("got %d, want 10", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go src.Run(ctx)

	// The panel writes a new value.
	write(t, pool, "test.settings.logs.retention_days", "", "60")

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if src.Int("test.settings.logs.retention_days", "", 14, 1, 3650) == 60 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("the change never reached the running service; still %d",
		src.Int("test.settings.logs.retention_days", "", 14, 1, 3650))
}

func TestSource_StringsReturnsACopy(t *testing.T) {
	pool := testPool(t)
	write(t, pool, "test.settings.logs.level", "", `["a","b"]`)

	src := New(context.Background(), pool, Config{})
	first := src.Strings("test.settings.logs.level", "", nil)
	if len(first) != 2 {
		t.Fatalf("got %v, want two entries", first)
	}
	first[0] = "mutated"

	second := src.Strings("test.settings.logs.level", "", nil)
	if second[0] != "a" {
		t.Errorf("mutating a returned slice changed the cache: %v", second)
	}
}
