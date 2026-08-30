//go:build integration

// Real coverage of the preflight checks against a live PostgreSQL. Half
// of what this package does is assert what a *different* database role
// may and may not do, and none of that can be faked - a mock that
// answers has_table_privilege is a mock of the thing under test. Run
// with:
//
//	docker compose up -d
//	./release/install.sh   # see internal/testdb for the whole recipe
//	go test -tags integration ./internal/panel/preflight/ -v

package preflight

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"testing"

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

// adminPool is a connection that owns the schema, for the one test that
// has to change it.
//
// Skipped rather than failed when unset: a machine that can run the rest
// of this suite as panel_user may have no superuser connection to offer,
// and a test that cannot run is not a test that failed.
func adminPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("CA_SUPERUSER_DSN")
	if dsn == "" {
		t.Skip("set CA_SUPERUSER_DSN to a connection that owns the schema; this test alters a column")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New (superuser): %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newTestChecker connects a Checker to the test database.
//
// It takes a pool and nothing else, which is the point of the split: the
// old version of these tests built a whole panel Store - users,
// sessions, audit cleanup - to ask whether a role can read a table.
func newTestChecker(t *testing.T) *Checker {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), testDatabaseURL)
	if err != nil {
		t.Fatalf("pgxpool.New: %v (is the database up and installed? see internal/testdb)", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		pool.Close()
		t.Fatalf("ping: %v (is the database up and installed? see internal/testdb)", err)
	}
	t.Cleanup(pool.Close)
	return New(pool, false)
}

func find(t *testing.T, results []CheckResult, id string) CheckResult {
	t.Helper()
	for _, r := range results {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("no check with id %q in %d results", id, len(results))
	return CheckResult{}
}

// The schema checks run against the database this suite is already
// using, which has every schema file applied - so they must pass.
func TestPreflight_PassesAgainstAProperlySetUpDatabase(t *testing.T) {
	c := newTestChecker(t)
	results := c.Run(context.Background(), Config{})

	for _, id := range []string{"schema.panel", "schema.analytics", "schema.columns"} {
		if got := find(t, results, id); got.Status != CheckPass {
			t.Errorf("%s = %s: %s", id, got.Status, got.Detail)
		}
	}
}

// The check that exists because CREATE TABLE IF NOT EXISTS does nothing
// to an existing table - the failure this project has already had once.
func TestPreflight_DetectsASchemaFileThatWasNeverReapplied(t *testing.T) {
	c := newTestChecker(t)
	ctx := context.Background()

	// Drop a self-migrating column, as a deployment that never re-ran the
	// schema file would look.
	//
	// On a second connection, as whoever owns the schema. The checker
	// stays as panel_user - that is the point of this suite - and
	// panel_user cannot ALTER anything, which is correct and is exactly
	// why the arrangement has to be two connections rather than one.
	// Doing the DDL through the checker's own pool is what the first
	// version did, and it worked only because the development database
	// let the panel own the beacon's table.
	admin := adminPool(t)
	if _, err := admin.Exec(ctx, `ALTER TABLE beacon_events DROP COLUMN IF EXISTS click_source`); err != nil {
		t.Fatalf("dropping column: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(),
			`ALTER TABLE beacon_events ADD COLUMN IF NOT EXISTS click_source TEXT NOT NULL DEFAULT ''`); err != nil {
			t.Errorf("putting click_source back: %v - the database is now missing a column", err)
		}
	})

	got := find(t, c.Run(ctx, Config{}), "schema.columns")
	if got.Status != CheckFail {
		t.Fatalf("status = %s, want fail; the missing column went unnoticed", got.Status)
	}
	if got.Fix == "" {
		t.Error("a failing check offered no command to fix it")
	}
}

// The isolation the whole design rests on. A deployment where somebody
// granted a little too much looks healthy until it matters.
func TestPreflight_CatchesAPanelRoleThatCanReadAnalytics(t *testing.T) {
	c := newTestChecker(t)
	ctx := context.Background()

	// Pointed at `collector`, which legitimately holds SELECT on
	// traffic_snapshots - so a deployment that named it as the panel's
	// role would be one where the panel can read analytics, and this
	// check must fail. That is the check working, not the deployment
	// being wrong.
	//
	// It has to name another role rather than rely on this suite's own,
	// which is now panel_user and correctly holds nothing there.
	got := find(t, c.Run(ctx, Config{
		Roles: Roles{Panel: "collector"},
	}), "grants.panel_isolation")

	if got.Status != CheckFail {
		t.Errorf("status = %s, want fail for a role that can read the analytics tables: %s", got.Status, got.Detail)
	}
	if got.Severity != SeverityRequired {
		t.Errorf("severity = %s, want required; this must block handover", got.Severity)
	}
}

// A role that does not exist is "not applicable", not an error: plenty
// of deployments will not have separated every role yet.
func TestPreflight_TreatsAMissingRoleAsNoPrivilege(t *testing.T) {
	c := newTestChecker(t)
	got := find(t, c.Run(context.Background(), Config{
		Roles: Roles{Panel: "no_such_role_anywhere"},
	}), "grants.panel_isolation")

	if got.Status != CheckPass {
		t.Errorf("status = %s, want pass for a role that does not exist: %s", got.Status, got.Detail)
	}
}

func TestPreflight_UnconfiguredChecksSkipRatherThanFail(t *testing.T) {
	c := newTestChecker(t)
	results := c.Run(context.Background(), Config{})

	// "We did not look" and "we looked and it was fine" are different
	// facts, and this project keeps them apart everywhere else too.
	for _, id := range []string{"logs.writable", "disk.free"} {
		if got := find(t, results, id); got.Status != CheckSkip {
			t.Errorf("%s = %s, want skip when nothing was configured", id, got.Status)
		}
	}
}

func TestPreflight_ChecksServicesThatWereGiven(t *testing.T) {
	c := newTestChecker(t)

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()

	results := c.Run(context.Background(), Config{
		ServiceURLs: map[string]string{
			"beacon":    healthy.URL,
			"collector": broken.URL,
			"api":       "http://127.0.0.1:1/healthz", // nothing listens here
		},
	})

	if got := find(t, results, "service.beacon"); got.Status != CheckPass {
		t.Errorf("beacon = %s: %s", got.Status, got.Detail)
	}
	if got := find(t, results, "service.collector"); got.Status != CheckFail {
		t.Errorf("collector = %s, want fail for HTTP 500", got.Status)
	}
	if got := find(t, results, "service.api"); got.Status != CheckFail {
		t.Errorf("api = %s, want fail for a refused connection", got.Status)
	}
}

func TestPreflight_LogDirectoryIsProbedRatherThanInspected(t *testing.T) {
	c := newTestChecker(t)
	ctx := context.Background()

	dir := t.TempDir()
	// A mode that would look right but is too open for data carrying IP
	// addresses.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	got := find(t, c.Run(ctx, Config{LogDir: dir}), "logs.writable")
	if got.Status != CheckWarn {
		t.Errorf("status = %s, want warn for a world-readable log directory: %s", got.Status, got.Detail)
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	got = find(t, c.Run(ctx, Config{LogDir: dir}), "logs.writable")
	if got.Status != CheckPass {
		t.Errorf("status = %s, want pass for a 0700 writable directory: %s", got.Status, got.Detail)
	}
}

// Handover is blocked by required failures and not by recommended ones.
func TestPreflightComplete_BlocksOnRequiredFailuresOnly(t *testing.T) {
	results := []CheckResult{
		{ID: "a", Severity: SeverityRequired, Status: CheckPass},
		{ID: "b", Severity: SeverityRecommended, Status: CheckWarn},
		{ID: "c", Severity: SeverityRecommended, Status: CheckFail},
	}
	if ok, blocking := Complete(results); !ok {
		t.Errorf("blocked by %+v; only required failures should block", blocking)
	}

	results = append(results, CheckResult{ID: "d", Severity: SeverityRequired, Status: CheckFail})
	ok, blocking := Complete(results)
	if ok {
		t.Error("completed despite a required check failing")
	}
	if len(blocking) != 1 || blocking[0].ID != "d" {
		t.Errorf("blocking = %+v, want exactly the required failure", blocking)
	}
}

// Backups cannot be checked, and saying so is the only honest option.
func TestPreflight_BackupCheckIsHonestAboutNotBeingAbleToCheck(t *testing.T) {
	c := newTestChecker(t)
	got := find(t, c.Run(context.Background(), Config{}), "backup.configured")
	if got.Status != CheckWarn {
		t.Errorf("status = %s, want warn - reporting a pass would be a lie and a fail would "+
			"block handover on something the installer may have handled themselves", got.Status)
	}
	if got.Fix == "" {
		t.Error("the backup warning offers no command")
	}
}

// The installer reads top-down and should meet the problem before the
// reassurance.
func TestPreflight_WorstResultsComeFirst(t *testing.T) {
	c := newTestChecker(t)
	results := c.Run(context.Background(), Config{
		ServiceURLs: map[string]string{"api": "http://127.0.0.1:1/healthz"},
	})
	if len(results) == 0 {
		t.Fatal("no results")
	}
	if results[0].Status != CheckFail {
		t.Errorf("first result is %s; a failure exists and should be at the top", results[0].Status)
	}
}

// Every check a manual step claims to be verified by must actually
// exist, or the wizard tells the installer something is checked when
// nothing checks it.
func TestManualSteps_ReferenceRealChecks(t *testing.T) {
	c := newTestChecker(t)
	results := c.Run(context.Background(), Config{
		ServiceURLs: map[string]string{"collector": "http://127.0.0.1:1", "beacon": "http://127.0.0.1:1", "api": "http://127.0.0.1:1"},
		Roles:       Roles{Panel: "nobody", API: "nobody"},
		LogDir:      t.TempDir(),
		DataDir:     t.TempDir(),
	})
	known := map[string]bool{}
	for _, r := range results {
		known[r.ID] = true
	}

	for _, step := range ManualSteps() {
		if step.CheckedBy == "" {
			continue
		}
		for _, id := range strings.Split(step.CheckedBy, ", ") {
			if !known[id] {
				t.Errorf("manual step %q says it is verified by %q, but no such check exists", step.ID, id)
			}
		}
	}
}

// Every step must say why the panel cannot do it, or the list reads as
// an arbitrary set of chores.
func TestManualSteps_EachExplainsWhyItCannotBeAutomated(t *testing.T) {
	for _, step := range ManualSteps() {
		if step.Why == "" {
			t.Errorf("manual step %q offers no reason it cannot be done from the panel", step.ID)
		}
		if step.Label == "" {
			t.Errorf("manual step %q has no label", step.ID)
		}
	}
}

func TestUncheckedSteps_AreTheOnesWithoutACheck(t *testing.T) {
	unchecked := UncheckedSteps()
	if len(unchecked) == 0 {
		t.Fatal("no unchecked steps; some of these genuinely cannot be verified")
	}
	for _, step := range unchecked {
		if step.CheckedBy != "" {
			t.Errorf("%q is listed as unchecked but names a checker", step.ID)
		}
	}
	// Backups are the clearest case: nothing in this system can see them.
	found := false
	for _, step := range unchecked {
		if step.ID == "backup" {
			found = true
		}
	}
	if !found {
		t.Error("backups are not listed as unverifiable, but nothing here can check them")
	}
}

// A role nobody created means this deployment never split its roles, so
// the grant check does not apply. Reporting "beacon_writer cannot read
// settings" about a role that does not exist would send an installer
// looking for a grant on nothing.
func TestPreflight_SettingsGrantSkipsWhenRolesWereNeverSeparated(t *testing.T) {
	c := newTestChecker(t)
	got := find(t, c.Run(context.Background(), Config{
		Roles: Roles{Beacon: "no_such_beacon_role", Collector: "no_such_collector_role"},
	}), "grants.live_settings")

	if got.Status != CheckSkip {
		t.Errorf("status = %s, want skip for roles that do not exist: %s", got.Status, got.Detail)
	}
}

// A misspelled role makes both isolation checks silently inapplicable:
// has_table_privilege reports no privileges for a role nobody created,
// so "cannot read analytics" passes for a role that does not exist. The
// isolation would look verified when nothing was verified.
func TestPreflight_WarnsAboutARoleNameThatDoesNotExist(t *testing.T) {
	c := newTestChecker(t)
	got := find(t, c.Run(context.Background(), Config{
		Roles: Roles{Panel: "panl_usr_typo", API: "collector"},
	}), "grants.roles_exist")

	if got.Status != CheckWarn {
		t.Errorf("status = %s, want warn for a misspelled role: %s", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "panl_usr_typo") {
		t.Errorf("detail does not name the missing role: %s", got.Detail)
	}
}

// Logs and the database routinely live on different volumes, and the log
// volume is the one that fills first.
func TestPreflight_MeasuresBothVolumes(t *testing.T) {
	c := newTestChecker(t)
	got := find(t, c.Run(context.Background(), Config{
		DataDir: "/", LogDir: t.TempDir(),
	}), "disk.free")

	if got.Status != CheckPass {
		t.Fatalf("status = %s: %s", got.Status, got.Detail)
	}
	for _, want := range []string{"veri", "kayıt"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail does not report the %q volume: %s", want, got.Detail)
		}
	}
}

// TestTheBackgroundJobCheckCatchesAPlantedJob.
//
// This is the H5 finding, kept honest. TimescaleDB grants EXECUTE on
// add_job() to PUBLIC, so before harden.sql any service role could
// schedule work that outlives the process which scheduled it. Closing
// the grant is one thing; noticing a job that is already there is
// another, and an upgrade that reinstalls the extension restores the
// default grants without saying so.
//
// The job is planted with the test's own connection and removed
// afterwards, so what is measured is the check rather than a fixture.
func TestTheBackgroundJobCheckCatchesAPlantedJob(t *testing.T) {
	checker := newTestChecker(t)
	ctx := context.Background()

	// Clean before, in case a previous run died between planting and
	// removing - a left-over job would make the "none" case below fail
	// for a reason that has nothing to do with this run.
	dropTestJobs(t, checker)

	before := find(t, checker.Run(ctx, Config{}), "db.no_background_jobs")
	if before.Status != CheckPass {
		t.Fatalf("with no jobs the check is %s: %s", before.Status, before.Detail)
	}

	var jobID int
	err := checker.pool.QueryRow(ctx,
		`SELECT add_job('pg_sleep', '1 hour')`).Scan(&jobID)
	if err != nil {
		// The grant may already be revoked on this database, which is
		// the state harden.sql leaves - and then there is nothing to
		// plant. Reported rather than skipped silently: a test that
		// cannot run is a fact worth printing.
		t.Skipf("could not plant a job (harden.sql may already be applied here): %v", err)
	}
	t.Cleanup(func() { dropTestJobs(t, checker) })

	// The planted job is owned by whoever this test connects as, which
	// is one of the four service roles - so the check must see it.
	after := find(t, checker.Run(ctx, Config{}), "db.no_background_jobs")
	if after.Status != CheckFail {
		t.Errorf("a planted job left the check at %s: %s", after.Status, after.Detail)
	}
	if !strings.Contains(after.Detail, "pg_sleep") {
		t.Errorf("the check does not name the job it found: %s", after.Detail)
	}
	if after.Fix == "" {
		t.Error("the check reports a planted job without saying how to remove it")
	}

	dropTestJobs(t, checker)
	final := find(t, checker.Run(ctx, Config{}), "db.no_background_jobs")
	if final.Status != CheckPass {
		t.Errorf("after removing the job the check is still %s: %s", final.Status, final.Detail)
	}
}

func dropTestJobs(t *testing.T, c *Checker) {
	t.Helper()
	_, _ = c.pool.Exec(context.Background(), `
		SELECT delete_job(job_id) FROM timescaledb_information.jobs
		WHERE owner::text IN ('collector','beacon_writer','analytics_reader','panel_user')`)
}

// The encryption check has three answers and this asserts whichever one
// the database it is actually pointed at deserves.
//
// The previous version of this test asserted the local one, on the
// strength of a comment that said "the connection this suite uses is to
// localhost". That was an assumption, written down and never checked,
// and it was false in the one place it mattered: on CI the database is a
// service container on a bridge network, so the check correctly warned
// and the test correctly failed - for two months, one of two independent
// reasons the merge gate was red.
//
// The lesson is the one this project keeps relearning from the other
// direction: a test that assumes its environment tests the environment
// it assumed. So the environment is read, not assumed, and all three
// routes are covered rather than the one that happened to be true on a
// laptop.
//
// The two passing routes matter separately, because only one of them is
// about encryption. A check that reported "encrypted" for a loopback
// connection would be telling every single-machine deployment something
// untrue.
func TestConnectionEncryptionMatchesWhereTheDatabaseIs(t *testing.T) {
	checker := newTestChecker(t)
	ctx := context.Background()

	// The same two facts the check reads, read here independently so
	// this asserts the mapping from facts to verdict rather than
	// re-deriving the verdict.
	var encrypted bool
	var serverAddr *string
	if err := checker.pool.QueryRow(ctx, `
		SELECT coalesce((SELECT ssl FROM pg_stat_ssl WHERE pid = pg_backend_pid()), false),
		       host(inet_server_addr())`).Scan(&encrypted, &serverAddr); err != nil {
		t.Fatalf("reading the connection's own state: %v", err)
	}

	loopback := serverAddr == nil
	if !loopback {
		addr, err := netip.ParseAddr(*serverAddr)
		loopback = err == nil && (addr.IsLoopback() || addr.IsUnspecified())
	}

	got := find(t, checker.Run(ctx, Config{}), "db.connection_encrypted")

	switch {
	case encrypted:
		if got.Status != CheckPass {
			t.Errorf("a TLS connection gave %s: %s", got.Status, got.Detail)
		}
		if !strings.Contains(got.Detail, "TLS") {
			t.Errorf("the detail does not say the connection is encrypted: %s", got.Detail)
		}

	case loopback:
		if got.Status != CheckPass {
			t.Errorf("a loopback connection gave %s: %s", got.Status, got.Detail)
		}
		if !strings.Contains(got.Detail, "bu makinede") {
			t.Errorf("the detail does not give the local reason: %s", got.Detail)
		}
		// And it must not claim encryption, which is the whole point of
		// keeping the two passes apart.
		if strings.Contains(got.Detail, "TLS ile şifreli") {
			t.Errorf("a loopback connection was reported as encrypted: %s", got.Detail)
		}

	default:
		// Remote and in the clear. A warning, and it has to name the
		// address, because "somewhere remote" is not something an
		// operator can act on.
		if got.Status != CheckWarn {
			t.Errorf("an unencrypted remote connection to %s gave %s: %s",
				*serverAddr, got.Status, got.Detail)
		}
		if !strings.Contains(got.Detail, *serverAddr) {
			t.Errorf("the warning does not name the server address %s: %s", *serverAddr, got.Detail)
		}
	}
}
