package panel

import (
	"context"
	"fmt"
	"time"
)

// The panel's side of the fetch log.
//
// # Why the panel can only read it
//
// The rows are written by whoever fetches - the collector, the beacon,
// or both - and grants.sql gives panel_user SELECT and nothing else. It
// is the same split panel_upgrade_requests makes: a record whose reader
// can also write it is a record that can be made to say the fetch
// succeeded.
//
// So there is no Record method here and there will not be one. If this
// package ever needs to write a fetch row, the thing that actually
// changed is which process fetches, and that is a design decision rather
// than a missing function.
//
// # Why the panel reads it at all
//
// "Is my geography data current, and if not, why not" is a question the
// customer asks and the journal answers - on a machine they have no
// shell on. A stale dataset has no symptom: the range tables keep last
// month's answers and every page draws normally.

// RangeFetch is one recorded attempt at one dataset file.
//
// Fields are what the row holds rather than what a page wants, because
// a store that shaped its rows for today's page is a store the next page
// has to work around.
type RangeFetch struct {
	ID         int64
	StartedAt  time.Time
	FinishedAt time.Time
	// SourceID is internal/ipsources' id, e.g. "user-country".
	SourceID string
	// Kind is "country" or "asn"; Family is "ipv4" or "ipv6".
	Kind, Family string
	// Origin is "download" or "mirror". Without it a byte count of zero
	// from a local directory reads as a failed download.
	Origin string
	// Outcome is "succeeded" or "failed".
	Outcome    string
	RowsParsed int64
	BytesRead  int64
	ErrorChain string
}

// Failed is whether this attempt failed, for a template that should not
// be comparing strings.
func (f RangeFetch) Failed() bool { return f.Outcome != "succeeded" }

// Took is how long the attempt ran.
func (f RangeFetch) Took() time.Duration { return f.FinishedAt.Sub(f.StartedAt) }

// maxRangeFetches bounds what a caller can ask for.
//
// A page shows the last handful; the cap is what stops a caller from
// turning a diagnostic read into a table scan the panel then has to
// render. 500 is far more than any page wants and far less than the
// ninety days the writers keep.
const maxRangeFetches = 500

// RecentRangeFetches returns the most recent attempts, newest first.
//
// Returns an empty slice and no error when the table does not exist,
// which is the ordinary state of a deployment with asn_lookup disabled -
// the schema is applied but nothing has ever fetched. A missing table
// there is not a fault to report, and treating it as one would put a red
// section on the health page of a correctly configured installation.
func (s *Store) RecentRangeFetches(ctx context.Context, limit int) ([]RangeFetch, error) {
	if limit <= 0 || limit > maxRangeFetches {
		limit = maxRangeFetches
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, started_at, finished_at, source_id, kind, family, origin,
		       outcome, rows_parsed, bytes_read, error_chain
		FROM ip_range_fetches
		ORDER BY started_at DESC, id DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("panel: read range fetches: %w", err)
	}
	defer rows.Close()

	out := []RangeFetch{}
	for rows.Next() {
		var f RangeFetch
		if err := rows.Scan(&f.ID, &f.StartedAt, &f.FinishedAt, &f.SourceID,
			&f.Kind, &f.Family, &f.Origin, &f.Outcome, &f.RowsParsed,
			&f.BytesRead, &f.ErrorChain); err != nil {
			return nil, fmt.Errorf("panel: scan range fetch: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("panel: read range fetches: %w", err)
	}
	return out, nil
}

// LastRangeFetchPerFile is the newest attempt for each dataset file.
//
// The list a person actually wants. A plain "last N" is dominated by
// whichever dataset refreshed most recently, so the one that has been
// failing for a month scrolls off the bottom - which is precisely the
// row somebody came to look for.
//
// Keyed on the three columns that identify a file rather than on
// source_id alone: a source that fetches IPv4 and IPv6 has two files
// that fail independently, and collapsing them would hide exactly the
// half-broken state the per-file rows exist to express.
func (s *Store) LastRangeFetchPerFile(ctx context.Context) ([]RangeFetch, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT ON (source_id, kind, family)
		       id, started_at, finished_at, source_id, kind, family, origin,
		       outcome, rows_parsed, bytes_read, error_chain
		FROM ip_range_fetches
		ORDER BY source_id, kind, family, started_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("panel: read last range fetches: %w", err)
	}
	defer rows.Close()

	out := []RangeFetch{}
	for rows.Next() {
		var f RangeFetch
		if err := rows.Scan(&f.ID, &f.StartedAt, &f.FinishedAt, &f.SourceID,
			&f.Kind, &f.Family, &f.Origin, &f.Outcome, &f.RowsParsed,
			&f.BytesRead, &f.ErrorChain); err != nil {
			return nil, fmt.Errorf("panel: scan range fetch: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("panel: read last range fetches: %w", err)
	}
	return out, nil
}
