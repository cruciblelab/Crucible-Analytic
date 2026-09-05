package web

import (
	"strconv"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/diskspace"
)

// The bar, and the reading it has to survive.
//
// # The defect these exist because of
//
// The first version drew one segment: used against total. Every number
// beside it was right and the picture was wrong. Rendered on a real
// machine it showed a bar 12% full next to "252 GB total, 6.6 GB
// available" - because 85% of that filesystem was root reserve, which
// the kernel calls free and no account in this product can write.
//
// A bar is read as "this much is gone, the rest is yours". So the
// reserve is drawn too, and the empty part of the bar is what is
// actually left.
//
// It was found by looking at the page, not by testing it. These tests
// are what keep it found.

// segmentsOf is the three widths for one hypothetical filesystem.
func segmentsOf(total, used, avail int64) (string, string, string) {
	return barSegments(diskspace.Space{
		TotalBytes:    total,
		UsedBytes:     used,
		AvailBytes:    avail,
		ReservedBytes: total - used - avail,
	})
}

func num(t *testing.T, s string) int64 {
	t.Helper()
	if s == "" {
		return 0
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("%q is not a number the SVG can use: %v", s, err)
	}
	return v
}

// TestTheEmptyPartOfTheBarIsWhatCanBeWritten.
//
// The claim the whole picture makes. Whatever the reserve is, the gap at
// the end has to match what is available - or the bar says there is room
// where there is none.
func TestTheEmptyPartOfTheBarIsWhatCanBeWritten(t *testing.T) {
	for _, c := range []struct {
		name                     string
		total, used, avail       int64
		wantEmptyPercentAtLeast  int64
		wantEmptyPercentAtMostPl int64
	}{
		// The real filesystem this was found on: a large reserve, and a
		// bar that looked comfortable.
		{"büyük ayrılmış pay", 252 << 30, 30 << 30, 6 << 30, 2, 3},
		// An ordinary disk with the usual five per cent.
		{"olağan ext4", 100 << 30, 50 << 30, 45 << 30, 45, 45},
		// No reserve at all, which is a tmpfs.
		{"ayrılmış pay yok", 64 << 20, 16 << 20, 48 << 20, 75, 75},
	} {
		t.Run(c.name, func(t *testing.T) {
			used, _, reserved := segmentsOf(c.total, c.used, c.avail)
			empty := 100 - num(t, used) - num(t, reserved)
			if empty < c.wantEmptyPercentAtLeast || empty > c.wantEmptyPercentAtMostPl {
				t.Errorf("the bar leaves %d%% empty and %d%% of the filesystem is "+
					"available. The empty part of a bar is read as the room left, so "+
					"those two have to be the same number",
					empty, c.avail*100/c.total)
			}
		})
	}
}

// TestTheBarNeverPromisesRoomThatIsNotThere.
//
// # The test this replaced, and why it was worthless
//
// The first version here asserted that the two segments never sum past
// a hundred, on the reasoning that two independently rounded
// percentages could overflow the bar. A mutation that switched the
// reserve back to independent rounding passed it, which is how the
// reasoning was found to be wrong: both percentages round *down*, so
// their sum cannot exceed the whole and the hazard did not exist.
//
// The test was green, the code was right, and the sentence explaining
// why was false. That is worse than a missing test: it is a reason
// somebody would trust.
//
// # What actually differs
//
// The gap at the end. It is the only part of this picture a decision
// gets read off - empty bar means room left - and rounding each segment
// down separately gives the lost fraction of *each* to the gap. So the
// bar can promise up to two per cent of a filesystem that is not there,
// against one for a single rounding.
//
// The bound below is ceil(available), which the derived version meets
// and the independently rounded one does not.
func TestTheBarNeverPromisesRoomThatIsNotThere(t *testing.T) {
	for _, c := range []struct {
		name               string
		total, used, avail int64
	}{
		// The case that separates the two: both percentages sit just
		// past a boundary, so rounding each down loses most of a point
		// twice.
		{"iki yüzde de sınırın hemen üstünde", 1000, 156, 688},
		{"büyük ayrılmış pay", 252 << 30, 30 << 30, 6 << 30},
		{"olağan ext4", 100 << 30, 50 << 30, 45 << 30},
		{"neredeyse dolu", 1000, 990, 1},
		{"tamamen dolu", 1000, 1000, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			used, _, reserved := segmentsOf(c.total, c.used, c.avail)
			empty := 100 - num(t, used) - num(t, reserved)
			// Ceiling, because one rounding of one quantity is the most
			// the drawing may cost.
			ceilAvail := (c.avail*100 + c.total - 1) / c.total
			if empty > ceilAvail {
				t.Errorf("the bar leaves %d%% empty and at most %d%% of the filesystem "+
					"is available. An empty bar is read as room left, and this one is "+
					"offering space that does not exist", empty, ceilAvail)
			}
			if empty < 0 {
				t.Errorf("the segments cover %d%% of the bar", 100-empty)
			}
		})
	}
}

// TestTheSegmentsMeetWithoutAGap.
//
// The reserve is drawn starting where the used part ends. A wrong
// offset draws either a stripe of background between two filled parts,
// which reads as free space in the middle of the disk, or an overlap.
func TestTheSegmentsMeetWithoutAGap(t *testing.T) {
	used, at, reserved := segmentsOf(252<<30, 30<<30, 6<<30)
	if reserved == "" {
		t.Fatal("a filesystem with a large reserve drew no second segment")
	}
	if num(t, at) != num(t, used) {
		t.Errorf("the used part ends at %s and the reserve starts at %s", used, at)
	}
}

// TestAFilesystemWithNoReserveDrawsNoSecondSegment.
//
// An empty width rather than a zero one: some renderers draw a hairline
// for a rect of width zero, and a hairline at the end of the used part
// reads as a boundary that is not there.
func TestAFilesystemWithNoReserveDrawsNoSecondSegment(t *testing.T) {
	used, at, reserved := segmentsOf(64<<20, 16<<20, 48<<20)
	if used == "" {
		t.Error("a filesystem in use drew no filled part at all")
	}
	if reserved != "" || at != "" {
		t.Errorf("a filesystem with no reserve drew a second segment at %q width %q",
			at, reserved)
	}
}

// TestAnUnmeasurableFilesystemDrawsNothing.
//
// Zero total is what a Space that could not be read looks like. A bar of
// width zero on a total of zero is 0/0, and the arithmetic below would
// divide by it.
func TestAnUnmeasurableFilesystemDrawsNothing(t *testing.T) {
	used, at, reserved := segmentsOf(0, 0, 0)
	if used != "" || at != "" || reserved != "" {
		t.Errorf("a filesystem of zero bytes drew %q/%q/%q", used, at, reserved)
	}
}

// The backup segment, drawn inside the used part.
//
// # What it is for
//
// "The disk is filling up" is followed immediately by "with what", and
// backups are the only thing on this page a reader can act on: they can
// delete one. Everything else on the bar is the operating system's
// business or the database's.

// backupSegmentOf is the segment for one hypothetical filesystem.
func backupSegmentOf(total, used, avail, backups int64) string {
	return backupSegment(diskspace.Space{
		TotalBytes:    total,
		UsedBytes:     used,
		AvailBytes:    avail,
		ReservedBytes: total - used - avail,
	}, backups)
}

// TestTheBackupSegmentNeverLeavesTheUsedPart.
//
// # What goes wrong without it
//
// The two numbers come from different places and are read at different
// moments: the disk from statfs, the bytes from a catalogue the upgrader
// wrote. They can disagree - a file deleted from a shell after the sweep
// last ran is still in the catalogue - and the disagreement is in the
// direction that matters, with the catalogue larger than the disk.
//
// An unclamped segment would then paint backups over the empty part of
// the bar, which is the one part anybody reads a decision off. The page
// would say the disk is fuller than it is, on the strength of a file
// that is not there.
func TestTheBackupSegmentNeverLeavesTheUsedPart(t *testing.T) {
	const gb = int64(1) << 30
	for _, c := range []struct {
		name                      string
		total, used, avail, backs int64
	}{
		{"catalogue larger than the disk", 100 * gb, 10 * gb, 90 * gb, 80 * gb},
		{"catalogue larger than the whole disk", 100 * gb, 10 * gb, 90 * gb, 400 * gb},
		{"backups are everything on the disk", 100 * gb, 40 * gb, 60 * gb, 40 * gb},
		{"ordinary", 100 * gb, 40 * gb, 60 * gb, 5 * gb},
	} {
		t.Run(c.name, func(t *testing.T) {
			used, _, _ := segmentsOf(c.total, c.used, c.avail)
			got := num(t, backupSegmentOf(c.total, c.used, c.avail, c.backs))
			if got > num(t, used) {
				t.Errorf("the backup segment is %d%% and the used part is %s%%.\n"+
					"The extra is drawn over the empty end of the bar, which is the "+
					"part somebody reads to decide whether there is room", got, used)
			}
			if got < 0 {
				t.Errorf("a negative width (%d) is not something an SVG can draw", got)
			}
		})
	}
}

// TestTheBackupSegmentNeverClaimsMoreThanItIs.
//
// # The version of this test that was wrong
//
// It used to assert the opposite: that a small backup still drew
// something, on the reasoning that backups are the only thing on this
// page a reader can act on, so a segment of nothing hides it. The code
// rounded up to match.
//
// Then the page was rendered. 73 KB of backups on a 252 GB filesystem
// drew one per cent of the bar - about two and a half gigabytes' worth -
// directly beside the words "Yedekler 73,2 KB". The picture disagreed
// with its own caption by four orders of magnitude, and the picture is
// the half people believe.
//
// So the property is the honest one: the segment is never wider than the
// backups' real share, rounded to the same whole percent every other
// segment uses. A backup too small to draw draws nothing, and the figure
// in the list carries that case - which is what a figure is for.
func TestTheBackupSegmentNeverClaimsMoreThanItIs(t *testing.T) {
	const gb = int64(1) << 30
	const kb = int64(1) << 10
	for _, c := range []struct {
		name                      string
		total, used, avail, backs int64
	}{
		{"a rounding error's worth on a large disk", 252 * gb, 30 * gb, 6 * gb, 73 * kb},
		{"a tenth of a per cent", 100 * gb, 50 * gb, 50 * gb, 100 * (1 << 20)},
		{"a third of the disk", 100 * gb, 40 * gb, 60 * gb, 33 * gb},
		{"the whole used part", 100 * gb, 40 * gb, 60 * gb, 40 * gb},
	} {
		t.Run(c.name, func(t *testing.T) {
			drawn := num(t, backupSegmentOf(c.total, c.used, c.avail, c.backs))
			truth := c.backs * 100 / c.total
			if drawn > truth {
				t.Errorf("the bar draws %d%% for backups that are %d%% of the disk.\n"+
					"The figure beside it says the real size, and a picture that "+
					"disagrees with its own caption is the half people believe",
					drawn, truth)
			}
		})
	}
}

// TestABackupWorthDrawingIsDrawn is the other side of it.
//
// Rounding down is right and it must not become "never draws anything".
// A backup taking a real share of the disk is the case the segment
// exists for, and it has to show.
func TestABackupWorthDrawingIsDrawn(t *testing.T) {
	const gb = int64(1) << 30
	got := num(t, backupSegmentOf(100*gb, 40*gb, 60*gb, 12*gb))
	if got < 11 || got > 12 {
		t.Errorf("12 GB of backups on a 100 GB disk drew %d%%, want 12", got)
	}
}

// TestNoBackupsDrawsNoSegment.
//
// An empty width rather than a zero one, for the reason barSegments
// records: the template omits the element entirely, and a rect of width
// zero is a thing some renderers still draw a hairline for. A hairline
// on a deployment that has never taken a backup is a picture of a
// feature nobody used.
func TestNoBackupsDrawsNoSegment(t *testing.T) {
	const gb = int64(1) << 30
	for _, c := range []struct {
		name  string
		backs int64
	}{
		{"none taken", 0},
		{"a negative count is not drawn either", -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := backupSegmentOf(100*gb, 40*gb, 60*gb, c.backs); got != "" {
				t.Errorf("drew %q", got)
			}
		})
	}
}
