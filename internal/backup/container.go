package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cruciblelab/crucible-analytic/internal/diskspace"
)

// The file, and everything true of every file this package writes.
//
// # Why this is its own function
//
// There are two kinds of backup - the data dump and the secrets file -
// and almost nothing they have in common is about their contents. What
// they share is the part that is easy to get subtly wrong and whose
// failures are invisible until somebody needs the file:
//
//   - the directory is 0700 even when it already existed
//   - the bytes go to a dot-name and take the real name only when they
//     are complete and on the disk
//   - the checksum is of what was written, not of a second read
//   - fsync happens before the rename, not after
//   - the rename refuses to land on a name that is already taken
//   - a failed run leaves nothing behind
//
// Every one of those is a defect this project already had. The
// filename collision destroyed a backup and left two catalogue rows
// pointing at one file; the directory mode left every backup's name,
// date and size world-readable. A second copy of this logic would be a
// second place for each of them to come back, and the whole reason they
// are fixed is that they are fixed once.
//
// So the contents are a callback and the container is not.

// ErrDiskFilling is returned when a backup was stopped because the
// filesystem it was being written to ran down to its margin.
var ErrDiskFilling = errors.New("backup: stopped to keep the disk from filling")

// GuardEvery is the most that is written between two free-space
// readings.
//
// Eight megabytes. The reading is one statfs, so the cost of checking is
// not what sets this; what sets it is how far past the margin a write
// can get before anybody looks.
//
// It is a ceiling and not the interval, because on a small filesystem it
// would be larger than the thing it is protecting. The margin is a tenth
// of the filesystem when that is under a gigabyte, so a 64 MB volume -
// which is what a container's backup mount can be - has a 6.4 MB margin,
// and checking every 8 MB could step over it between two readings. The
// interval is therefore a quarter of the margin when that is smaller,
// which makes the worst overshoot a quarter of the margin by
// construction rather than by the filesystem happening to be big.
const GuardEvery = 8 << 20

// spaceGuard stops a write before the filesystem fills.
//
// # Why an estimate is not enough
//
// Measure predicts the file's size from the tables' size, and that
// prediction was wrong by a factor of two in the direction that matters
// on one of three measured kinds of data. It will be wrong again: the
// ratio depends on what the rows contain, and this code does not know
// what the rows contain until it has read them.
//
// There is also a second writer to the same disk that no estimate can
// see. The database keeps running while the backup is taken. A machine
// with room for the file when the button was pressed can be out of room
// twenty minutes later without the backup having been wrong about
// anything.
//
// So the estimate refuses the hopeless cases cheaply, and this refuses
// the rest at the moment they become hopeless. container removes the
// temporary file on the way out, so a stopped backup gives back every
// byte it took.
//
// *Bir tahmin, tahmin olduğu için garanti olamaz.*
type spaceGuard struct {
	w   io.Writer
	dir string

	// margin is what must stay free, and every is how often to look.
	// Both come from the filesystem's size, which does not change while
	// the file is being written; the free space does, and that is what
	// gets re-read.
	//
	// Zero every means this filesystem could not be measured at all, and
	// the guard passes everything through. Read fails on anything but
	// Linux, and a backup that refused to run wherever it could not
	// measure would be a backup that refuses to run on a developer's
	// machine.
	margin int64
	every  int64

	since int64
}

// newSpaceGuard measures the filesystem once and returns a guard for it.
func newSpaceGuard(w io.Writer, dir string) *spaceGuard {
	g := &spaceGuard{w: w, dir: dir}
	space, err := diskspace.Read(dir)
	if err != nil {
		return g
	}
	g.margin = MarginFor(space.TotalBytes)
	g.every = GuardEvery
	if quarter := g.margin / 4; quarter > 0 && quarter < g.every {
		g.every = quarter
	}
	return g
}

func (g *spaceGuard) Write(p []byte) (int, error) {
	if g.every > 0 {
		g.since += int64(len(p))
		if g.since >= g.every {
			g.since = 0
			if space, err := diskspace.Read(g.dir); err == nil &&
				space.AvailBytes < g.margin {
				return 0, fmt.Errorf("%w: %s has %d bytes free and %d must stay "+
					"free. The partial file has been removed",
					ErrDiskFilling, g.dir, space.AvailBytes, g.margin)
			}
		}
	}
	return g.w.Write(p)
}

// container writes one tar.gz into dir under name and reports what it
// made.
//
// fill is called with the open archive and writes the members. Whatever
// it returns is returned, and nothing is left on the disk when it
// returns an error.
func container(dir, name string, fill func(tw *tar.Writer) error) (Result, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Result{}, fmt.Errorf("backup: preparing %s: %w", dir, err)
	}
	// And tightened even when it was already there.
	//
	// MkdirAll's mode applies only to directories it creates. A backup
	// directory the operator made by hand, or one left by an earlier
	// install, keeps whatever mode it had - 0755 on most machines - and
	// the 0600 on the files then protects their contents while leaving
	// every backup's name, date and size readable by anyone with an
	// account.
	//
	// Found by a test asserting the directory's mode, not by reading
	// this function: MkdirAll looks like it sets the mode, and it does,
	// on exactly the runs where the directory did not exist yet.
	//
	// Tightened rather than refused, because the alternative is a
	// customer pressing a button and being told to go and chmod
	// something. Nothing but backups belongs in here.
	if err := os.Chmod(dir, 0o700); err != nil {
		return Result{}, fmt.Errorf("backup: restricting %s: %w", dir, err)
	}

	final := filepath.Join(dir, name)
	tmp := filepath.Join(dir, "."+name+".tmp")

	// 0600: the four services run as `crucible` and a data backup holds
	// every row they are not allowed to read, plus the panel's password
	// hashes and TOTP secrets. Only the account that wrote it may open
	// it, and that is the upgrader's.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return Result{}, fmt.Errorf("backup: creating %s: %w", tmp, err)
	}
	// Removed on every path that does not rename it. A failed backup
	// that left forty megabytes behind each time somebody retried would
	// fill the disk of the machine least able to spare it.
	defer func() {
		if _, statErr := os.Stat(tmp); statErr == nil {
			_ = os.Remove(tmp)
		}
	}()

	// Hashed as it is written rather than by reading the file back: the
	// checksum is of what went to the disk, and a second pass could hash
	// a file something else had changed in between.
	sum := sha256.New()
	counted := &countingWriter{w: io.MultiWriter(newSpaceGuard(f, dir), sum)}
	gz := gzip.NewWriter(counted)
	tw := tar.NewWriter(gz)

	if err := fill(tw); err != nil {
		_ = tw.Close()
		_ = gz.Close()
		_ = f.Close()
		return Result{}, err
	}

	if err := tw.Close(); err != nil {
		_ = f.Close()
		return Result{}, fmt.Errorf("backup: closing the archive: %w", err)
	}
	if err := gz.Close(); err != nil {
		_ = f.Close()
		return Result{}, fmt.Errorf("backup: closing the compressor: %w", err)
	}
	// Sync before rename, not after.
	//
	// rename is atomic in the directory entry, and says nothing about
	// whether the bytes reached the disk. A machine that loses power
	// between the two comes back with a file under its final name and no
	// contents - which is exactly the shape the temporary name exists to
	// prevent, arrived at from the other side.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return Result{}, fmt.Errorf("backup: flushing %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return Result{}, fmt.Errorf("backup: closing %s: %w", tmp, err)
	}
	// Never onto a file that is already there.
	//
	// rename replaces an existing name silently and atomically, which is
	// the property that makes it right for everything above and wrong
	// for exactly this. A backup landing on the name of an older backup
	// destroys it, reports success, and leaves the catalogue with two
	// rows - two dates, two sizes, two checksums - describing one file.
	// The customer sees two backups and has one.
	//
	// Runner.fileName carries milliseconds so this should not be
	// reachable. "Should not be reachable" is the reason to check rather
	// than the reason to skip it: the name is a timestamp, and a clock
	// that steps backwards produces one that has been used before.
	//
	// The gap between this and the rename is a race only another writer
	// could enter, and the queue's one-in-flight index means there is no
	// other writer. It is not the case being defended against.
	if _, err := os.Stat(final); err == nil {
		return Result{}, fmt.Errorf("backup: %s already exists; refusing to write over "+
			"a backup that is already there", final)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("backup: checking %s: %w", final, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return Result{}, fmt.Errorf("backup: naming %s: %w", final, err)
	}
	if err := syncDir(dir); err != nil {
		return Result{}, err
	}

	out := Result{
		Path:   final,
		Bytes:  counted.n,
		SHA256: hex.EncodeToString(sum.Sum(nil)),
	}

	// Which filesystem this landed on, so the panel can draw it into the
	// right bar. See panel_backups.device.
	//
	// Read after the file exists rather than before, because the answer
	// is about the file: a directory created moments ago on a
	// filesystem that has since been remounted would give the wrong one.
	//
	// A failure here is not a failure of the backup. The file is written,
	// synced and named; the device is a label for a picture. So it is
	// left at zero, which the panel reads as "not on any bar I am
	// drawing" rather than as an error.
	if space, err := diskspace.Read(final); err == nil {
		out.Device = int64(space.Device)
	}
	return out, nil
}
