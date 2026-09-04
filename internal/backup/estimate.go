package backup

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/diskspace"
)

// Will it fit.
//
// # Why this is a measurement and not a guess
//
// A backup that fills the disk stops the collector, and the collector is
// in front of the customer's website. So the one failure this feature
// can cause on its own is an outage, produced by the feature working
// exactly as asked.
//
// The estimate therefore has to be made of something real. Two numbers
// were measured on a live database with real traffic in it:
//
//	traffic_snapshots   16.4 MB on disk   68 KB compressed   0.4%
//	beacon_events       18.1 MB on disk   97 KB compressed   0.5%
//
// Half a per cent, and that is not a number to divide by. Compression
// ratios depend on what the rows contain: a table of near-identical
// synthetic rows compresses far better than a real one, and this test
// data is more repetitive than a customer's traffic. Taking 0.5% as the
// rule would build a check that passes on every machine and protects
// none of them.
//
// So the estimate is deliberately pessimistic: it assumes the file will
// be a *fifth* of what the tables occupy, which is forty times worse
// than measured. A wrong estimate in this direction refuses a backup
// that would have fitted, and the customer sees a number and a reason.
// Wrong in the other direction it fills the disk.
//
// *Bir tahminin ölçülmüş olması, ölçülen şeyin temsil ettiği anlamına
// gelmez.*

// CompressedShare is the fraction of a table's on-disk size the estimate
// assumes the backup will take.
//
// One fifth. Measured at one two-hundredth on test data; see above for
// why the measurement is not the number.
const CompressedShare = 5

// FreeMargin is what must still be free after the backup is written.
//
// A gigabyte, or a tenth of the filesystem, whichever is smaller. Not
// zero: the database keeps writing while the backup is taken, and a
// machine left with nothing spare is one where the next WAL segment is
// the outage instead.
const FreeMargin = 1 << 30

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

	space, err := diskspace.Read(dir)
	if err != nil {
		// The directory not existing is the ordinary case on a machine
		// that has never taken a backup, and it is not a reason to
		// refuse: what matters is the filesystem it will be created on.
		parent, parentErr := diskspace.Read(parentOf(dir))
		if parentErr != nil {
			return Estimate{}, fmt.Errorf("backup: measuring %s: %w", dir, err)
		}
		space = parent
	}

	margin := int64(FreeMargin)
	if tenth := space.TotalBytes / 10; tenth < margin {
		margin = tenth
	}

	return Estimate{
		TableBytes: tableBytes,
		FileBytes:  tableBytes / CompressedShare,
		AvailBytes: space.AvailBytes,
		Margin:     margin,
	}, nil
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
