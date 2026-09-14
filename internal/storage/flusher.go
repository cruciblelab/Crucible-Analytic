package storage

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/privacy"
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
	Store ratestore.RateStore
	// SiteID identifies which site every row this flusher writes belongs
	// to - see config.SiteID and Row.SiteID.
	SiteID    string
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
	//
	// Settable at construction and replaceable afterwards with
	// SetKnownBotASNs; the field is read once per flush, through the
	// atomic below, so a list arriving mid-flush applies to the next one
	// rather than to half of this one.
	KnownBotASNs map[int]struct{}

	// liveASNs holds the replacement, if SetKnownBotASNs was ever
	// called. Kept beside the field rather than replacing it so a caller
	// that only builds a Flusher and never changes it - every test, and
	// the collector before A5.2 - reads exactly as it did.
	liveASNs atomic.Pointer[map[int]struct{}]
	// IPMode decides how much of each address is written. The zero value
	// masks - see storage.RowOptions.
	IPMode privacy.IPMode
	// IPHashKey keys the token stored in full mode.
	IPHashKey []byte
}

// SetKnownBotASNs replaces the scoring signal while the collector runs.
//
// The one list in this system that answers "that network is hammering
// us, mark it" without an SSH session. A nil or empty map turns the
// signal off, which is what apply_to_scoring = false means: scoring.Score
// never matches against an empty map, so nothing else has to know.
//
// The map is not copied. Callers build a fresh one and hand it over
// rather than mutating one already published - a map being written while
// BuildRows reads it is a data race no atomic pointer can fix.
func (f *Flusher) SetKnownBotASNs(asns map[int]struct{}) {
	f.liveASNs.Store(&asns)
}

// knownBotASNs is the list in force: whatever SetKnownBotASNs last
// published, or the field it was built with.
func (f *Flusher) knownBotASNs() map[int]struct{} {
	if live := f.liveASNs.Load(); live != nil {
		return *live
	}
	return f.KnownBotASNs
}

// flushTimeout bounds a write that has been detached from cancellation.
//
// Five seconds: the value Run's shutdown branch used before this rule
// moved into flushOnce, kept so a clean stop waits no longer than it
// used to. Detached is not unbounded - a database that has stopped
// answering must not hold shutdown open past what systemd allows.
const flushTimeout = 5 * time.Second

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
			// No fresh context built here: flushOnce detaches from
			// cancellation itself, for both call sites. The ticker path
			// needed the same thing and did not have it - see the
			// comment in flushOnce.
			f.flushOnce(ctx, lastFlush, time.Now())
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

	rows := BuildRows(snapshots, now, RowOptions{
		SiteID:       f.SiteID,
		KnownBots:    f.KnownBots,
		KnownBotASNs: f.knownBotASNs(),
		Resolver:     f.Resolver,
		IPMode:       f.IPMode,
		IPHashKey:    f.IPHashKey,
	})
	// A write that has started finishes.
	//
	// Cancellation stops new work; it must not abort a write already
	// going out. These rows came from Snapshot(since, now) and Run
	// advances lastFlush whether this succeeded or not, so a write
	// killed here is a window nothing retries: the requests in it are
	// gone from the collector's history.
	//
	// The same defect as the beacon's, found by measuring the
	// neighbour after fixing that one. There the shutdown branch built
	// a fresh context and the ticker branch did not; here it was
	// identical, line for line.
	//
	// Bounded, because detached is not unbounded: five seconds, the
	// value the shutdown branch already used, so a clean stop waits no
	// longer than it used to.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
	defer cancel()

	n, err := f.Writer.WriteRows(writeCtx, rows)
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
