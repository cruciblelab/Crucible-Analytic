package web

import (
	"context"
	"sort"
	"strconv"

	"github.com/cruciblelab/crucible-analytic/internal/diskspace"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The disk, which no page in this product has ever shown.
//
// # What the storage section was, and was not
//
// It reported four table sizes out of the database. Useful, and not the
// question: "this table is 4.2 GB" does not say whether the next write
// succeeds. A full disk stops the collector, and the collector fronts
// the customer's website - so the disk filling up is the failure with
// the largest blast radius in this product, and nothing displayed it.
//
// (The table names are deliberately not written here. An invariant reads
// this directory for them, because the panel must never read analytics
// out of the database, and a check that can be argued with in comments
// is a check that gets argued with in code.)
//
// # What it cannot say, and says instead
//
// The panel measures the filesystems it was configured with. It cannot
// measure the database's, and the temptation is to show its own numbers
// under a heading that implies otherwise.
//
// It cannot for a specific reason rather than an accident: finding a
// PostgreSQL data directory means reading `data_directory`, which needs
// a privilege the panel's role does not have and must not be given.
// Guessing /var/lib/postgresql would be right on Debian and wrong in
// every container. So the section reports the database's *size*, which
// SQL will tell anybody, and says plainly that its disk is a different
// question.
//
// *Yokluğu, "sayı bulamadım" ile aynı şey sanan bir kontrol, kusurun tam
// da ürettiği şekli görmez.*

// healthFilesystem is one filesystem and the configured directories that
// live on it.
type healthFilesystem struct {
	// Paths are the directories on this filesystem, as the page names
	// them.
	Paths []string
	// Labels are those directories' translated names.
	Labels []string

	TotalBytes    int64
	UsedBytes     int64
	AvailBytes    int64
	ReservedBytes int64

	// UsedPercent is the filled part of the bar, as a string because the
	// template writes it into an SVG attribute and the content security
	// policy forbids a style attribute.
	UsedPercent string
	// ReservedPercent is a second segment drawn after the first, and
	// ReservedOffset is where it starts.
	//
	// # Why the bar has two segments
	//
	// Measured on a real page rather than reasoned about. The first
	// version drew used against total and nothing else, and on the
	// machine it was rendered on it showed a bar 12% full beside the
	// numbers "252 GB total, 6.7 GB available".
	//
	// Both were correct and together they were a lie: a bar 12% full
	// reads as "plenty of room", and the room was 2.6%. The rest was
	// reserve, which the kernel calls free and no account in this
	// product can write.
	//
	// So the reserve is drawn. The empty part of the bar is then the
	// part somebody can actually use, which is what a reader takes a bar
	// to mean in the first place.
	ReservedPercent string
	ReservedOffset  string

	// BackupBytes is what this product's own backups occupy here, and
	// BackupPercent is their slice of the bar.
	//
	// Drawn inside the used part rather than beside it, because that is
	// what they are: a subset of the files already counted. A segment
	// added alongside would show a bar wider than the disk.
	//
	// The question it answers is the one somebody asks a moment after
	// "the disk is filling up", which is "with what". Backups are the
	// only thing on this page the reader can act on directly - they can
	// delete one.
	BackupBytes   int64
	BackupPercent string

	// AtRisk means this is a container and the directory is not on a
	// volume, so what is written here is destroyed by the next image
	// update.
	AtRisk bool

	// Error is why this filesystem could not be measured, empty when it
	// was. Per-filesystem rather than per-section: one unreadable
	// directory must not take the other rows off the page.
	Error string
}

// healthDisk is the section.
type healthDisk struct {
	Filesystems []healthFilesystem
	// Container is whether this looks like a container, which is what
	// makes AtRisk meaningful.
	Container bool
	// DatabaseBytes is the size of the database itself.
	DatabaseBytes int64
	// DatabaseKnown separates "the database is empty" from "the size
	// could not be read". Without it a failed query renders as a
	// database of zero bytes, which is a sentence somebody would act on.
	DatabaseKnown bool
	// DatabaseLocal is whether the database runs on this machine, from
	// the DSN. It decides which of two honest sentences the page prints
	// about a disk it cannot see either way.
	DatabaseLocal bool
	// UnplacedBackupBytes is what the backups occupy on a filesystem this
	// page is not drawing: one taken before the device was recorded, one
	// whose filesystem could not be read, or one written to a directory
	// nobody configured the panel with.
	//
	// Shown as its own figure rather than folded into a bar. Adding it to
	// any of the bars would put bytes on a disk that may not hold them,
	// and dropping it would make the page disagree with the backup
	// section's own total for no reason a reader could see.
	UnplacedBackupBytes int64
	// Error is a failure that took the whole section, which at this
	// point means only "nothing was configured to measure".
	Error string
}

// healthDiskSection gathers it.
//
// Takes the store rather than reading it off the server, so the section
// can be built with no database at all. See stores.go.
func (s *Server) healthDiskSection(ctx context.Context, db diskStore, lang *ui.Language) healthDisk {
	out := healthDisk{
		Container:     diskspace.InContainer(),
		DatabaseLocal: s.DatabaseIsLocal,
	}

	if bytes, err := db.DatabaseBytes(ctx); err != nil {
		s.logger().Error("panel: reading the database size", "err", err)
	} else {
		out.DatabaseBytes, out.DatabaseKnown = bytes, true
	}

	// Read before the filesystems, so a failure here costs the bar's
	// extra segment and nothing else. An empty map draws the page the
	// way it looked before this existed, which is the right fallback for
	// a decoration on top of a measurement.
	backups, unplaced, err := db.BackupBytesByDevice(ctx)
	if err != nil {
		s.logger().Error("panel: reading what the backups occupy", "err", err)
		backups = nil
	}
	out.UnplacedBackupBytes = unplaced

	if len(s.StoragePaths) == 0 {
		out.Error = lang.T("saglik.disk.yol_yok")
		return out
	}

	// Grouped by device, so two directories on one disk are one row.
	// Without this a plain install shows the same numbers twice and
	// invites somebody to add them together, which is exactly twice the
	// truth.
	byDevice := map[uint64]*healthFilesystem{}
	var order []uint64
	for _, p := range s.StoragePaths {
		label := lang.T("saglik.disk.yol." + p.Key)
		space, err := diskspace.Read(p.Path)
		if err != nil {
			s.logger().Error("panel: measuring a configured directory",
				"path", p.Path, "err", err)
			out.Filesystems = append(out.Filesystems, healthFilesystem{
				Paths:  []string{p.Path},
				Labels: []string{label},
				Error:  lang.T("saglik.disk.olculemedi"),
			})
			continue
		}
		if fs, ok := byDevice[space.Device]; ok {
			fs.Paths = append(fs.Paths, p.Path)
			fs.Labels = append(fs.Labels, label)
			// One directory on a volume and another beside it cannot
			// happen - they are the same filesystem - but a row that
			// claimed otherwise would be unfalsifiable, so the risk is
			// kept as the OR of what was measured.
			fs.AtRisk = fs.AtRisk || diskspace.AtRisk(space)
			continue
		}
		fs := &healthFilesystem{
			Paths:         []string{p.Path},
			Labels:        []string{label},
			TotalBytes:    space.TotalBytes,
			UsedBytes:     space.UsedBytes,
			AvailBytes:    space.AvailBytes,
			ReservedBytes: space.ReservedBytes,
			BackupBytes:   backups[int64(space.Device)],
			AtRisk:        diskspace.AtRisk(space),
		}
		fs.UsedPercent, fs.ReservedOffset, fs.ReservedPercent = barSegments(space)
		fs.BackupPercent = backupSegment(space, fs.BackupBytes)
		byDevice[space.Device] = fs
		order = append(order, space.Device)
	}
	for _, dev := range order {
		out.Filesystems = append(out.Filesystems, *byDevice[dev])
	}
	// Deterministic: the failures collected above went straight onto the
	// slice and the measured ones came out of a map, so without this the
	// order of a page with one unreadable directory changes per request.
	sort.SliceStable(out.Filesystems, func(i, j int) bool {
		return out.Filesystems[i].Paths[0] < out.Filesystems[j].Paths[0]
	})
	return out
}

// barSegments splits the bar into what files occupy and what the
// reserve holds, leaving the remainder as what can still be written.
//
// # Why the reserve's width is derived rather than computed
//
// It is the distance from the used part to where the unavailable part
// ends, not the reserve's own bytes as a percentage.
//
// The first reason written here was that two independently rounded
// percentages could sum past a hundred and draw a bar wider than its
// box. That was wrong, and a mutation proved it: both are rounded down,
// so their sum cannot exceed the whole. The reason was plausible, it
// was written before it was checked, and it justified code that happens
// to be right for a different reason.
//
// The real one is about the gap at the end, which is the only part of
// this picture anybody reads a decision off. Empty bar means room left.
// Rounding each segment down separately loses up to a point on each and
// gives that lost width to the gap, so the bar can promise nearly two
// per cent of a filesystem that is not there. Deriving the second
// segment from the far end rounds once, and the overstatement is
// bounded by a single point.
//
// Two per cent of a small disk is not much. It is also exactly the kind
// of number somebody is squinting at when the disk is nearly full.
func barSegments(s diskspace.Space) (used, reservedAt, reserved string) {
	if s.TotalBytes <= 0 {
		return "", "", ""
	}
	usedPct := clampPercent(s.UsedBytes * 100 / s.TotalBytes)
	// Everything that is not available: files plus reserve. Measured
	// from the available end so the two segments cannot overrun.
	unavailPct := clampPercent((s.TotalBytes - s.AvailBytes) * 100 / s.TotalBytes)
	if unavailPct < usedPct {
		unavailPct = usedPct
	}
	if unavailPct == usedPct {
		// No reserve worth drawing. An empty width rather than a zero
		// one: the template omits the element entirely, and a rect of
		// width zero is a thing some renderers still draw a hairline for.
		return strconv.FormatInt(usedPct, 10), "", ""
	}
	return strconv.FormatInt(usedPct, 10),
		strconv.FormatInt(usedPct, 10),
		strconv.FormatInt(unavailPct-usedPct, 10)
}

// backupSegment is the backups' slice of the bar, drawn from the left
// inside what the used part already covers.
//
// # Why it is clamped to the used part rather than to the whole bar
//
// The two numbers come from different places and are read at different
// moments: the disk from statfs, the bytes from a catalogue the upgrader
// wrote. A file deleted from a shell between the two makes the catalogue
// larger than the disk, and so does a backup directory that is not on
// the filesystem this row is about, if a device number is ever reused.
//
// A segment wider than the used part would draw backups over the empty
// space and say the disk is fuller than it is. Clamped, the worst case
// is a bar whose used part is entirely backups - which, when the
// catalogue really does account for everything on a dedicated backup
// volume, is also the truth.
//
// # Why it rounds down, after being written rounding up
//
// The first version rounded up, so that a small backup would still draw
// something rather than disappearing. The reasoning was that backups are
// the only thing on this page a reader can act on, and a segment of
// nothing hides it.
//
// Then the page was rendered. 73 KB of backups on a 252 GB filesystem
// drew a segment one per cent wide - about two and a half gigabytes of
// bar - directly beside the words "Yedekler 73,2 KB". The picture and
// its own caption disagreed by four orders of magnitude, and the picture
// is the part people believe.
//
// So it rounds down like every other segment here, and a backup too
// small to draw draws nothing. The figure in the list carries that case,
// which is what a figure is for. A bar is for proportions, and a
// proportion that is not there is not a proportion to draw.
//
// *Bir tahminin ölçülmüş olması, ölçülen şeyin temsil ettiği anlamına
// gelmez* - and neither does a reason that was written before the thing
// it justifies was looked at.
func backupSegment(s diskspace.Space, backupBytes int64) string {
	if s.TotalBytes <= 0 || backupBytes <= 0 {
		return ""
	}
	usedPct := clampPercent(s.UsedBytes * 100 / s.TotalBytes)
	pct := clampPercent(backupBytes * 100 / s.TotalBytes)
	if pct > usedPct {
		pct = usedPct
	}
	if pct <= 0 {
		return ""
	}
	return strconv.FormatInt(pct, 10)
}

func clampPercent(p int64) int64 {
	if p < 0 {
		return 0
	}
	if p > 100 {
		return 100
	}
	return p
}
