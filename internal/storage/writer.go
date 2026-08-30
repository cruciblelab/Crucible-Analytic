package storage

import (
	"context"
	"fmt"
	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const tableName = "traffic_snapshots"

// columns must stay in sync with the field order WriteRows encodes in and
// with schema.sql's column list.
var columns = []string{
	"time", "site_id", "ip", "ip_hash", "ja4", "prev_window_count", "curr_window_count",
	"request_rate", "bot_score", "is_known_bot_ja4",
	"country", "asn", "asn_org", "is_known_bot_asn",
}

// Writer persists Rows to TimescaleDB using COPY, which for the batch
// sizes this collector produces per flush (one row per active IP per
// interval, not per request) is dramatically faster than row-by-row
// INSERTs.
type Writer struct {
	pool *pgxpool.Pool

	// Counters, for the health page.
	//
	// Added for B4, which asks the question no liveness check answers:
	// is the collector still writing? A process that is up and has
	// failed every write since Tuesday looks perfectly healthy to
	// anything that only asks whether it is running.
	//
	// Rows rather than batches, because a batch is an implementation
	// detail of the flush interval and a row is what a customer's
	// numbers are made of.
	written atomic.Uint64
	failed  atomic.Uint64
}

// NewWriter opens a connection pool to databaseURL and verifies it's
// reachable. It does not create or migrate the schema - apply schema.sql
// once, separately, before running the collector.
func NewWriter(ctx context.Context, databaseURL string) (*Writer, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("storage: create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("storage: ping database: %w", err)
	}
	// Ping proved the database answers. It proved nothing about the
	// shape of this table, and the difference costs rows: measured, a
	// collector missing one column starts, reports healthy, and drops
	// every batch it is handed. See internal/schemaver.
	if err := schemaver.RequireColumns(ctx, pool, tableName, columns); err != nil {
		pool.Close()
		return nil, fmt.Errorf("storage: %w", err)
	}
	return &Writer{pool: pool}, nil
}

// WriteRows batch-inserts rows via COPY and returns how many were written.
func (w *Writer) WriteRows(ctx context.Context, rows []Row) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}

	n, err := w.pool.CopyFrom(ctx, pgx.Identifier{tableName}, columns, pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
		r := rows[i]
		return []any{
			r.Time, r.SiteID, r.storedIP(), r.storedIPHash(), r.JA4, r.PrevWindowCount, r.CurrWindowCount,
			r.RequestRate, r.BotScore, r.IsKnownBotJA4,
			r.Country, r.ASN, r.ASNName, r.IsKnownBotASN,
		}, nil
	}))
	if err != nil {
		w.failed.Add(uint64(len(rows))) // len is never negative
		return n, fmt.Errorf("storage: copy rows: %w", err)
	}
	// Guarded rather than asserted. n is the row count CopyFrom returns
	// on success, so the error path above has already returned and it is
	// never negative - and a comparison is cheaper than a comment
	// promising that stays true.
	if n > 0 {
		w.written.Add(uint64(n))
	}
	return n, nil
}

// Counters reports rows written and rows lost to a failed write, since
// the process started.
func (w *Writer) Counters() (written, failed uint64) {
	return w.written.Load(), w.failed.Load()
}

// Close releases the connection pool. Safe to call once.
// Pool exposes the connection pool for work that is not row writing -
// today, installing this table's retention policy. Deliberately narrow:
// the caller gets the pool, not a second writer.
func (w *Writer) Pool() *pgxpool.Pool { return w.pool }

func (w *Writer) Close() {
	w.pool.Close()
}
