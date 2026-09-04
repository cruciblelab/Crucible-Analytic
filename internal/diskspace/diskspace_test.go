//go:build linux

package diskspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The numbers are checked against df, not against themselves.
//
// A test that asserted TotalBytes > 0 and AvailBytes <= TotalBytes would
// pass against a block size read from the wrong field, against f_bfree
// where f_bavail was meant, and against a unit that was off by a factor
// of eight. Every one of those is a plausible mistake here and none of
// them is visible from inside.
//
// df is the second implementation. It is on every machine this runs on,
// it reads the same syscall, and it is what an operator will compare the
// page against when they doubt it - so it is the right thing to be wrong
// against.

// dfKB runs df for one path and returns 1K-blocks, used and available.
func dfKB(t *testing.T, path string) (total, used, avail int64) {
	t.Helper()
	out, err := exec.Command("df", "-P", "-k", path).Output()
	if err != nil {
		t.Skipf("df is not usable here: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		t.Fatalf("df printed no data row:\n%s", out)
	}
	// The device name can contain spaces, so the numbers are taken from
	// the right rather than by column index from the left.
	f := strings.Fields(lines[len(lines)-1])
	if len(f) < 5 {
		t.Fatalf("df printed %d fields, not a POSIX row:\n%s", len(f), out)
	}
	n := len(f)
	parse := func(s string) int64 {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			t.Fatalf("df field %q is not a number: %v", s, err)
		}
		return v
	}
	// -P fixes the order: Filesystem 1024-blocks Used Available Capacity Mounted-on
	return parse(f[n-5]), parse(f[n-4]), parse(f[n-3])
}

// TestTheNumbersAgreeWithDf.
func TestTheNumbersAgreeWithDf(t *testing.T) {
	dir := t.TempDir()
	got, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	total, used, avail := dfKB(t, dir)

	// Compared in kibibytes, because that is df's unit and rounding it
	// the other way would invent a disagreement.
	const kb = 1024
	for _, c := range []struct {
		what      string
		mine, dfs int64
	}{
		{"total", got.TotalBytes / kb, total},
		{"used", got.UsedBytes / kb, used},
		{"available", got.AvailBytes / kb, avail},
	} {
		// A filesystem in use moves between the two calls, so exact
		// equality would be flaky on a busy machine. One part in a
		// thousand is far tighter than any of the mistakes this is
		// looking for: a wrong block size is off by 8x or 4096x, and
		// f_bfree instead of f_bavail is off by the whole root reserve.
		diff := c.mine - c.dfs
		if diff < 0 {
			diff = -diff
		}
		tolerance := c.dfs / 1000
		if tolerance < 64 {
			tolerance = 64
		}
		if diff > tolerance {
			t.Errorf("%s: this package says %d KiB, df says %d KiB (off by %d)",
				c.what, c.mine, c.dfs, diff)
		}
	}
}

// TestAvailableIsWhatANonRootProcessMayHave.
//
// The one field where the plausible mistake and the correct answer are
// both non-zero, both nearly right, and differ by exactly the amount
// that matters: ext4 reserves about 5% for root, and every process in
// this product runs as an ordinary account.
//
// df's "Available" column is f_bavail. If this package used f_bfree the
// test above would already fail, but only on a filesystem that has a
// reserve - a tmpfs has none, and a test that happened to run on one
// would pass. So this asserts the relationship itself.
func TestAvailableIsWhatANonRootProcessMayHave(t *testing.T) {
	s, err := Read(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.AvailBytes > s.TotalBytes-s.UsedBytes {
		t.Errorf("available (%d) is more than total minus used (%d). That is only "+
			"possible if the reserve is being counted as free, which promises space no "+
			"account in this product can write",
			s.AvailBytes, s.TotalBytes-s.UsedBytes)
	}
	if s.ReservedBytes < 0 {
		t.Errorf("the reserve is negative (%d)", s.ReservedBytes)
	}
	if s.UsedBytes+s.AvailBytes+s.ReservedBytes != s.TotalBytes {
		t.Errorf("used %d + available %d + reserved %d = %d, and the filesystem is %d. "+
			"The three parts have to account for the whole, or the page shows a bar "+
			"with a gap nobody can explain",
			s.UsedBytes, s.AvailBytes, s.ReservedBytes,
			s.UsedBytes+s.AvailBytes+s.ReservedBytes, s.TotalBytes)
	}
}

// TestAMissingDirectoryIsAnErrorAndNotAFullDisk.
//
// The zero value of Space is a filesystem with no room. Returning it for
// a path that is not there would render as "0 bytes available" on the
// health page, which sends somebody to delete files on a disk that is
// fine, to fix a configuration line that is wrong.
func TestAMissingDirectoryIsAnErrorAndNotAFullDisk(t *testing.T) {
	_, err := Read(filepath.Join(t.TempDir(), "not-here"))
	if err == nil {
		t.Fatal("reading a directory that does not exist reported success")
	}
	if !strings.Contains(err.Error(), "not-here") {
		t.Errorf("the error does not name the path: %v", err)
	}
}

// TestTheRootDirectoryIsAMountPoint.
//
// filepath.Dir("/") is "/", so comparing device numbers with the parent
// says "not a mount" for the one path that always is one. Wrong in
// exactly one case, which is how it would have survived.
func TestTheRootDirectoryIsAMountPoint(t *testing.T) {
	s, err := Read("/")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Mount {
		t.Error("/ was reported as not being a mount point")
	}
}

// TestAnOrdinaryDirectoryIsNotAMountPoint is the negative half, so the
// test above cannot be satisfied by always answering true.
func TestAnOrdinaryDirectoryIsNotAMountPoint(t *testing.T) {
	s, err := Read(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.Mount {
		t.Errorf("%s was reported as its own mount point", s.Path)
	}
}

// TestTwoPathsOnOneDiskShareADevice.
//
// The number the page groups by. If Device came from f_fsid it could be
// zero for every filesystem, and three disks would collapse into one row
// showing one disk's numbers.
func TestTwoPathsOnOneDiskShareADevice(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "alt")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	a, err := Read(root)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Read(child)
	if err != nil {
		t.Fatal(err)
	}
	if a.Device != b.Device {
		t.Errorf("two directories on one filesystem reported devices %d and %d",
			a.Device, b.Device)
	}
	if a.Device == 0 {
		t.Error("the device is zero, so every filesystem would group together")
	}
}
