package retention

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
)

// Compression: why it lives beside retention rather than in a package of
// its own.
//
// Both are policies on the age of a chunk, on the same two hypertables,
// applied by the same two services at the same moment in their startup.
// And they have to agree about one thing: a chunk compressed after the
// day it is dropped is a chunk that is never compressed. Splitting them
// into two packages would put that agreement in neither.
//
// # What it is for, measured
//
// 2026-09-09, against a real TimescaleDB with 12 million rows over 90
// days - a site with about fourteen active addresses in any ten-second
// flush window, which is small:
//
//	aralık      özet      zaman serisi
//	 7 gün    1,50 sn        0,98 sn
//	30 gün    7,97 sn        4,48 sn
//	90 gün   26,96 sn       19,55 sn
//
// The panel's client gives one call five seconds and a whole page eight.
// So two of the four buttons in the range picker did not work, and the
// page blamed the data source for numbers that were sitting right there.
//
// EXPLAIN said why, and it was not "the data is large". The hypertable
// orders rows by time; every dashboard query asks by site. Reading
// 20.748 rows for one site touched 13.632 heap pages - about one page
// per row - because that site's rows are interleaved with every other
// site's. A single summary read 3,1 GB.
//
// Compressing with site_id as the segment stores each site's rows
// together. Measured on the same data: disk 3269 MB to 792 MB, the
// 90-day summary 21,9 sn to 11,6 sn.
//
// # And it is not enough, which is written here rather than discovered
//
// Halving is not fixing: 30 days still sits on the timeout. The query
// still touches every row in the range, and no physical layout changes
// that. The other half is a precomputed rollup, planned as O2, and this
// package does not pretend to be it.
//
// # Who does the compressing
//
// This package, on the loop that already applies retention - not
// TimescaleDB's background policy. add_compression_policy is a superuser
// call, and nothing in this product runs as a superuser after
// release/sql/grants.sql has run. The full reasoning, with the measured
// privilege table, is at ca_set_compression in schema.sql.
//
// The practical consequence for a reader here: compression happens when
// the service's retention interval comes round, not on TimescaleDB's own
// schedule, and a chunk becomes compressed at most one interval after it
// becomes old enough.

// LogApply applies compression and says what happened, once, for the two
// services that both do exactly this.
//
// Here rather than copied into each main, because the interesting part
// is which outcomes are worth which level - and a second copy of that
// judgement is a second chance for one of them to start treating an
// unavailable feature as a fault. A database that cannot compress is
// news, not an error.
//
// Takes the values rather than a config type: internal/collector's
// config imports this package, so this package cannot import it back.
func (m *Manager) LogApply(ctx context.Context, logger *slog.Logger, afterDays, retentionDays int) {
	report, err := m.ApplyCompression(ctx, afterDays, retentionDays)
	switch {
	case errors.Is(err, ErrCompressionUnavailable):
		logger.Info("compression: not available on this database, so long ranges in the "+
			"panel will stay slow", "table", string(m.table), "err", err)
	case err != nil:
		logger.Warn("compression: could not apply", "table", string(m.table), "err", err)
	case report.Skipped != "":
		logger.Info("compression: nothing done", "table", string(report.Table),
			"why", report.Skipped)
	case report.Gained > 0:
		logger.Info("compression: chunks compressed", "table", string(report.Table),
			"newly_compressed", report.Gained, "compressed", report.Compressed,
			"chunks", report.Chunks, "after_days", report.AfterDays)
	}
	// A pass that compressed nothing new says nothing, which is what
	// almost every pass after the first is. A log line per hour per table
	// reporting that there was nothing to do is a log nobody reads, in
	// the file that matters on the one day something goes wrong.
}

// DefaultCompressAfterDays is how old a chunk must be before it is
// compressed.
//
// A week, and the number is chosen by what compression costs rather than
// by what it saves. A compressed chunk can still be written to, but the
// row goes to a slower path; recent chunks are where the collector is
// writing every ten seconds. Seven days leaves the write path alone and
// still compresses everything the dashboard's long ranges have to read.
const DefaultCompressAfterDays = 7

// CompressionWanted reads a configured value, and the retention it has
// to fit inside, into a decision.
//
// Here rather than in either config package, because both services have
// their own RetentionConfig and a rule written twice is a rule that will
// eventually mean two things.
//
// A negative number is the way to say no. Zero cannot be, because zero
// is what a file that never heard of the setting contains - and a
// feature that measurably fixes a broken dashboard should not be off for
// everybody who upgraded without reading the changelog.
//
// # The default gives way; an explicit value does not
//
// retentionDays can be shorter than the default compression age, and a
// deployment keeping one day of data is a perfectly legal one. The first
// version of this refused such a file outright, which meant an upgrade
// turned a running deployment's config into an invalid one over a
// feature nobody had asked for. Caught by a test that already existed.
//
// So a default that does not fit simply switches compression off. Only a
// value the operator wrote themselves is worth refusing the file for,
// because only then did somebody state two things that cannot both be
// true. Explicit is the caller's business: it passes configured through
// and reads wanted.
func CompressionWanted(configured, retentionDays int) (days int, wanted bool) {
	switch {
	case configured < 0:
		return 0, false
	case configured == 0:
		if retentionDays > 0 && DefaultCompressAfterDays >= retentionDays {
			return 0, false
		}
		return DefaultCompressAfterDays, true
	default:
		return configured, true
	}
}

// ErrCompressionUnavailable reports a deployment whose TimescaleDB
// cannot compress.
//
// Compression is a Timescale-License feature and the Apache-licensed
// build does not have it. That is a legitimate deployment, not a broken
// one: everything else in this product works there, and the only cost is
// that long ranges stay slow. So it is an error the caller is expected
// to log and carry on from, never one that stops a service.
var ErrCompressionUnavailable = errors.New("retention: this TimescaleDB cannot compress")

// CompressionReport is what one Apply did or refused to do.
type CompressionReport struct {
	Table Table
	// AfterDays is the age at which chunks are compressed.
	AfterDays int
	// Compressed and Chunks are how many of the table's chunks are
	// compressed after this pass, and how many there are.
	Compressed int
	Chunks     int
	// Gained is how many this pass compressed.
	Gained int
	// Skipped explains why nothing was done, when nothing was. Empty on
	// an ordinary apply.
	Skipped string
}

// ChunkCompression counts the table's chunks and how many of them are
// compressed.
//
// A state rather than a delta, and that is deliberate: the number worth
// putting in a log line is "11 of 14", which answers both "is this
// working" and "how far has it got". A count of what one pass did
// answers neither on the pass that finds everything already done.
//
// A database that cannot compress reports every chunk uncompressed
// rather than an error: asking it how much it has compressed is a
// question with a plain answer.
func (m *Manager) ChunkCompression(ctx context.Context) (compressed, total int, err error) {
	err = m.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE is_compressed), count(*)
		  FROM timescaledb_information.chunks
		 WHERE hypertable_schema = 'public' AND hypertable_name = $1`,
		string(m.table)).Scan(&compressed, &total)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("retention: counting compressed chunks for %s: %w", m.table, err)
	}
	return compressed, total, nil
}

// ApplyCompression turns on site-segmented compression and schedules it.
//
// afterDays must be inside the same bounds retention uses, and must be
// shorter than the retention the deployment keeps - a chunk compressed
// after the day it is dropped is never compressed at all. That second
// check is here rather than in the database because it needs both
// numbers, and because getting it wrong wastes a feature rather than
// losing data: the wrapper's own bounds are what protect the database.
func (m *Manager) ApplyCompression(ctx context.Context, afterDays, retentionDays int) (CompressionReport, error) {
	if !m.table.Valid() {
		return CompressionReport{}, fmt.Errorf("retention: unknown table %q", m.table)
	}
	report := CompressionReport{Table: m.table, AfterDays: afterDays}

	if afterDays < MinDays || afterDays > MaxDays {
		return report, fmt.Errorf("retention: compress after %d days is outside %d..%d",
			afterDays, MinDays, MaxDays)
	}
	if retentionDays > 0 && afterDays >= retentionDays {
		report.Skipped = fmt.Sprintf(
			"compressing after %d days on a table kept %d days would never compress anything",
			afterDays, retentionDays)
		return report, nil
	}

	before, _, err := m.ChunkCompression(ctx)
	if err != nil {
		return report, err
	}

	var status string
	if err := m.pool.QueryRow(ctx,
		`SELECT ca_set_compression($1, $2)`, string(m.table), afterDays).Scan(&status); err != nil {
		// Everything this database cannot do arrives here as one of a
		// small set of failures, and none of them is worth stopping a
		// service for. Named rather than swallowed, so the caller can
		// say which it was.
		return report, fmt.Errorf("%w: %s: %v", ErrCompressionUnavailable, m.table, err)
	}
	if status != "ok" {
		// The table already carries different settings. Changing them
		// means decompressing every chunk, which is not a thing a
		// service decides to do to a customer's history at startup.
		report.Skipped = status
		return report, nil
	}

	after, total, err := m.ChunkCompression(ctx)
	if err != nil {
		return report, err
	}
	report.Compressed, report.Chunks, report.Gained = after, total, after-before
	return report, nil
}
