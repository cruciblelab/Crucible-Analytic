package backup

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/diskspace"
)

// Will it fit.
//
// # What this is for, and what it cannot do
//
// A backup that fills the disk stops the collector, and the collector is
// in front of the customer's website. So the one failure this feature
// can cause on its own is an outage, produced by the feature working
// exactly as asked.
//
// This file does not prevent that outage. It predicts, before anything
// is written, roughly what a backup will cost, so a customer pressing a
// button gets a number and a hopeless request is refused without
// touching the disk. The thing that actually prevents the outage is
// spaceGuard in container.go, which watches the filesystem *while* the
// bytes are going down and stops before the margin is gone. An estimate
// cannot be a guarantee; that is what makes it an estimate.
//
// # The number, and why it moved
//
// The share used to be one fifth, and the comment justifying it said
// this:
//
//	traffic_snapshots   16.4 MB on disk   68 KB compressed   0.4%
//	beacon_events       18.1 MB on disk   97 KB compressed   0.5%
//
//	[...] the estimate is deliberately pessimistic: it assumes the file
//	will be a *fifth* of what the tables occupy, which is forty times
//	worse than measured.
//
// The measurement was real and the conclusion drawn from it was not.
// Those rows were near-identical, so what was measured was how well gzip
// compresses repetition - not how large a customer's backup is. The
// ratio was then measured again on three kinds of row, 200 000 of them,
// through this same Measure and the real writer:
//
//	repetitive    102 MB on disk    1.7 MB file    1/60
//	realistic     142 MB on disk   15.3 MB file    1/9
//	adversarial   194 MB on disk   62.2 MB file    1/3
//
// "Realistic" is a distinct pseudonym and visitor id on every row - which
// every deployment produces, because ip_hash is 32 bytes of SHA-256 and
// nothing compresses it - with the descriptive columns drawn from a small
// pool, which every deployment also produces. "Adversarial" makes every
// text column high-entropy as well; nothing produces that.
//
// So the margin the old comment claimed was forty-fold is twofold on
// realistic data, and one fifth is *breached* by the third arm: the
// estimate would have promised 38.7 MB and the file was 62.2 MB.
//
// The share is now one third - the worst ratio measured, from the arm
// nothing real produces. Not a proven bound; the worst of three arms.
// Which is exactly why the guard exists.
//
// *Bir tahminin ölçülmüş olması, ölçülen şeyin temsil ettiği anlamına
// gelmez.*

// CompressedShare is the fraction of a table's on-disk size the estimate
// assumes the backup will take.
//
// One third: the worst of three measured arms. See above for why the
// previous one fifth was not the pessimistic figure its comment claimed,
// and why no number here can be the thing that keeps the disk from
// filling.
const CompressedShare = 3

// FreeMargin is what must still be free after the backup is written.
//
// A gigabyte, or a tenth of the filesystem, whichever is smaller. Not
// zero: the database keeps writing while the backup is taken, and a
// machine left with nothing spare is one where the next WAL segment is
// the outage instead.
const FreeMargin = 1 << 30

// MarginFor is that rule, applied to one filesystem's size.
//
// A function rather than three copies of two lines. It had three callers
// and two of them disagreed: this one and MeasureSecrets both take the
// smaller of a gigabyte and a tenth, but only MeasureSecrets asked
// whether the size was known first. On a filesystem whose total reads as
// zero - which Read reports rather than refuses - the data backup's
// margin was zero and the secrets backup's was a gigabyte. Neither was
// chosen; they were two spellings of one rule, drifting.
//
// Zero total means "not measured", so the fixed margin stands.
func MarginFor(total int64) int64 {
	if total <= 0 {
		return FreeMargin
	}
	if tenth := total / 10; tenth < FreeMargin {
		return tenth
	}
	return FreeMargin
}

// Estimate is what a backup of these sets would cost.
type Estimate struct {
	// TableBytes is what the tables occupy on disk.
	TableBytes int64
	// FileBytes is the pessimistic guess at the backup's size.
	FileBytes int64
	// AvailBytes is what the destination filesystem has left.
	AvailBytes int64
	// Margin is what is kept spare beyond the file.
	Margin int64
}

// Fits reports whether the backup can be written without leaving the
// disk too full to run on.
func (e Estimate) Fits() bool {
	return e.AvailBytes >= e.FileBytes+e.Margin
}

// Short is how many bytes are missing, zero when it fits.
func (e Estimate) Short() int64 {
	if e.Fits() {
		return 0
	}
	return e.FileBytes + e.Margin - e.AvailBytes
}

// Measure sizes a backup of these sets against the destination.
//
// # Where the sizes come from
//
// hypertable_detailed_size for a hypertable, pg_total_relation_size for
// an ordinary table - the same split the health page needs, and for the
// same reason: pg_total_relation_size measures a hypertable's parent,
// which holds no rows. Using it here would size the traffic tables at
// forty kilobytes and let a backup start on a disk with no room for it.
func Measure(ctx context.Context, pool *pgxpool.Pool, dir string, sets []string) (Estimate, error) {
	tables, err := TablesFor(sets)
	if err != nil {
		return Estimate{}, err
	}

	var tableBytes int64
	if err := pool.QueryRow(ctx, `
		WITH wanted AS (
		    SELECT t.name,
		           to_regclass('public.' || t.name) AS rel,
		           EXISTS (SELECT 1 FROM timescaledb_information.hypertables h
		                   WHERE h.hypertable_name = t.name) AS hyper
		    FROM unnest($1::text[]) AS t(name)
		)
		SELECT COALESCE(sum(
		    CASE
		        WHEN w.rel IS NULL THEN 0
		        WHEN NOT w.hyper THEN COALESCE(pg_total_relation_size(w.rel), 0)
		        ELSE COALESCE((SELECT sum(total_bytes)
		                       FROM hypertable_detailed_size(w.rel)), 0)
		    END), 0)
		FROM wanted w`, tables).Scan(&tableBytes); err != nil {
		return Estimate{}, fmt.Errorf("backup: sizing the tables: %w", err)
	}

	space, err := spaceFor(dir)
	if err != nil {
		return Estimate{}, err
	}

	return Estimate{
		TableBytes: tableBytes,
		FileBytes:  tableBytes / CompressedShare,
		AvailBytes: space.AvailBytes,
		Margin:     MarginFor(space.TotalBytes),
	}, nil
}

// spaceFor is the filesystem a backup written to dir would land on.
//
// The directory itself when it exists, and the directory that will hold
// it when it does not - which is the ordinary case on a machine that has
// never taken a backup, and not a reason to refuse.
//
// # Why the order matters, measured
//
// MeasureSecrets used to read parentOf(dir) unconditionally, and its own
// comment said it had "the same shape as Measure and for the same
// reason". It did not. The difference only shows when the backup
// directory is its own mount - which is not an exotic case, it is what
// KURULUM.md requires for Docker, because a backup written outside the
// volume is destroyed by the next image update.
//
// On such a machine the two read different filesystems entirely:
//
//	dir    /var/lib/crucible-analytic/yedek   32 MB total   margin 3.3 MB
//	parent /var/lib/crucible-analytic        252 GB total   margin 1.0 GB
//
// So the secrets estimate was answering "does this fit" about the
// container's root filesystem while the file went to the volume. The
// numbers are small enough that it has probably never refused wrongly;
// it was also reporting free space for the wrong disk to the page.
//
// One function, so the two cannot disagree again.
func spaceFor(dir string) (diskspace.Space, error) {
	space, err := diskspace.Read(dir)
	if err == nil {
		return space, nil
	}
	parent, parentErr := diskspace.Read(parentOf(dir))
	if parentErr != nil {
		return diskspace.Space{}, fmt.Errorf("backup: measuring %s: %w", dir, err)
	}
	return parent, nil
}

// parentOf is the directory holding dir, for the case where dir has not
// been created yet.
func parentOf(dir string) string {
	for i := len(dir) - 1; i > 0; i-- {
		if dir[i] == '/' {
			return dir[:i]
		}
	}
	return "/"
}
