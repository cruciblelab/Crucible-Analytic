//go:build integration

// The disk filling up while a backup is being written.
//
// # Why this needs a real filesystem
//
// Because the thing being tested is arithmetic against statfs, and a
// fake filesystem would let the arithmetic be wrong in the same
// direction as the fake. The estimate this guard exists to back up was
// itself justified by a measurement that did not support it - see
// estimate.go - and the way that happened was a number nobody made a
// filesystem to check.
//
// So: a real tmpfs, really filled, with a real backup writing into it.
//
// *Bir tahmin, tahmin olduğu için garanti olamaz.*

package backup

import (
	"archive/tar"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// smallFilesystem mounts a tmpfs of the given size and returns its path.
func smallFilesystem(t *testing.T, size string) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("filling a filesystem on purpose needs root")
	}
	dir, err := os.MkdirTemp("", "backup-guard-*")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("mount", "-t", "tmpfs", "-o", "size="+size, "tmpfs", dir).
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

// ballast takes up n bytes on the filesystem holding dir.
func ballast(t *testing.T, dir string, n int64) {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dolgu"), buf, 0o600); err != nil {
		t.Fatalf("filling %s to leave a known amount free: %v", dir, err)
	}
}

// incompressible is a fill function writing n bytes that gzip cannot
// shrink, so the bytes asked for are the bytes that land.
func incompressible(t *testing.T, n int) func(*tar.Writer) error {
	t.Helper()
	body := make([]byte, n)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	return func(tw *tar.Writer) error {
		if err := tw.WriteHeader(&tar.Header{
			Name: "veri", Mode: 0o600, Size: int64(len(body)),
		}); err != nil {
			return err
		}
		_, err := tw.Write(body)
		return err
	}
}

// freeBytes is what the filesystem holding dir has left.
func freeBytes(t *testing.T, dir string) int64 {
	t.Helper()
	var out int64
	cmd := exec.Command("df", "--output=avail", "-B1", dir)
	b, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	// df prints a header line and then the number, both indented.
	fields := strings.Fields(string(b))
	if len(fields) != 2 {
		t.Fatalf("df printed %q, which is not a header and one number", b)
	}
	if _, err := fmt.Sscan(fields[1], &out); err != nil {
		t.Fatalf("reading df output %q: %v", b, err)
	}
	return out
}

// TestAWriteThatWouldFillTheDiskStopsAndGivesTheBytesBack.
//
// The shape is the one that matters: the write *starts* with room to
// spare, the way every write that fills a disk does, and stops at the
// moment the margin is gone rather than at the moment the filesystem
// says ENOSPC. By then the collector has already missed writes.
func TestAWriteThatWouldFillTheDiskStopsAndGivesTheBytesBack(t *testing.T) {
	// 16 MB, so the margin is a tenth of it - 1.6 MB - and the guard
	// looks every quarter of that.
	dir := smallFilesystem(t, "16m")

	// 12 MB used leaves about 4 MB, comfortably above the margin. The
	// write below asks for 3 MB, which takes it under.
	ballast(t, dir, 12<<20)
	before := freeBytes(t, dir)
	if before < 2<<20 {
		t.Fatalf("the ballast left %d bytes free, which is already under the "+
			"margin. This test would then prove the guard fires on the first "+
			"check rather than part way through a write that had room", before)
	}

	_, err := container(dir, "yedek.tar.gz", incompressible(t, 3<<20))
	if !errors.Is(err, ErrDiskFilling) {
		t.Fatalf("writing 3 MB into %d free bytes returned %v.\n"+
			"Without the guard this either fills the filesystem or fails with "+
			"ENOSPC, and in both cases the database on the same disk is the "+
			"thing that stops", before, err)
	}

	// And the disk is where it was. A guard that stops the write but
	// leaves the partial file behind has moved the outage rather than
	// prevented it: the next attempt starts from less room than this one.
	after := freeBytes(t, dir)
	if after != before {
		t.Errorf("%d bytes were free before the stopped write and %d after. The "+
			"partial file was not given back, so retrying walks the disk down",
			before, after)
	}

	// Nothing under the final name, and nothing under the temporary one.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "dolgu" {
			t.Errorf("%s is left in the backup directory after a stopped write", e.Name())
		}
	}
}

// TestTheGuardDoesNotStopAWriteThatFits.
//
// Without this the test above proves nothing: "writing to a small tmpfs
// fails" and "the guard fired" look the same from the outside. Same
// filesystem, same writer, same bytes - only the room differs.
func TestTheGuardDoesNotStopAWriteThatFits(t *testing.T) {
	dir := smallFilesystem(t, "16m")
	ballast(t, dir, 4<<20)

	res, err := container(dir, "yedek.tar.gz", incompressible(t, 3<<20))
	if err != nil {
		t.Fatalf("a 3 MB backup into a filesystem with about 12 MB free was "+
			"stopped: %v", err)
	}
	info, err := os.Stat(res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != res.Bytes {
		t.Errorf("the result says %d bytes and the file is %d", res.Bytes, info.Size())
	}
}

// TestTheSecretsEstimateMeasuresTheDiskTheFileGoesTo.
//
// # The defect this pins
//
// MeasureSecrets read parentOf(dir) unconditionally, while Measure read
// dir and only fell back to the parent when dir did not exist. Its own
// comment claimed "the same shape as Measure and for the same reason".
//
// The two are the same directory on an ordinary install, so nothing
// showed. They are different filesystems exactly when the backup
// directory is its own mount - which KURULUM.md requires under Docker,
// because a backup written outside the volume is destroyed by the next
// image update. Measured on a 32 MB mount under a 252 GB root:
//
//	dir     32 MB total,  margin 3.3 MB
//	parent 252 GB total,  margin 1.0 GB
//
// So on precisely the deployment the documentation asks for, the secrets
// estimate was answering "does this fit" about the wrong disk, and
// reporting that disk's free space to the page.
func TestTheSecretsEstimateMeasuresTheDiskTheFileGoesTo(t *testing.T) {
	root, err := os.MkdirTemp("", "backup-mount-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })

	// The backup directory is a mount of its own, under a much larger
	// filesystem. That is the arrangement the docs require.
	dir := filepath.Join(root, "yedek")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() != 0 {
		t.Skip("making the backup directory its own mount needs root")
	}
	out, err := exec.Command("mount", "-t", "tmpfs", "-o", "size=32m", "tmpfs", dir).
		CombinedOutput()
	if err != nil {
		t.Skipf("this machine does not allow mounting: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("umount", dir).CombinedOutput(); err != nil {
			t.Errorf("unmounting %s: %v\n%s", dir, err, out)
		}
	})

	onMount := freeBytes(t, dir)
	onParent := freeBytes(t, root)
	if onMount == onParent {
		t.Fatalf("both report %d bytes free, so the mount did not take and this "+
			"test cannot tell the two readings apart", onMount)
	}

	est, err := MeasureSecrets(dir, []SecretFile{{Name: "a.toml", Bytes: []byte("x")}})
	if err != nil {
		t.Fatal(err)
	}
	if est.AvailBytes != onMount {
		t.Errorf("the secrets estimate reports %d bytes free; the mount the file "+
			"is written to has %d and the directory above it has %d.\n"+
			"The number on the page, and the decision to refuse, are about a "+
			"filesystem the backup never touches",
			est.AvailBytes, onMount, onParent)
	}
	if want := MarginFor(32 << 20); est.Margin != want {
		t.Errorf("the estimate keeps %d bytes spare; a 32 MB filesystem's margin "+
			"is %d. A margin taken from the parent is a gigabyte, which on this "+
			"mount is thirty times the disk", est.Margin, want)
	}
}

// TestTheGuardLooksOftenEnoughForTheFilesystemItIsOn.
//
// The interval is a ceiling of 8 MB and a quarter of the margin,
// whichever is smaller, and the second half is the one that matters:
// with only the ceiling, a 64 MB volume - which is a plausible size for
// a container's backup mount - has a 6.4 MB margin and would be checked
// every 8 MB, so a single unlucky gap could step straight over it.
func TestTheGuardLooksOftenEnoughForTheFilesystemItIsOn(t *testing.T) {
	dir := smallFilesystem(t, "64m")
	g := newSpaceGuard(io.Discard, dir)

	if g.margin <= 0 {
		t.Fatalf("the guard measured no margin on %s, so it is guarding nothing", dir)
	}
	if g.every > g.margin/4 {
		t.Errorf("the margin is %d bytes and the guard looks every %d. One gap "+
			"is then larger than a quarter of the thing being defended, and "+
			"whether that overshoots is left to how big the filesystem "+
			"happens to be", g.margin, g.every)
	}
	if g.every > GuardEvery {
		t.Errorf("the guard looks every %d bytes, above the %d ceiling",
			g.every, GuardEvery)
	}
}
