package asnlookup

import (
	"context"
	"fmt"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/ipsources"
)

// The fetch log: what this deployment tried to download, and how it went.
//
// # Why it exists
//
// The refresh already logged its failures, to the service's journal.
// That is the one place a customer with no shell cannot look, and the
// failure it hides is quiet in the way this project keeps finding: the
// range tables keep last month's data, every page draws normally, and
// "my geography numbers look stale" has no answer anywhere a person can
// reach.
//
// So the same facts go into a table the panel can read: which dataset,
// which address family, how long it took, how many rows, how many bytes,
// and on a failure the whole error chain.
//
// # Failing to record must not fail the refresh
//
// Every write here is best-effort and logged rather than returned. The
// record is a diagnostic; the ranges are the product. A deployment whose
// fetch log is unwritable - the table not applied yet, a missing grant,
// a full disk - must still resolve addresses, and the alternative
// ordering would mean the diagnostic could take down the thing it is
// diagnosing.

// fetchRetention is how long a row is kept.
//
// Ninety days, and longer than the panel's own 30-day operation
// retention on purpose. The question this table answers is "when did
// this last work", and for a dataset that refreshes weekly by default
// a month of history is barely a handful of attempts. Ninety days is
// three months of weekly refreshes: enough to see that something broke
// in the spring.
//
// A constant rather than a setting, for the reason housekeeping.go gives
// about the panel's own diagnostic tables: nobody has a reason to want a
// year of this, and every setting is a surface.
const fetchRetention = 90 * 24 * time.Hour

// fetchOutcome values. Two, and no third for "partly": a file either
// parsed or it did not, and the per-family rows are what express
// "IPv6 is current and IPv4 is not".
const (
	fetchSucceeded = "succeeded"
	fetchFailed    = "failed"
)

// Where the bytes came from. Recorded because otherwise a byte count of
// zero from a mirror directory reads as a failed download rather than as
// asn_lookup.local_csv_path doing what it was set to do.
const (
	originDownload = "download"
	originMirror   = "mirror"
)

// Address families, as the rows spell them.
const (
	familyIPv4 = "ipv4"
	familyIPv6 = "ipv6"
)

// kindName is what a SourceKind is called in a row.
//
// A switch rather than a Stringer on ipsources.SourceKind: these strings
// are stored in a database and read back a year later, so they are this
// package's business to keep stable. A String() method is the kind of
// thing somebody improves for a log line, and the row would change
// meaning without anybody noticing.
func kindName(k ipsources.SourceKind) string {
	switch k {
	case ipsources.KindCountry:
		return "country"
	case ipsources.KindASN:
		return "asn"
	default:
		// Not "unknown": a kind this build does not name is worth seeing
		// in the row rather than being flattened into a word that
		// describes several different situations.
		return fmt.Sprintf("kind%d", int(k))
	}
}

// fetchRecord is one attempt at one file.
type fetchRecord struct {
	started  time.Time
	finished time.Time
	sourceID string
	kind     string
	family   string
	origin   string
	outcome  string
	rows     int64
	bytes    int64
	errChain string
}

// origin says which transport this resolver is using.
func (r *Resolver) origin() string {
	if r.localCSVPath != "" {
		return originMirror
	}
	return originDownload
}

// newFetchRecord builds a row from what one load returned.
func (r *Resolver) newFetchRecord(src ipsources.Source, family string,
	started, finished time.Time, rows int, bytes int64, err error) fetchRecord {

	rec := fetchRecord{
		started:  started,
		finished: finished,
		sourceID: src.ID,
		kind:     kindName(src.Kind),
		family:   family,
		origin:   r.origin(),
		outcome:  fetchSucceeded,
		rows:     int64(rows),
		bytes:    bytes,
	}
	if err != nil {
		rec.outcome = fetchFailed
		rec.errChain = err.Error()
		// Rows are whatever the parser managed before it stopped, which
		// on a truncated file is most of them. Left as measured rather
		// than zeroed: "failed after 9,201 rows" and "failed after 0"
		// are different problems and only the number separates them.
	}
	return rec
}

// recordFetch writes one row, best-effort.
func (r *Resolver) recordFetch(ctx context.Context, rec fetchRecord) {
	if r.pool == nil || r.SkipRangePersistence {
		return
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO ip_range_fetches
		  (started_at, finished_at, source_id, kind, family, origin,
		   outcome, rows_parsed, bytes_read, error_chain)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		rec.started, rec.finished, rec.sourceID, rec.kind, rec.family, rec.origin,
		rec.outcome, rec.rows, rec.bytes, rec.errChain)
	if err != nil {
		// One line, and it does not stop anything. See the package note
		// above on why the diagnostic must not be able to break the
		// thing it diagnoses.
		r.logger().Warn("asnlookup: could not record the fetch",
			"source", rec.sourceID, "family", rec.family, "err", err)
	}
}

// PurgeOldFetches trims ip_range_fetches and returns how many rows went.
//
// # Why here and not in the panel's housekeeping
//
// PLAN.md's M2 says both "the collector writes, the panel reads" and
// "its retention hangs off internal/panel/housekeeping.go", and the two
// cannot both hold: a sweep needs DELETE, and DELETE on a table it does
// not write is a privilege the panel has no other use for.
//
// The writer sweeps instead. It is the component with a ticker already
// running, the table only grows while it is running, and the row that
// outlives a decommissioned collector is a handful of rows per week
// rather than an unbounded table. The alternative - a SECURITY DEFINER
// function so the panel can delete without the privilege - is real
// surface for a benefit measured in bytes.
//
// What the plan was actually protecting against is a sweep with no
// caller, and that is guarded here too: TestTheSweepIsCalledByTheRefresh
// reads Run's source and fails if the call disappears.
func (r *Resolver) PurgeOldFetches(ctx context.Context) (int64, error) {
	if r.pool == nil {
		return 0, nil
	}
	tag, err := r.pool.Exec(ctx,
		// Bound passed as a parameter rather than pasted in, the same
		// rule internal/panel's purges follow: the value is a constant
		// today, and this is what stops the next edit from being the one
		// that makes it a string.
		`DELETE FROM ip_range_fetches WHERE started_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int64(fetchRetention/time.Second)))
	if err != nil {
		return 0, fmt.Errorf("asnlookup: purge fetch log: %w", err)
	}
	return tag.RowsAffected(), nil
}
