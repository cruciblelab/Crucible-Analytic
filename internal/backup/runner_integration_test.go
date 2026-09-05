//go:build integration

// The chain, not the links.
//
// Ask, Claim, Finish and ExpireStale each have a shape that is easy to
// test and easy to get right. internal/relupdate had all four, tested,
// beside a fetcher, an installer, a rollback and a panel button - and
// nothing called Claim. The button wrote a row no process read, and the
// page said "Sırada" forever.
//
// So this test starts where a person starts: a request written by the
// panel's role. It never calls Write directly.
//
// *Her halkası test edilmiş bir zincir, test edilmiş bir zincir
// değildir.*

package backup_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// backupQueue opens the queue as both roles and takes its lock.
//
// Two pools, because the separation is the design: the panel's role asks
// and cannot answer, the upgrader's answers and cannot ask. A test that
// used one connection for both halves would run against a permission
// arrangement this project does not ship - and would go on passing if
// the policies were removed entirely.
func backupQueue(t *testing.T) (asks, answers *pgxpool.Pool) {
	t.Helper()
	answers = testdb.Pool(t, testdb.SchemaAdmin)
	asks = testdb.Pool(t, testdb.Panel)
	testdb.Lock(t, answers, testdb.BackupQueueLock)

	// Cleared on the way in as well as on the way out.
	//
	// Every test in this file counts rows - "one backup produced one
	// catalogue row" - and a count is only an assertion if the starting
	// point is known. Clearing only afterwards makes each test depend on
	// the one before it having run and finished, which holds inside a
	// single `go test` and holds for nothing else: a run interrupted
	// half way, a row left by hand, a demo taken on the same database.
	//
	// Found that way rather than reasoned about. Two backups taken
	// outside the suite made the first test here report three rows after
	// one backup, and the message named the symptom rather than the
	// cause - which is what a test that arranges nothing can do.
	clear(t, asks, answers)
	t.Cleanup(func() { clear(t, asks, answers) })
	return asks, answers
}

// clear empties the queue and the catalogue, each by the role that is
// allowed to.
func clear(t *testing.T, asks, answers *pgxpool.Pool) {
	t.Helper()
	// Swept by the role the DELETE policy names, which is the panel's.
	if _, err := asks.Exec(context.Background(),
		`DELETE FROM panel_backup_requests`); err != nil {
		t.Fatalf("sweeping the backup queue: %v", err)
	}
	// And the catalogue, by the role that owns it.
	if _, err := answers.Exec(context.Background(),
		`DELETE FROM panel_backups`); err != nil {
		t.Fatalf("sweeping the catalogue: %v", err)
	}
}

// TestPressingTheButtonProducesAFileAndACatalogueRow.
func TestPressingTheButtonProducesAFileAndACatalogueRow(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()
	dir := t.TempDir()

	if _, err := backup.Ask(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
		[]string{backup.SetPanel}); err != nil {
		t.Fatal(err)
	}

	r := backup.Runner{Pool: answers, Dir: dir, Name: "test-upgrader",
		BinaryVersion: "v0.0.0-test", SchemaVersion: 99}
	req, err := r.RunOnce(ctx)
	if err != nil {
		t.Fatalf("the runner did not carry out the request: %v", err)
	}
	if req == nil {
		t.Fatal("the runner claimed nothing while a request was pending")
	}

	// The row says it worked.
	latest, err := backup.Latest(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != backup.StateSucceeded {
		t.Fatalf("the request is in state %q: %s", latest.State, latest.ErrorChain)
	}
	if latest.BackupID == nil {
		t.Fatal("the request succeeded and names no catalogue row, so the page has " +
			"nothing to show for it")
	}

	// The catalogue agrees with the disk.
	rows, err := backup.ListWithPaths(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the catalogue has %d rows after one backup", len(rows))
	}
	info, err := os.Stat(rows[0].Path)
	if err != nil {
		t.Fatalf("the catalogue names %s and it is not there: %v", rows[0].Path, err)
	}
	if info.Size() != rows[0].Bytes {
		t.Errorf("the catalogue says %d bytes and the file is %d", rows[0].Bytes, info.Size())
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the file is mode %v", perm)
	}
	// And the directory it sits in is not readable by anybody else.
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("the backup directory is mode %v; a file nobody may read inside a "+
			"directory anybody may list is a file whose name and size are public", perm)
	}
}

// TestThePanelCannotLearnWhereTheFileIs.
//
// The column-level grant, checked against the database rather than
// against the comment claiming it. This is the whole protection: nothing
// serves a backup over HTTP, and a process that does not know the path
// cannot be talked into reading it.
func TestThePanelCannotLearnWhereTheFileIs(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()

	if _, err := backup.Ask(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
		[]string{backup.SetPanel}); err != nil {
		t.Fatal(err)
	}
	r := backup.Runner{Pool: answers, Dir: t.TempDir(), Name: "test-upgrader"}
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// What the panel may read.
	rows, err := backup.List(ctx, asks)
	if err != nil {
		t.Fatalf("the panel could not list backups at all: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the panel sees no backups, so this test proves nothing about what it " +
			"cannot see")
	}
	for _, b := range rows {
		if b.Path != "" {
			t.Errorf("List handed the panel a path (%q)", b.Path)
		}
		if b.Bytes == 0 {
			t.Error("the panel cannot see the size either, which it needs")
		}
	}

	// And what it may not, refused by the database.
	if _, err := backup.ListWithPaths(ctx, asks); err == nil {
		t.Error("the panel's role read the path column. The grant on panel_backups " +
			"names columns for exactly this reason: a panel that knows where the " +
			"backups are is one bug away from being asked to read one")
	} else if !strings.Contains(strings.ToLower(err.Error()), "permission") &&
		!strings.Contains(strings.ToLower(err.Error()), "izin") {
		t.Errorf("the refusal is not a permission error, so something else stopped "+
			"it and the grant is untested: %v", err)
	}
}

// TestTheUpgraderCannotAskAndThePanelCannotAnswer.
//
// The split the queue exists for, checked in both directions. FORCE ROW
// LEVEL SECURITY is what makes the second half true: without it the
// table's owner is exempt from its own policies, and schema_admin owns
// this table.
func TestTheUpgraderCannotAskAndThePanelCannotAnswer(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()

	if _, err := backup.Ask(ctx, answers, backup.Actor{Kind: "user", Label: "test"}, "",
		[]string{backup.SetPanel}); err == nil {
		t.Error("the upgrader's role queued a backup request. It answers requests; a " +
			"process that could also write them could give itself work and then " +
			"report having done it")
	}

	if _, err := backup.Ask(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
		[]string{backup.SetPanel}); err != nil {
		t.Fatalf("the panel's role could not queue a request: %v", err)
	}
	if _, err := backup.Claim(ctx, asks, "panel-pretending"); err == nil {
		t.Error("the panel's role claimed a request. Claiming is how the upgrader says " +
			"it is doing the work, and a panel that could would be a panel that could " +
			"tell itself a backup was taken")
	}
}

// TestOnlyOneBackupIsInFlight.
//
// Two dumps of the same database at once double the disk cost of the
// operation whose entire risk is disk cost, and they race for the same
// temporary name.
func TestOnlyOneBackupIsInFlight(t *testing.T) {
	asks, _ := backupQueue(t)
	ctx := context.Background()
	a := backup.Actor{Kind: "user", Label: "test"}

	if _, err := backup.Ask(ctx, asks, a, "", []string{backup.SetPanel}); err != nil {
		t.Fatal(err)
	}
	_, err := backup.Ask(ctx, asks, a, "", []string{backup.SetPanel})
	if !errors.Is(err, backup.ErrAlreadyInFlight) {
		t.Errorf("a second request while one was pending returned %v", err)
	}
}

// TestADeploymentWithNoBackupDirectoryFailsTheRowRatherThanWaiting.
//
// The state every machine starts in. A request queued there must end up
// failed with a reason on the row: the page is where somebody is
// waiting, and "nothing is configured" is exactly the answer they need.
func TestADeploymentWithNoBackupDirectoryFailsTheRowRatherThanWaiting(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()

	if _, err := backup.Ask(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
		[]string{backup.SetPanel}); err != nil {
		t.Fatal(err)
	}
	r := backup.Runner{Pool: answers, Name: "test-upgrader"} // no Dir
	if _, err := r.RunOnce(ctx); err == nil {
		t.Fatal("a deployment with no backup directory reported a successful backup")
	}

	latest, err := backup.Latest(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != backup.StateFailed {
		t.Errorf("the request is in state %q rather than failed", latest.State)
	}
	if !strings.Contains(latest.ErrorChain, "upgrader.toml") {
		t.Errorf("the reason does not say where to configure it: %q", latest.ErrorChain)
	}
}

// TestABackupThatWouldNotFitIsRefusedBeforeAnythingIsWritten.
//
// The one outage this feature can cause is the one it causes by working:
// a full disk stops the collector, and the collector is in front of the
// customer's website.
func TestABackupThatWouldNotFitIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()

	// A directory on a filesystem far too small for the estimate.
	dir := tinyFilesystem(t)

	if _, err := backup.Ask(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
		[]string{backup.SetAnalitik, backup.SetPanel}); err != nil {
		t.Fatal(err)
	}
	r := backup.Runner{Pool: answers, Dir: dir, Name: "test-upgrader"}
	if _, err := r.RunOnce(ctx); err == nil {
		t.Fatal("a backup that could not fit was reported as taken")
	}

	latest, err := backup.Latest(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(latest.ErrorChain, "short by") {
		t.Errorf("the reason does not say by how much: %q", latest.ErrorChain)
	}
	// Nothing was written, not even a partial file.
	left, err := filepath.Glob(filepath.Join(dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Errorf("a refused backup left %v behind", left)
	}
}

// TestACatalogueRowWhoseFileIsGoneIsMarkedRatherThanDropped.
//
// An operator with a shell can delete a backup. A row that vanished with
// it would leave the page saying nothing, where "there was a backup here
// and it is gone" is what somebody needs to read.
func TestACatalogueRowWhoseFileIsGoneIsMarkedRatherThanDropped(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()
	dir := t.TempDir()

	if _, err := backup.Ask(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
		[]string{backup.SetPanel}); err != nil {
		t.Fatal(err)
	}
	r := backup.Runner{Pool: answers, Dir: dir, Name: "test-upgrader"}
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	rows, err := backup.ListWithPaths(ctx, answers)
	if err != nil || len(rows) == 0 {
		t.Fatalf("no catalogue row to work with: %v", err)
	}
	if err := os.Remove(rows[0].Path); err != nil {
		t.Fatal(err)
	}

	marked, err := r.Sweep(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if marked != 1 {
		t.Errorf("the sweep marked %d rows after one file was deleted", marked)
	}

	after, err := backup.List(ctx, asks)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("the row was dropped rather than marked; the page would say nothing "+
			"at all about a backup that used to exist (%d rows)", len(after))
	}
	if after[0].State != "missing" {
		t.Errorf("the row is in state %q", after[0].State)
	}
}

// tinyFilesystem is a mount too small for any real backup.
//
// A real filesystem rather than a fake number, because what is being
// tested is the arithmetic against a real statfs. Skips when it cannot
// mount: the check it guards is about a machine with no room, and a test
// that cannot make one has nothing to say.
func tinyFilesystem(t *testing.T) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("making a filesystem too small to hold a backup needs root")
	}
	dir, err := os.MkdirTemp("", "backup-tiny-*")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("mount", "-t", "tmpfs", "-o", "size=1m", "tmpfs", dir).
		CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Skipf("this machine does not allow mounting: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("umount", dir).CombinedOutput(); err != nil {
			t.Errorf("unmounting %s: %v\n%s", dir, err, out)
			return
		}
		_ = os.RemoveAll(dir)
	})
	return dir
}

// TestARequestThisBuildCannotCarryOutIsFinishedRatherThanLeftRunning.
//
// # The wedge this closes
//
// Claim checks the row's sets on the way out, because the row was
// written by a process this one does not trust to have been honest and
// by a build that may not be this one. That check was right and its
// only outcome was wrong: it returned an error *after* the UPDATE had
// already moved the row to `running`, and RunOnce returned without
// finishing it.
//
// So the row sat in `running` with nobody working on it. ExpireStale
// released it into the next run, which claimed it, failed the same
// check, and left it again - every thirty seconds, forever, while the
// page said "Alınıyor" and the queue's one in-flight slot stayed taken.
// No backup could be requested again on that deployment.
//
// Reachable exactly where Claim says it is: a row written by a panel of
// a different version, naming a set this build renamed. Which is not
// hypothetical - a set was renamed in the commit that found this.
func TestARequestThisBuildCannotCarryOutIsFinishedRatherThanLeftRunning(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()

	// Written with SQL, because backup.Ask refuses it - which is the
	// first of the two checks and not the one under test. This is the
	// row a panel of another version leaves behind.
	if _, err := asks.Exec(ctx, `
		INSERT INTO panel_backup_requests (actor_kind, actor_label, sets)
		VALUES ('user', 'test', $1)`, []string{"analitik-eski"}); err != nil {
		t.Fatalf("writing the row this test needs: %v", err)
	}

	r := backup.Runner{Pool: answers, Dir: t.TempDir(), Name: "test-upgrader",
		BinaryVersion: "v0.0.0-test", SchemaVersion: 99}
	if _, err := r.RunOnce(ctx); err == nil {
		t.Fatal("the runner carried out a request naming a set this build does not know")
	}

	latest, err := backup.Latest(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != backup.StateFailed {
		t.Fatalf("the request is in state %q. A row left running is a queue nobody can "+
			"use again: the in-flight index refuses every later request, and the page "+
			"goes on saying a backup is being taken", latest.State)
	}
	if !strings.Contains(latest.ErrorChain, "analitik-eski") {
		t.Errorf("the row says %q and does not name what it could not do",
			latest.ErrorChain)
	}
	// And it was refused at the door rather than part-way through.
	//
	// Only Claim's check produces this phrase. write checks the sets
	// again and would reach the same outcome, so without this the two
	// are indistinguishable - and a redundant defence no test can tell
	// apart from its backstop is one that gets deleted as dead. Found
	// by mutation: removing Claim's validation entirely was green.
	//
	// The difference is real. Claim refuses before the runner measures
	// the database and before it touches the disk, and the row an
	// operator reads names the request it could not carry out.
	if !strings.Contains(latest.ErrorChain, "claimed as request") {
		t.Errorf("the row says %q. That is the backstop in write speaking, not the "+
			"check Claim runs on the way out - so a row this build cannot obey now "+
			"gets as far as the estimate before anything refuses it", latest.ErrorChain)
	}

	// And the slot is free again: the whole cost of the wedge was that
	// it was not.
	if _, err := backup.Ask(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
		[]string{backup.SetPanel}); err != nil {
		t.Fatalf("a later request was refused after the failure: %v", err)
	}
}
