//go:build integration && linux

// Against a filesystem whose size this test chose.
//
// Every other test here measures whatever filesystem the temp directory
// happens to be on, and compares against df. That catches a wrong unit
// and a wrong field, and it cannot catch the two answers that only exist
// on a real mount: whether Mount is true for one, and whether a second
// filesystem gets its own device number rather than the parent's.
//
// So this one makes a filesystem. A tmpfs of a size written here, which
// means the expected total is not read from the same syscall being
// tested - it is a number this test picked and the kernel was told.

package diskspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// mountTmpfs mounts a tmpfs of sizeMB at a fresh directory and unmounts
// it afterwards.
func mountTmpfs(t *testing.T, sizeMB int) string {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("mounting needs root; the other tests in this package do not")
	}
	if _, err := exec.LookPath("mount"); err != nil {
		t.Skip("mount is not on PATH")
	}

	// Under the parent of a temp dir rather than inside it: t.TempDir's
	// own cleanup walks the tree, and walking into a live mount is a way
	// to delete something that is not the test's.
	dir, err := os.MkdirTemp("", "diskspace-mount-*")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("mount", "-t", "tmpfs",
		"-o", "size="+itoa(sizeMB)+"m", "tmpfs", dir).CombinedOutput()
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Skipf("this machine does not allow mounting: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		if out, err := exec.Command("umount", dir).CombinedOutput(); err != nil {
			// Reported rather than ignored. A mount this test leaves
			// behind outlives the test binary and the next run of the
			// suite inherits it.
			t.Errorf("unmounting %s: %v\n%s", dir, err, out)
			return
		}
		_ = os.RemoveAll(dir)
	})
	return dir
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestAMountedFilesystemIsMeasuredAsItself.
func TestAMountedFilesystemIsMeasuredAsItself(t *testing.T) {
	const sizeMB = 16
	dir := mountTmpfs(t, sizeMB)

	s, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}

	// The size this test asked for, not a number read back out of the
	// thing under test.
	want := int64(sizeMB) * 1024 * 1024
	if s.TotalBytes != want {
		t.Errorf("a %d MB tmpfs was measured as %d bytes, expected %d",
			sizeMB, s.TotalBytes, want)
	}
	if !s.Mount {
		t.Error("a real mount point was reported as not being one. In a container this " +
			"is the difference between a directory that survives an image update and " +
			"one that is discarded by it")
	}
	// tmpfs keeps no root reserve, so everything is available.
	if s.AvailBytes != want {
		t.Errorf("a fresh %d MB tmpfs reports %d bytes available", sizeMB, s.AvailBytes)
	}
	if s.UsedBytes != 0 {
		t.Errorf("a fresh tmpfs reports %d bytes used", s.UsedBytes)
	}
}

// TestASecondFilesystemGetsItsOwnDevice.
//
// The page groups rows by device. Two filesystems sharing a number would
// be shown as one, with one of the two sets of numbers - and the disk
// that is full is as likely to be the hidden one as not.
func TestASecondFilesystemGetsItsOwnDevice(t *testing.T) {
	mounted := mountTmpfs(t, 8)

	inside, err := Read(mounted)
	if err != nil {
		t.Fatal(err)
	}
	outside, err := Read(filepath.Dir(mounted))
	if err != nil {
		t.Fatal(err)
	}
	if inside.Device == outside.Device {
		t.Errorf("a tmpfs and the directory it is mounted under both report device %d",
			inside.Device)
	}
}

// TestWritingFillsTheFilesystemThePageIsAbout.
//
// The measurement has to move when the disk does. A cached, stale or
// constant answer passes every test above: they all read a filesystem
// nobody is changing.
//
// This is also the arithmetic a backup depends on - "will four more
// megabytes fit" - so it is worth seeing it work once against real bytes
// rather than trusting that a syscall does what it says.
func TestWritingFillsTheFilesystemThePageIsAbout(t *testing.T) {
	dir := mountTmpfs(t, 16)

	before, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}

	const written = 4 << 20
	if err := os.WriteFile(filepath.Join(dir, "dolgu"), make([]byte, written), 0o600); err != nil {
		t.Fatal(err)
	}

	after, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := after.UsedBytes - before.UsedBytes; got < written {
		t.Errorf("%d bytes were written and used went up by %d", written, got)
	}
	if got := before.AvailBytes - after.AvailBytes; got < written {
		t.Errorf("%d bytes were written and available went down by %d", written, got)
	}
	if after.TotalBytes != before.TotalBytes {
		t.Errorf("the filesystem changed size from %d to %d while being written to",
			before.TotalBytes, after.TotalBytes)
	}
}
