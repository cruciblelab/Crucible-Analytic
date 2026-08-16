//go:build integration

package panel

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

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
	store := newTestStore(t, "preflight")
	results := store.RunPreflight(context.Background(), PreflightConfig{})

	for _, id := range []string{"schema.panel", "schema.analytics", "schema.columns"} {
		if got := find(t, results, id); got.Status != CheckPass {
			t.Errorf("%s = %s: %s", id, got.Status, got.Detail)
		}
	}
}

// The check that exists because CREATE TABLE IF NOT EXISTS does nothing
// to an existing table - the failure this project has already had once.
func TestPreflight_DetectsASchemaFileThatWasNeverReapplied(t *testing.T) {
	store := newTestStore(t, "preflight-columns")
	ctx := context.Background()

	// Drop a self-migrating column, as a deployment that never re-ran the
	// schema file would look.
	if _, err := store.Pool().Exec(ctx, `ALTER TABLE beacon_events DROP COLUMN IF EXISTS click_source`); err != nil {
		t.Fatalf("dropping column: %v", err)
	}
	t.Cleanup(func() {
		fresh, err := NewStore(context.Background(), testDatabaseURL)
		if err != nil {
			return
		}
		defer fresh.Close()
		_, _ = fresh.Pool().Exec(context.Background(),
			`ALTER TABLE beacon_events ADD COLUMN IF NOT EXISTS click_source TEXT NOT NULL DEFAULT ''`)
	})

	got := find(t, store.RunPreflight(ctx, PreflightConfig{}), "schema.columns")
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
	store := newTestStore(t, "preflight-isolation")
	ctx := context.Background()

	// The suite's own role is the superuser-ish `collector`, which by
	// construction can read everything - so pointing the check at it must
	// fail. That is the check working, not the deployment being wrong.
	got := find(t, store.RunPreflight(ctx, PreflightConfig{
		Roles: PreflightRoles{Panel: "collector"},
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
	store := newTestStore(t, "preflight-missing-role")
	got := find(t, store.RunPreflight(context.Background(), PreflightConfig{
		Roles: PreflightRoles{Panel: "no_such_role_anywhere"},
	}), "grants.panel_isolation")

	if got.Status != CheckPass {
		t.Errorf("status = %s, want pass for a role that does not exist: %s", got.Status, got.Detail)
	}
}

func TestPreflight_UnconfiguredChecksSkipRatherThanFail(t *testing.T) {
	store := newTestStore(t, "preflight-skip")
	results := store.RunPreflight(context.Background(), PreflightConfig{})

	// "We did not look" and "we looked and it was fine" are different
	// facts, and this project keeps them apart everywhere else too.
	for _, id := range []string{"logs.writable", "disk.free"} {
		if got := find(t, results, id); got.Status != CheckSkip {
			t.Errorf("%s = %s, want skip when nothing was configured", id, got.Status)
		}
	}
}

func TestPreflight_ChecksServicesThatWereGiven(t *testing.T) {
	store := newTestStore(t, "preflight-services")

	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer healthy.Close()
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()

	results := store.RunPreflight(context.Background(), PreflightConfig{
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
	store := newTestStore(t, "preflight-logs")
	ctx := context.Background()

	dir := t.TempDir()
	// A mode that would look right but is too open for data carrying IP
	// addresses.
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	got := find(t, store.RunPreflight(ctx, PreflightConfig{LogDir: dir}), "logs.writable")
	if got.Status != CheckWarn {
		t.Errorf("status = %s, want warn for a world-readable log directory: %s", got.Status, got.Detail)
	}

	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	got = find(t, store.RunPreflight(ctx, PreflightConfig{LogDir: dir}), "logs.writable")
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
	if ok, blocking := PreflightComplete(results); !ok {
		t.Errorf("blocked by %+v; only required failures should block", blocking)
	}

	results = append(results, CheckResult{ID: "d", Severity: SeverityRequired, Status: CheckFail})
	ok, blocking := PreflightComplete(results)
	if ok {
		t.Error("completed despite a required check failing")
	}
	if len(blocking) != 1 || blocking[0].ID != "d" {
		t.Errorf("blocking = %+v, want exactly the required failure", blocking)
	}
}

// Backups cannot be checked, and saying so is the only honest option.
func TestPreflight_BackupCheckIsHonestAboutNotBeingAbleToCheck(t *testing.T) {
	store := newTestStore(t, "preflight-backup")
	got := find(t, store.RunPreflight(context.Background(), PreflightConfig{}), "backup.configured")
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
	store := newTestStore(t, "preflight-order")
	results := store.RunPreflight(context.Background(), PreflightConfig{
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
	store := newTestStore(t, "manual-steps")
	results := store.RunPreflight(context.Background(), PreflightConfig{
		ServiceURLs: map[string]string{"collector": "http://127.0.0.1:1", "beacon": "http://127.0.0.1:1", "api": "http://127.0.0.1:1"},
		Roles:       PreflightRoles{Panel: "nobody", API: "nobody"},
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
	store := newTestStore(t, "preflight-noroles")
	got := find(t, store.RunPreflight(context.Background(), PreflightConfig{
		Roles: PreflightRoles{Beacon: "no_such_beacon_role", Collector: "no_such_collector_role"},
	}), "grants.live_settings")

	if got.Status != CheckSkip {
		t.Errorf("status = %s, want skip for roles that do not exist: %s", got.Status, got.Detail)
	}
}
