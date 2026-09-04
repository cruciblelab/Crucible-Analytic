// Package diskspace measures the filesystems this product writes to.
//
// # What was here before, and why it was not enough
//
// The setup checks already measured free space: internal/panel/preflight
// carried its own Statfs, reading f_bavail correctly, behind a check
// called "disk.free". That check answers one question - is there at
// least N bytes free - once, during installation, as a pass or a fail.
//
// It is not the question the rest of this needs answered. "There is more
// than two gigabytes free" does not say how much out of how much, on
// which filesystem, or whether what gets written there survives a
// container image update. And the health page has never shown a disk
// number at all: its storage section reports four table sizes read out
// of the database, and a table size does not say whether the next write
// succeeds.
//
// So this package is the measurement, and preflight's check is one of
// its callers rather than a second implementation. Two Statfs calls in
// one repository is one too many, and the copy that drifts is always the
// one nobody is looking at.
//
// # Why it is not inside the panel
//
// Because the panel is not its only caller. The component that decides
// whether a backup fits is the one that writes it, and that is not the
// panel: the panel's role cannot read the analytics tables, so it cannot
// produce a dump, and it must not gain the ability.
//
// # Platform
//
// Read works on Linux. Everywhere else it returns an error saying so,
// for the reason preflight's version already gave: this has to compile
// where developers work, and an honest "not measured" beats both a build
// failure and a guess.
package diskspace

import "math"

// Space is one filesystem, measured.
type Space struct {
	// Path is the directory that was asked about.
	Path string

	// Device identifies the filesystem.
	//
	// Two configured paths are usually on one disk - a plain install
	// puts /var/log/crucible-analytic and /var/lib/crucible-analytic
	// both on the root filesystem - and reporting them as two rows with
	// identical numbers invites somebody to add them together. That
	// answer would be exactly twice the truth.
	Device uint64

	// TotalBytes is the filesystem's size.
	TotalBytes int64

	// AvailBytes is what a process that is not root may still write.
	//
	// This is f_bavail, not f_bfree, and the difference is not small.
	// Measured on the machine this was written on: the kernel reports
	// 222 GB free and 7 GB available, the rest being reserved. Every
	// process in this product runs as an ordinary account - `crucible`
	// for the four services, `crucible-upgrader` for the upgrader - so
	// f_bfree would promise space none of them can have.
	//
	// The failure that promise produces is not a wrong number on a page.
	// It is a backup that passes a "will it fit" check, runs for four
	// minutes, and dies at 96% with a partial file, on the machine whose
	// disk was already nearly full.
	AvailBytes int64

	// UsedBytes is what files occupy.
	UsedBytes int64

	// ReservedBytes is the root reserve: counted as free by the kernel
	// and unavailable to everything here.
	//
	// Exposed rather than folded into one of the others, because
	// Used + Avail does not equal Total and somebody will eventually
	// notice. Better that they find a named field than decide the
	// arithmetic is broken and "fix" it by switching to f_bfree.
	ReservedBytes int64

	// Mount is whether Path is its own mount point.
	//
	// The question behind it is "does what I write here survive". In a
	// container it is decisive: a path on the image's writable layer is
	// destroyed by the next `docker compose up -d`, and a path on a
	// volume is not. On an ordinary server it is neither good nor bad
	// news - /var/lib on the root filesystem is the normal arrangement -
	// which is why the warning this feeds needs both halves. See AtRisk.
	Mount bool
}

// Read measures the filesystem holding path.
//
// An error rather than a zero Space when the path is not there. A
// configured directory that does not exist is a real misconfiguration
// and the caller can say so; a zero Space renders as a disk with no room
// left, which sends somebody to delete files on a healthy disk in order
// to fix a wrong line in a config file.
func Read(path string) (Space, error) { return read(path) }

// blocksToBytes multiplies without wrapping.
//
// Saturating rather than asserting it cannot happen: it needs a
// filesystem of nine exabytes and there is not one, but "a negative
// number of bytes free" is a worse failure than "a number stuck at the
// maximum", and the check costs a comparison on a path that runs when
// somebody opens a page.
func blocksToBytes(blocks uint64, unit int64) int64 {
	if unit <= 0 || blocks == 0 {
		return 0
	}
	if blocks > uint64(math.MaxInt64)/uint64(unit) {
		return math.MaxInt64
	}
	return int64(blocks) * unit
}
