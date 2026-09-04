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
