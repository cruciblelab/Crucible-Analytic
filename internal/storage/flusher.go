package storage

import (
	"context"
	"log/slog"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// RowWriter persists rows to durable storage. *Writer implements this;
// tests substitute a fake to exercise Flusher without a live database.
type RowWriter interface {
	WriteRows(ctx context.Context, rows []Row) (int64, error)
}

// Flusher periodically snapshots a RateStore, scores each active IP,
// optionally enriches it with country/ASN, and writes the resulting rows
// via Writer.
type Flusher struct {
	Store     ratestore.RateStore
	Writer    RowWriter
	KnownBots map[string]string
	Interval  time.Duration
	Logger    *slog.Logger
	// Resolver, if set, enriches each row with country/ASN. Left nil when
	// asn_lookup.enabled = false - see BuildRows.
	Resolver GeoResolver
	// KnownBotASNs feeds scoring.Score's ASN component. Left nil when
	// asn_lookup.apply_to_scoring = false (the default) - see BuildRows
	// and scoring.Score for why nil alone is enough to make it a no-op.
	KnownBotASNs map[int]struct{}
}

// Run flushes every f.Interval until ctx is cancelled, then performs one
// best-effort final flush (bounded to 5s) so the last partial interval's
// activity isn't silently dropped on shutdown.
func (f *Flusher) Run(ctx context.Context) {
	ticker := time.NewTicker(f.Interval)
	defer ticker.Stop()

	// Zero value, not time.Now(): seeding with "now" would make the very
	// first flush's since-filter exclude anything RecordRequest happened
	// to record in the brief window between process startup and this
	// call - a real, if narrow, race between the proxy and flusher
	// goroutines starting up. Starting from the zero time means the first
	// flush always captures everything tracked so far.
	var lastFlush time.Time
	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			f.flushOnce(shutdownCtx, lastFlush, time.Now())
			cancel()
			return
		case <-ticker.C:
			now := time.Now()
			f.flushOnce(ctx, lastFlush, now)
			lastFlush = now
		}
	}
}

func (f *Flusher) flushOnce(ctx context.Context, since, now time.Time) {
	snapshots := f.Store.Snapshot(since, now)
	if len(snapshots) == 0 {
		return
	}

	rows := BuildRows(snapshots, f.KnownBots, now, f.Resolver, f.KnownBotASNs)
	n, err := f.Writer.WriteRows(ctx, rows)
	if err != nil {
		f.logger().Error("flush failed", "err", err, "attempted_rows", len(rows))
		return
	}
	f.logger().Info("flush complete", "rows", n)
}

func (f *Flusher) logger() *slog.Logger {
	if f.Logger != nil {
		return f.Logger
	}
	return slog.Default()
}
