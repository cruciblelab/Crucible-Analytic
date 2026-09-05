//go:build integration && linux

// Which filesystem a backup landed on, and how the panel finds out.
//
// The panel draws the disk. It groups the directories it was configured
// with by device, and to put the backups into that picture it has to
// know which disk they are on - which it structurally cannot work out
// for itself: the directory is named in upgrader.toml, which the panel
// does not read, and `path` is not granted to its role.
//
// So the component that can see the filesystem records what it saw, and
// the panel reads an opaque number. This is the test that the number is
// the right one, taken against a real backup on a real filesystem rather
// than against the field being copied from A to B.

package backup_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
	"github.com/cruciblelab/crucible-analytic/internal/diskspace"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// TestTheCatalogueSaysWhichFilesystemTheFileIsOn.
//
// # What goes wrong without it
//
// The device is recorded once, on a path, by a call whose failure is
// deliberately ignored - a backup that could not be stat'ed is still a
// backup. That makes zero a value this column reaches quietly, and a
// column that is quietly always zero draws no segment, on a bar nobody
// is looking at closely enough to notice something is missing.
//
// So the number is compared against the kernel's answer for the same
// file, read independently.
func TestTheCatalogueSaysWhichFilesystemTheFileIsOn(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()
	dir := t.TempDir()

	if _, err := backup.Ask(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
		[]string{backup.SetPanel}); err != nil {
		t.Fatal(err)
	}
	r := backup.Runner{Pool: answers, Dir: dir, Name: "test-upgrader",
		BinaryVersion: "v0.0.0-test", SchemaVersion: 99}
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}

	rows, err := backup.ListWithPaths(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the catalogue has %d rows after one backup", len(rows))
	}

	// The kernel's own answer for that exact file, read here rather than
	// taken from the writer. A test that asked the same code twice would
	// pass against a column that recorded the wrong path's device.
	space, err := diskspace.Read(rows[0].Path)
	if err != nil {
		t.Fatalf("measuring %s: %v", rows[0].Path, err)
	}
	if rows[0].Device == 0 {
		t.Fatal("the catalogue recorded no filesystem for a file that is on one, so " +
			"the panel will show these bytes as belonging nowhere")
	}
	if rows[0].Device != int64(space.Device) {
		t.Errorf("the catalogue says device %d and the file is on %d.\n"+
			"The panel puts the bytes on whichever bar matches, so a wrong number "+
			"draws them onto somebody else's disk",
			rows[0].Device, int64(space.Device))
	}
}

// TestTheSumByFilesystemLeavesOutFilesThatAreGone.
//
// The list shows a missing backup on purpose: "there was one here on
// Tuesday and it is gone" is a sentence somebody needs. A bar must not,
// because a bar is a claim about what the disk is holding right now.
//
// Two readers, two rules, and this is the one that would drift silently
// - the numbers on the page would simply be a little large, in the
// direction that says the disk is fuller than it is.
func TestTheSumByFilesystemLeavesOutFilesThatAreGone(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()
	dir := t.TempDir()

	take := func() {
		t.Helper()
		if _, err := backup.Ask(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
			[]string{backup.SetPanel}); err != nil {
			t.Fatal(err)
		}
		r := backup.Runner{Pool: answers, Dir: dir, Name: "test-upgrader",
			BinaryVersion: "v0.0.0-test", SchemaVersion: 99}
		if _, err := r.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	take()
	take()

	rows, err := backup.ListWithPaths(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("the catalogue has %d rows after two backups", len(rows))
	}

	space, err := diskspace.Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	device := int64(space.Device)

	both, unplaced, err := backup.BytesByDevice(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if unplaced != 0 {
		t.Errorf("%d bytes were reported as belonging to no filesystem", unplaced)
	}
	if want := rows[0].Bytes + rows[1].Bytes; both[device] != want {
		t.Fatalf("the sum for this filesystem is %d and the two files are %d",
			both[device], want)
	}

	// Now one of them goes, the way an operator with a shell removes one.
	if err := os.Remove(rows[0].Path); err != nil {
		t.Fatal(err)
	}
	sweeper := backup.Runner{Pool: answers, Dir: dir, Name: "test-upgrader"}
	if marked, err := sweeper.Sweep(ctx); err != nil {
		t.Fatal(err)
	} else if marked != 1 {
		t.Fatalf("the sweep marked %d rows after one file was deleted", marked)
	}

	after, _, err := backup.BytesByDevice(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if after[device] != rows[1].Bytes {
		t.Errorf("after deleting one file the sum is %d and the remaining file is %d.\n"+
			"A bar drawn from this says the disk is holding bytes it is not",
			after[device], rows[1].Bytes)
	}
}

// TestARowWithNoFilesystemIsCountedButNotPlaced.
//
// Zero is what a backup taken before this column existed carries, and
// what one whose filesystem could not be read carries. Dropping those
// bytes would make the disk section disagree with the backup section's
// own total on the same page, for a reason no reader could see.
func TestARowWithNoFilesystemIsCountedButNotPlaced(t *testing.T) {
	_, answers := backupQueue(t)
	ctx := context.Background()

	// Written directly: there is no way to make the runner produce one,
	// which is the point - these rows come from a database that predates
	// the column.
	if _, err := answers.Exec(ctx, `
		INSERT INTO panel_backups (sets, bytes, sha256, path, device)
		VALUES ($1, $2, '', '/nerede-oldugu-kayitli-degil', 0)`,
		[]string{backup.SetPanel}, int64(4096)); err != nil {
		t.Fatal(err)
	}

	placed, unplaced, err := backup.BytesByDevice(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if unplaced != 4096 {
		t.Errorf("unplaced bytes came back as %d, want 4096", unplaced)
	}
	if _, ok := placed[0]; ok {
		t.Error("device zero was returned as a filesystem. The panel groups its bars " +
			"by device, and nothing it measures has device zero, so these bytes " +
			"would be attached to no bar and silently lost")
	}
}

// TestThePanelCanReadTheDeviceAndStillNotThePath.
//
// The whole design rests on this pair. The device is shared because it
// is opaque; the path is not, because it is not. A grant that widened
// while nobody was looking would be invisible in every other test here,
// because every other test connects as the upgrader.
func TestThePanelCanReadTheDeviceAndStillNotThePath(t *testing.T) {
	ctx := context.Background()
	panelPool := testdb.Pool(t, testdb.Panel)

	var device int64
	err := panelPool.QueryRow(ctx,
		`SELECT coalesce(max(device), 0) FROM panel_backups`).Scan(&device)
	if err != nil {
		t.Fatalf("the panel cannot read the device column: %v", err)
	}

	var path string
	err = panelPool.QueryRow(ctx, `SELECT path FROM panel_backups LIMIT 1`).Scan(&path)
	if err == nil {
		t.Fatal("the panel read a backup's path. The column-level grant is the boundary " +
			"and it is open")
	}
}

// TestTwoBackupsInOneSecondDoNotBecomeOne.
//
// # The defect, and how it was found
//
// The name was `yedek-<date>-<hhmmss>.tar.gz`. Two backups taken in the
// same second got the same name, and the rename that gives a finished
// file its real name replaces an existing one silently and atomically -
// which is exactly the property that makes rename right everywhere else
// here.
//
// The first file was destroyed. Both catalogue rows survived, with two
// dates, two sizes and two checksums, pointing at one file. The customer
// sees two backups and has one, and the one they would reach for first
// is the one that is gone.
//
// Found by a test written for something else: the sum-by-filesystem test
// took two backups in a row and the sweep marked two rows missing after
// one file was deleted. One second is not a hypothetical window - it is
// how long two RunOnce calls take on a small deployment.
//
// Fixed twice over, because the two fixes answer different questions.
// The name carries milliseconds, so a collision needs a clock that steps
// backwards; and Write refuses to land on a name that exists, so a
// collision that does happen fails loudly instead of destroying a file.
func TestTwoBackupsInOneSecondDoNotBecomeOne(t *testing.T) {
	asks, answers := backupQueue(t)
	ctx := context.Background()
	dir := t.TempDir()

	for i := 0; i < 2; i++ {
		if _, err := backup.Ask(ctx, asks, backup.Actor{Kind: "user", Label: "test"}, "",
			[]string{backup.SetPanel}); err != nil {
			t.Fatal(err)
		}
		r := backup.Runner{Pool: answers, Dir: dir, Name: "test-upgrader",
			BinaryVersion: "v0.0.0-test", SchemaVersion: 99}
		if _, err := r.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := backup.ListWithPaths(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("the catalogue has %d rows after two backups", len(rows))
	}
	if rows[0].Path == rows[1].Path {
		t.Fatalf("both catalogue rows name %s. One of the two files no longer exists "+
			"and the page shows both", rows[0].Path)
	}
	for _, b := range rows {
		if _, err := os.Stat(b.Path); err != nil {
			t.Errorf("the catalogue names %s and it is not there: %v", b.Path, err)
		}
	}
}

// TestWritingOntoAnExistingBackupIsRefused.
//
// The guard on its own, reached the only way it can be: by putting the
// file there first. The name is unique enough that nothing else produces
// this, which is precisely why the refusal needs its own test - a guard
// no test enters is a guard somebody deletes as dead weight.
func TestWritingOntoAnExistingBackupIsRefused(t *testing.T) {
	_, answers := backupQueue(t)
	ctx := context.Background()
	dir := t.TempDir()

	const name = "yedek-hedef.tar.gz"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("onceki yedek"), 0o600); err != nil {
		t.Fatal(err)
	}

	w := backup.Writer{Pool: answers, Dir: dir,
		BinaryVersion: "v0.0.0-test", SchemaVersion: 99}
	if _, err := w.Write(ctx, name, []string{backup.SetPanel}); err == nil {
		t.Fatal("writing over an existing backup succeeded")
	}

	// And the file that was there is untouched.
	body, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "onceki yedek" {
		t.Errorf("the existing file was changed: %q", body)
	}
}

// TestTakeRefusesWhenNoDirectoryIsConfigured.
//
// # Why this needs its own test
//
// carryOut has the same guard and a test for it, and for a moment that
// looked like enough. It is not: the two paths reach the same condition
// and want opposite things from it. A queued request must end up failed
// with the reason on the row, because somebody is watching the page.
// Take is called by the schema upgrade, which reads the sentinel and
// carries on - a deployment that never configured backups is not one
// that just lost one.
//
// A mutation removing Take's guard survived the queue's test, which is
// exactly what a shared condition with two callers does when only one of
// them is asserted.
func TestTakeRefusesWhenNoDirectoryIsConfigured(t *testing.T) {
	_, answers := backupQueue(t)

	// No Dir, which is what a deployment with no [backup] section has.
	r := backup.Runner{Pool: answers, Name: "test-upgrader"}
	id, err := r.Take(context.Background(), []string{backup.SetPanel})

	if !errors.Is(err, backup.ErrNotConfigured) {
		t.Fatalf("Take returned %v, want ErrNotConfigured.\n"+
			"The schema upgrade tells this apart from every other failure to "+
			"decide whether to go ahead, and it can only do that through the "+
			"sentinel", err)
	}
	if id != nil {
		t.Errorf("Take named catalogue row %d while refusing", *id)
	}
}

// TestTakeWritesTheSameKindOfBackupAsTheButton.
//
// The automatic copy and the one somebody presses for have to end up in
// the same catalogue, with the same shape. A backup taken by the machine
// that a person could not find beside their own would be a backup they
// do not know they have - and the page is the only place they can look.
func TestTakeWritesTheSameKindOfBackupAsTheButton(t *testing.T) {
	_, answers := backupQueue(t)
	ctx := context.Background()
	dir := t.TempDir()

	r := backup.Runner{Pool: answers, Dir: dir, Name: "test-upgrader",
		BinaryVersion: "v0.0.0-test", SchemaVersion: 99}
	id, err := r.Take(ctx, []string{backup.SetPanel})
	if err != nil {
		t.Fatal(err)
	}
	if id == nil {
		t.Fatal("Take succeeded and named no catalogue row, so nothing on the page " +
			"will ever mention this backup")
	}

	rows, err := backup.ListWithPaths(ctx, answers)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("the catalogue has %d rows after one automatic backup", len(rows))
	}
	if rows[0].ID != *id {
		t.Errorf("Take named row %d and the catalogue has %d", *id, rows[0].ID)
	}
	if info, err := os.Stat(rows[0].Path); err != nil {
		t.Errorf("the catalogue names %s and it is not there: %v", rows[0].Path, err)
	} else if info.Mode().Perm() != 0o600 {
		t.Errorf("the file is mode %v; an automatic backup is not less private "+
			"than one somebody asked for", info.Mode().Perm())
	}
	if rows[0].Device == 0 {
		t.Error("no filesystem was recorded, so the panel cannot draw these bytes " +
			"onto the disk they are on")
	}
}

// TestWritableSaysNoWhenTheFilesystemIsReadOnly.
//
// # The condition this is about
//
// ProtectSystem=strict remounts the filesystem read-only inside the
// unit's mount namespace. The directory keeps its mode and its owner, so
// every check that reads metadata says "writable" and the write fails
// anyway. That is what made every backup on every systemd install fail,
// with a message the customer could not act on.
//
// Reproduced rather than described: the test creates its own mount
// namespace and remounts the directory read-only, which is the same
// mechanism systemd uses.
func TestWritableSaysNoWhenTheFilesystemIsReadOnly(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to create a mount namespace; this is what CI's " +
			"container gives and a developer's laptop does not")
	}
	dir := t.TempDir()

	// The directory itself is fine - this is the whole point.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r := backup.Runner{Dir: dir}
	if err := r.Writable(); err != nil {
		t.Fatalf("a writable directory was reported as not: %v", err)
	}

	// Now the same directory, on a read-only mount, in a namespace of
	// this test's own so nothing else on the machine sees it.
	script := `set -e
mount --bind "$1" "$1"
mount -o remount,ro,bind "$1"
touch "$1/deneme" 2>/dev/null && echo WRITABLE || echo READONLY`
	out, err := exec.Command("unshare", "-m", "sh", "-c", script, "sh", dir).CombinedOutput()
	if err != nil {
		t.Skipf("could not build the read-only mount here: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "READONLY") {
		t.Fatalf("the mount namespace did not make the directory read-only, so this "+
			"test would prove nothing:\n%s", out)
	}

	// And Writable, run inside that namespace, has to agree. The test
	// binary re-execs itself as the child; see TestWritableProbeHelper.
	probe := `set -e
mount --bind "$1" "$1"
mount -o remount,ro,bind "$1"
exec "$2" -test.run=TestWritableProbeHelper`
	child := exec.Command("unshare", "-m", "sh", "-c", probe, "sh", dir, os.Args[0])
	child.Env = append(os.Environ(), writableProbeEnv+"="+dir)
	out, err = child.CombinedOutput()
	if err == nil {
		t.Fatalf("Writable said yes on a read-only filesystem.\n"+
			"The directory's mode and owner are untouched by the remount, so "+
			"anything short of writing says the same - which is how this shipped:\n%s",
			out)
	}
	if !strings.Contains(string(out), "read-only") {
		t.Errorf("the refusal does not mention the filesystem, so an operator "+
			"reading the journal would not know what to fix:\n%s", out)
	}
}

// TestWritableProbeHelper is the child half of the test above.
//
// # Why a child process at all
//
// The check has to run *inside* a mount namespace, and a Go test cannot
// enter one without taking the rest of its own process with it. The
// standard answer is to re-exec the test binary with a marker in the
// environment, which is what this is: skipped in an ordinary run, and
// the whole program when the parent sets the variable.
func TestWritableProbeHelper(t *testing.T) {
	dir := os.Getenv(writableProbeEnv)
	if dir == "" {
		t.Skip("the child half of TestWritableSaysNoWhenTheFilesystemIsReadOnly")
	}
	if err := (backup.Runner{Dir: dir}).Writable(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

const writableProbeEnv = "CA_WRITABLE_PROBE_DIR"
