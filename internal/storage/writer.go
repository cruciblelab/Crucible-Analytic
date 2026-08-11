package storage

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const tableName = "traffic_snapshots"

// columns must stay in sync with the field order WriteRows encodes in and
// with schema.sql's column list.
var columns = []string{
	"time", "site_id", "ip", "ja4", "prev_window_count", "curr_window_count",
	"request_rate", "bot_score", "is_known_bot_ja4",
	"country", "asn", "asn_org", "is_known_bot_asn",
}

// Writer persists Rows to TimescaleDB using COPY, which for the batch
// sizes this collector produces per flush (one row per active IP per
// interval, not per request) is dramatically faster than row-by-row
// INSERTs.
type Writer struct {
	pool *pgxpool.Pool
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
			r.Time, r.SiteID, r.IP, r.JA4, r.PrevWindowCount, r.CurrWindowCount,
			r.RequestRate, r.BotScore, r.IsKnownBotJA4,
			r.Country, r.ASN, r.ASNName, r.IsKnownBotASN,
		}, nil
	}))
	if err != nil {
		return n, fmt.Errorf("storage: copy rows: %w", err)
	}
	return n, nil
}

// Close releases the connection pool. Safe to call once.
func (w *Writer) Close() {
	w.pool.Close()
}
