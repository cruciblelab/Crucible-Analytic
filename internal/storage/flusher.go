package storage

import (
	"context"
	"log/slog"
	"sync"
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
	//
	// Settable at construction and replaceable afterwards with SetIPMode,
	// through the atomic below, for the same reason KnownBotASNs is: the
	// field is read once per flush, so a mode arriving mid-flush applies
	// to the next batch rather than to half of this one. A batch written
	// half in each mode would be two key spaces inside one interval.
	IPMode privacy.IPMode
	// liveIPMode holds the replacement, if SetIPMode was ever called.
	liveIPMode atomic.Pointer[privacy.IPMode]
	// IPHashKey keys the token stored in full mode.
	IPHashKey []byte

	// Pause, when set, is this site's recording pause (PLAN §4, #3).
	// Nil is never paused, which is every test and every build before
	// the pause existed.
	Pause *Pause
}

// Pause is one site's recording pause: the span it covers, and how much it
// has held back.
//
// Its own value rather than fields on Flusher, because two parts of the
// collector need it and they start in the wrong order for sharing a
// Flusher: the heartbeat, which reports what was held back, starts
// before the flusher exists. Built first and handed to both, neither has
// to exist for the other to start.
//
// Applied at the write, not at the proxy. The site goes on being served
// and the rate windows go on being kept - they are what scoring and the
// limits read - and only the rows stop.
//
// # A span, not an end
//
// A flush writes one row per address seen since the last one, and a row
// belongs to the moment of the address's latest request - that is all the
// rate store keeps. So a pause is the span [From, Until), and a flush
// holds back the addresses last seen inside it, whenever the flush runs.
//
// The first version kept only the end and asked whether the flush itself
// ran paused. That moved both edges to the flush ticks: the flush after
// the end wrote every address last seen in the pause's final interval -
// visits made while paused, recorded (up to flush_interval_seconds of
// them, 10 by default) - and the first paused flush dropped the visits
// made before the pause in the interval it began in.
type Pause struct {
	mu          sync.Mutex
	from, until time.Time
	held        atomic.Uint64
}

// SetUntil applies the pause setting as read at now: paused until end, or
// recording if end is not after now (the zero time included).
//
// The span begins when a pause is first applied, not when it was pressed:
// this process cannot know a moment it had not heard of, and a visit it
// served before hearing was served while recording. Lifting the pause
// ends the span at now; an end that simply passes ends it there.
func (p *Pause) SetUntil(end, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	inForce := now.Before(p.until)
	switch {
	case end.After(now) && inForce:
		p.until = end // moved, the same pause
	case end.After(now):
		p.from, p.until = now, end
	case inForce:
		p.until = now // lifted
	}
	// Otherwise nothing was in force and nothing is: the last span stays
	// as it ended, so a flush that has not yet passed it still holds it.
}

// Holds reports whether a row last seen at seen belongs to the pause.
func (p *Pause) Holds(seen time.Time) bool {
	if p == nil {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return !seen.Before(p.from) && seen.Before(p.until)
}

// Held is how many rows the pause has kept out of the table since start.
func (p *Pause) Held() uint64 {
	if p == nil {
		return 0
	}
	return p.held.Load()
}

// SetIPMode swaps how much of each address is written, while the
// collector runs.
//
// The beacon's server has had this since A6 and this did not exist, so
// privacy.ip_storage reached one writer of the crossover join and not
// the other - see collector.PrivacyConfig.Live for what that cost.
//
// No validation here: the caller resolves the mode through Live, which
// admits only the two declared values, and privacy.ParseIPMode maps
// anything else to masked. A setter that re-checked would be a second
// place for that decision to live.
func (f *Flusher) SetIPMode(mode privacy.IPMode) { f.liveIPMode.Store(&mode) }

// ipMode is the mode in force: whatever SetIPMode last published, or the
// field the Flusher was built with.
func (f *Flusher) ipMode() privacy.IPMode {
	if live := f.liveIPMode.Load(); live != nil {
		return *live
	}
	return f.IPMode
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
	// What was last seen while paused is not recorded, now or later - see
	// Pause for why the address's latest request decides.
	if f.Pause != nil {
		kept := snapshots[:0]
		for _, snap := range snapshots {
			if !f.Pause.Holds(snap.LastSeen) {
				kept = append(kept, snap)
			}
		}
		if held := len(snapshots) - len(kept); held > 0 {
			f.Pause.held.Add(uint64(held))
			f.logger().Debug("rows held: recording is paused", "rows", held)
		}
		if snapshots = kept; len(snapshots) == 0 {
			return
		}
	}

	rows := BuildRows(snapshots, now, RowOptions{
		SiteID:       f.SiteID,
		KnownBots:    f.KnownBots,
		KnownBotASNs: f.knownBotASNs(),
		Resolver:     f.Resolver,
		IPMode:       f.ipMode(),
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
