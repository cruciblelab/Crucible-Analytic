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
//
// # Why df is bracketed rather than compared against once
//
// The first version of this test read the filesystem once, ran df, and
// allowed the two answers to differ by one part in a thousand. It failed
// on this machine at 6596 KiB of drift against a 6156 KiB tolerance -
// and the code was right. A `go test ./...` run writes hundreds of
// megabytes of build cache and test binaries while this test is running,
// so the free space genuinely changes between the two readings.
//
// The fix is not a wider tolerance. A tolerance wide enough for an
// arbitrarily busy machine is a tolerance wide enough to accept a real
// mistake, and picking one is guessing at how busy CI will be.
//
// So the filesystem is read on both sides of df, and df's answer has to
// fall inside that interval. On a quiet machine the two readings are
// identical and this is exact equality. On a busy one the interval is
// exactly as wide as the disk actually moved, which is the honest
// allowance - it is derived from the machine rather than chosen.
//
// *Bir tahminin ölçülmüş olması, ölçülen şeyin temsil ettiği anlamına
// gelmez.*
func TestTheNumbersAgreeWithDf(t *testing.T) {
	dir := t.TempDir()

	before, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	total, used, avail := dfKB(t, dir)
	after, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Compared in kibibytes, because that is df's unit and rounding it
	// the other way would invent a disagreement.
	const kb = 1024
	for _, c := range []struct {
		what        string
		lo, hi, dfs int64
	}{
		{"total", before.TotalBytes / kb, after.TotalBytes / kb, total},
		{"used", before.UsedBytes / kb, after.UsedBytes / kb, used},
		{"available", before.AvailBytes / kb, after.AvailBytes / kb, avail},
	} {
		lo, hi := c.lo, c.hi
		if lo > hi {
			lo, hi = hi, lo
		}

		// A check that cannot fail is worse than no check. If the disk
		// moved by more than a percent while df was running, the interval
		// is wide enough to swallow the root reserve - which is the
		// smallest of the mistakes this test exists to catch - so it
		// says so rather than passing.
		if hi-lo > c.dfs/100 {
			t.Skipf("%s moved from %d to %d KiB while df ran, which is too much for this "+
				"comparison to mean anything. Run it on a quieter machine", c.what, lo, hi)
		}

		// 64 KiB of slack for the two roundings: this package truncates
		// bytes to kibibytes and df rounds its own blocks.
		const slack = 64
		if c.dfs < lo-slack || c.dfs > hi+slack {
			t.Errorf("%s: df says %d KiB, and this package read %d..%d KiB around it",
				c.what, c.dfs, lo, hi)
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
