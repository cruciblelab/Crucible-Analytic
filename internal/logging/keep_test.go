package logging

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The keeper's tests run real passes over a real directory. A keeper
// whose decisions were tested against a fake Maintain could agree with
// itself about "a pass ran" while no file moved, and the defect W4a
// fixes was exactly a setting that said saved while the files stayed.

// keepFixture is a service tree with one ordinary log per given age.
type keepFixture struct {
	t    *testing.T
	root string
	now  time.Time
	c    *Controls
}

func newKeepFixture(t *testing.T, file Config) *keepFixture {
	t.Helper()
	root := t.TempDir()
	file.Dir = root
	_, c, closeLogs, err := Setup("w4a", file)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	t.Cleanup(closeLogs)
	return &keepFixture{t: t, root: root, now: time.Now(), c: c}
}

// plant writes one file of category under the day n days before now.
func (f *keepFixture) plant(n int, category Category) string {
	f.t.Helper()
	day := f.now.UTC().AddDate(0, 0, -n).Format("2006-01-02")
	dir := filepath.Join(f.root, "w4a", day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	path := filepath.Join(dir, string(category)+".log")
	if err := os.WriteFile(path, []byte("{\"msg\":\"eski\"}\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Setup touches no file. It used to make a pass with the config file's
// numbers before the service could know the panel's, and a retention the
// owner had lengthened in the panel lost its files on every restart -
// measured on the real beacon: 400 days in the panel, a 370-day-old
// security log gone at startup.
func TestSetup_DeletesNothingBeforeThePolicyIsKnown(t *testing.T) {
	root := t.TempDir()
	day := time.Now().UTC().AddDate(0, 0, -370).Format("2006-01-02")
	dir := filepath.Join(root, "w4a", day)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	security := filepath.Join(dir, "security.log")
	if err := os.WriteFile(security, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Past the file's important retention (365 by default) and past its
	// archive age: either kind of pass would change it.
	_, _, closeLogs, err := Setup("w4a", Config{Dir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer closeLogs()
	if !exists(security) {
		t.Error("Setup deleted a 370-day-old security log before anybody knew the policy in force")
	}
}

// The first Keep runs a pass, the same policy again within the hour does
// not, and a changed policy runs at once.
//
// The middle line is the shape of every refresh in a service nobody
// configured in the panel - once a minute, the same policy - and a keeper
// that walked the tree on each of them would be the cost MaintainEvery
// exists to avoid. The last is the defect: a policy the panel changed
// reaching the files on the next refresh rather than never.
func TestKeep_ARunningServiceAppliesAChangedPolicyAtOnce(t *testing.T) {
	f := newKeepFixture(t, Config{})
	old := f.plant(3, CategoryAccess)

	if _, ran, err := f.c.Keep(f.c.FileLifecycle(), f.now); err != nil || !ran {
		t.Fatalf("the first Keep ran no pass (ran=%v, err=%v)", ran, err)
	}
	if _, ran, err := f.c.Keep(f.c.FileLifecycle(), f.now.Add(time.Minute)); err != nil || ran {
		t.Fatalf("the same policy a minute later ran a pass again (ran=%v, err=%v)", ran, err)
	}
	if !exists(old) {
		t.Fatal("the default policy deleted a 3-day-old access log; the fixture is not testing a change")
	}

	panel := f.c.FileLifecycle()
	panel.RetentionDays = 2
	report, ran, err := f.c.Keep(panel, f.now.Add(2*time.Minute))
	if err != nil || !ran {
		t.Fatalf("a changed policy did not run a pass (ran=%v, err=%v)", ran, err)
	}
	if exists(old) {
		t.Errorf("retention 2 left a 3-day-old access log in place: %s", old)
	}
	if report.Deleted != 1 {
		t.Errorf("the pass reports %d deleted, want 1", report.Deleted)
	}
}

// Unknown archives and deletes nothing: the policy for a service that has
// not read its settings yet.
func TestKeep_AnUnknownPolicyArchivesAndDeletesNothing(t *testing.T) {
	f := newKeepFixture(t, Config{})
	ordinary := f.plant(30, CategoryAccess)
	important := f.plant(400, CategorySecurity)

	report, ran, err := f.c.Keep(Unknown(f.c.FileLifecycle()), f.now)
	if err != nil || !ran {
		t.Fatalf("ran=%v err=%v", ran, err)
	}
	if report.Deleted != 0 {
		t.Errorf("an unknown policy deleted %d files", report.Deleted)
	}
	if !exists(ordinary+".gz") || !exists(important+".gz") {
		t.Errorf("an unknown policy did not archive the old files (it keeps the file's archive age)")
	}
}

// An unchanged policy is applied again once MaintainEvery has gone by,
// and not before.
//
// This is the half that keeps a process that never restarts: before W4a
// a file that came of age after startup stayed until the next start.
// Both sides of the hour are asked, so a keeper that ran on every call
// and one that never ran again are each caught by a different line.
func TestKeep_AnUnchangedPolicyIsAppliedHourly(t *testing.T) {
	f := newKeepFixture(t, Config{})
	policy := f.c.FileLifecycle()
	start := f.now
	if _, ran, err := f.c.Keep(policy, start); err != nil || !ran {
		t.Fatalf("the first pass did not run (ran=%v, err=%v)", ran, err)
	}

	// A file that comes of age after the first pass.
	aged := f.plant(policy.RetentionDays, CategoryAccess)

	if _, ran, _ := f.c.Keep(policy, start.Add(MaintainEvery-time.Second)); ran {
		t.Error("an unchanged policy ran again before MaintainEvery had gone by")
	}
	if !exists(aged) {
		t.Fatal("the aged file went before any pass was due")
	}
	_, ran, err := f.c.Keep(policy, start.Add(MaintainEvery+time.Second))
	if err != nil || !ran {
		t.Fatalf("an unchanged policy was not applied again after MaintainEvery (ran=%v, err=%v)", ran, err)
	}
	if exists(aged) {
		t.Errorf("the hourly pass left a file past its retention: %s", aged)
	}
}

// The important categories keep their own, longer age when the ordinary
// one shrinks - the reason the policy has two figures.
func TestKeep_AShorterOrdinaryRetentionLeavesSecurityLogs(t *testing.T) {
	f := newKeepFixture(t, Config{})
	access := f.plant(3, CategoryAccess)
	security := f.plant(3, CategorySecurity)

	policy := f.c.FileLifecycle()
	policy.RetentionDays = 2
	if _, _, err := f.c.Keep(policy, f.now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if exists(access) {
		t.Error("the access log outlived its retention")
	}
	if !exists(security) {
		t.Error("a 3-day-old security log was deleted under an ordinary retention of 2 days")
	}
}

// The config file's numbers, negative ones included, are what
// FileLifecycle returns - the fallback LiveLifecycle uses for a key with
// no row. A keeper that answered the package defaults instead would undo
// an operator's "never delete" the first time the settings were read.
func TestKeep_FileLifecycleIsTheConfigFilesPolicy(t *testing.T) {
	file := Config{RetentionDays: 30, ImportantRetentionDays: -1, ArchiveAfterDays: 2}
	f := newKeepFixture(t, file)
	want := Lifecycle{ArchiveAfterDays: 2, RetentionDays: 30, ImportantRetentionDays: -1}
	if got := f.c.FileLifecycle(); got != want {
		t.Errorf("FileLifecycle = %+v, want %+v", got, want)
	}

	// And with no log directory: no tree to keep, but the file still
	// said what it said.
	_, c, closeLogs, err := Setup("w4a", Config{RetentionDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	defer closeLogs()
	if got := c.FileLifecycle().RetentionDays; got != 30 {
		t.Errorf("without a directory FileLifecycle().RetentionDays = %d, want 30", got)
	}
	if _, ran, err := c.Keep(Lifecycle{RetentionDays: 1}, time.Now()); ran || err != nil {
		t.Errorf("a service with no log directory ran a pass (ran=%v, err=%v)", ran, err)
	}
}

// A pass that fails is not retried on every refresh.
//
// The failure is a file the process cannot remove; the next refresh comes
// a minute later and would fail the same way, logging the same line
// sixty times an hour. It is tried again when the hour is up.
func TestKeep_AFailedPassWaitsForTheHour(t *testing.T) {
	f := newKeepFixture(t, Config{})
	// A file old enough to archive whose temporary output path is a
	// directory: compressing it fails for any user, root included, which
	// a permission-based fixture would not.
	log := f.plant(8, CategoryAccess)
	if err := os.Mkdir(log+".gz.tmp", 0o755); err != nil {
		t.Fatal(err)
	}

	policy := f.c.FileLifecycle()
	policy.RetentionDays = 13
	if _, ran, err := f.c.Keep(policy, f.now.Add(time.Minute)); !ran || err == nil {
		t.Fatalf("the fixture did not make the pass fail (ran=%v, err=%v)", ran, err)
	}
	if _, ran, _ := f.c.Keep(policy, f.now.Add(2*time.Minute)); ran {
		t.Error("a failed pass was retried on the next refresh, a minute later")
	}
	if _, ran, _ := f.c.Keep(policy, f.now.Add(time.Minute+MaintainEvery)); !ran {
		t.Error("a failed pass was not tried again once the hour was up")
	}
}

// KeepFile makes its first pass at once, not an hour in: for the API and
// the upgrader it is the pass Setup used to make at startup, and a
// process that restarts more often than hourly would otherwise never
// make one. Asked with a context that has already ended, so the only
// pass that can happen is the first.
func TestKeepFile_PassesAtOnce(t *testing.T) {
	f := newKeepFixture(t, Config{})
	old := f.plant(20, CategoryAccess)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	KeepFile(ctx, f.c, slog.New(slog.DiscardHandler))
	if exists(old) || exists(old+".gz") {
		t.Error("KeepFile returned without a first pass: a 20-day-old access log is still there " +
			"under the file's 14-day retention")
	}
}
