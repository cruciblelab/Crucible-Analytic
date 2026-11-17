//go:build integration

package logging_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/settings"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// The panel's three log ages, read by a real settings.Source from a real
// panel_settings, as the role a service runs as.
//
// In its own schema, with a copy of the table derived by LIKE: these are
// deployment-wide keys, and other suites write logs.retention_days on the
// shared table while this one runs. Reading their row would make every
// assertion here a question about scheduling.

const liveSchema = "ca_logging_live"

type liveFixture struct {
	t     *testing.T
	admin *pgxpool.Pool
	src   *settings.Source
}

func newLiveFixture(t *testing.T) *liveFixture {
	t.Helper()
	ctx := context.Background()
	admin := testdb.Admin(t)
	for _, sql := range []string{
		`DROP SCHEMA IF EXISTS ` + liveSchema + ` CASCADE`,
		`CREATE SCHEMA ` + liveSchema,
		`CREATE TABLE ` + liveSchema + `.panel_settings (LIKE public.panel_settings INCLUDING ALL)`,
		`GRANT USAGE ON SCHEMA ` + liveSchema + ` TO ` + testdb.Beacon,
		`GRANT SELECT ON ` + liveSchema + `.panel_settings TO ` + testdb.Beacon,
	} {
		if _, err := admin.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(),
			`DROP SCHEMA IF EXISTS `+liveSchema+` CASCADE`); err != nil {
			t.Errorf("dropping %s: %v", liveSchema, err)
		}
	})

	pool, err := pgxpool.New(ctx, testdb.DSN(testdb.Beacon)+"?options=-csearch_path%3D"+liveSchema+",public")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	// The Source reads the copy and not the shared table. Without this
	// the assertions below could be answered by another suite's row.
	var where string
	if err := pool.QueryRow(ctx,
		`SELECT n.nspname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE c.oid = 'panel_settings'::regclass`).Scan(&where); err != nil {
		t.Fatalf("resolving panel_settings: %v", err)
	}
	if where != liveSchema {
		t.Fatalf("this pool resolves panel_settings to %q, not to the copy in %q", where, liveSchema)
	}

	return &liveFixture{t: t, admin: admin,
		src: settings.New(ctx, pool, settings.Config{Interval: time.Minute})}
}

// set stores key=value (a JSON literal) and refreshes the Source.
func (f *liveFixture) set(key, value string) {
	f.t.Helper()
	ctx := context.Background()
	if _, err := f.admin.Exec(ctx, `
		INSERT INTO `+liveSchema+`.panel_settings (scope, site_id, key, value)
		VALUES ('global', '', $1, $2::jsonb)
		ON CONFLICT (scope, site_id, key) DO UPDATE SET value = EXCLUDED.value`,
		key, value); err != nil {
		f.t.Fatalf("writing %s: %v", key, err)
	}
	if err := f.src.Refresh(ctx); err != nil {
		f.t.Fatalf("Refresh: %v", err)
	}
}

// Each of the three keys reaches its own field, a key with no row leaves
// the file's number - negative included - and a stored value outside
// the bounds is no value.
//
// The three file numbers and the three stored ones are all different, so
// a key wired to the wrong field reads as a wrong number rather than as
// the right one by coincidence.
func TestLiveLifecycle_ThePanelWinsFieldByField(t *testing.T) {
	f := newLiveFixture(t)
	file := logging.Lifecycle{ArchiveAfterDays: 6, RetentionDays: 21, ImportantRetentionDays: -1}

	if got := logging.LiveLifecycle(f.src, file); got != file {
		t.Fatalf("with no rows the policy is %+v, want the file's %+v", got, file)
	}

	f.set(settings.KeyLogRetentionDays, "2")
	want := file
	want.RetentionDays = 2
	if got := logging.LiveLifecycle(f.src, file); got != want {
		t.Errorf("with retention stored the policy is %+v, want %+v", got, want)
	}

	f.set(settings.KeyLogArchiveAfterDays, "3")
	f.set(settings.KeyLogImportantRetentionDays, "400")
	want = logging.Lifecycle{ArchiveAfterDays: 3, RetentionDays: 2, ImportantRetentionDays: 400}
	if got := logging.LiveLifecycle(f.src, file); got != want {
		t.Errorf("with all three stored the policy is %+v, want %+v", got, want)
	}

	// Zero is below the floor: a row that said it would mean "never
	// delete", which only the config file may say.
	f.set(settings.KeyLogRetentionDays, "0")
	if got := logging.LiveLifecycle(f.src, file).RetentionDays; got != file.RetentionDays {
		t.Errorf("a stored 0 gave retention %d, want the file's %d", got, file.RetentionDays)
	}
	f.set(settings.KeyLogRetentionDays, "3651")
	if got := logging.LiveLifecycle(f.src, file).RetentionDays; got != file.RetentionDays {
		t.Errorf("a stored 3651 gave retention %d, want the file's %d", got, file.RetentionDays)
	}
	// And the bounds themselves are inside.
	f.set(settings.KeyLogRetentionDays, "3650")
	if got := logging.LiveLifecycle(f.src, file).RetentionDays; got != 3650 {
		t.Errorf("a stored 3650 gave retention %d, want 3650", got)
	}
	f.set(settings.KeyLogRetentionDays, "1")
	if got := logging.LiveLifecycle(f.src, file).RetentionDays; got != 1 {
		t.Errorf("a stored 1 gave retention %d, want 1", got)
	}
}

// The whole path a running service takes: a row written after startup,
// a refresh, KeepLive - and the file is gone, with a line saying so.
func TestKeepLive_ARowWrittenAfterStartupReachesTheFiles(t *testing.T) {
	f := newLiveFixture(t)
	root := t.TempDir()
	_, controls, closeLogs, err := logging.Setup("w4a-live", logging.Config{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer closeLogs()

	day := time.Now().UTC().AddDate(0, 0, -3).Format("2006-01-02")
	dir := filepath.Join(root, "w4a-live", day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "access.log")
	if err := os.WriteFile(old, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var said strings.Builder
	logger := slog.New(slog.NewTextHandler(&said, nil))

	logging.KeepLive(controls, f.src, logger, time.Now())
	if _, err := os.Stat(old); err != nil {
		t.Fatalf("with no row the 3-day-old access log went (%v); the fixture tests nothing", err)
	}

	f.set(settings.KeyLogRetentionDays, "2")
	logging.KeepLive(controls, f.src, logger, time.Now())
	if _, err := os.Stat(old); err == nil {
		t.Error("retention 2 stored and read, and the 3-day-old access log is still there")
	}
	if !strings.Contains(said.String(), "logging: maintenance") || !strings.Contains(said.String(), "deleted=1") {
		t.Errorf("the pass said %q, want a maintenance line with deleted=1", said.String())
	}
}

// A service whose first read of the settings failed does not know the
// policy, and deletes nothing until it does - not even what the file's
// own shorter figure would delete. It still archives.
//
// The database is unreachable on purpose: a pool on a port nothing
// listens on is the state of a service started while PostgreSQL is down.
func TestKeepLive_NothingIsDeletedBeforeTheSettingsAreRead(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, "postgres://beacon_writer:x@127.0.0.1:1/analytics?connect_timeout=2")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	src := settings.New(ctx, pool, settings.Config{Logger: slog.New(slog.DiscardHandler)})
	if src.Loaded() {
		t.Fatal("a source on an unreachable database reports it has read the settings")
	}

	root := t.TempDir()
	_, controls, closeLogs, err := logging.Setup("w4a-down", logging.Config{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer closeLogs()
	day := time.Now().UTC().AddDate(0, 0, -30).Format("2006-01-02")
	dir := filepath.Join(root, "w4a-down", day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(dir, "access.log")
	if err := os.WriteFile(old, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logging.KeepLive(controls, src, slog.New(slog.DiscardHandler), time.Now())
	if _, err := os.Stat(old + ".gz"); err != nil {
		t.Errorf("a 30-day-old access log was not archived while the policy was unknown: %v", err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("the plain file is still there beside its archive")
	}
}
