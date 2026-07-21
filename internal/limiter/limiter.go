// Package limiter bounds the collector's own resource usage - total
// concurrent connections/requests and total requests/second, across all
// IPs combined - independent of anything in ratestore, which tracks
// per-IP behavior for scoring, not the collector's own load. Without
// this, the collector itself has no upper bound on concurrency and
// becomes a resource-exhaustion target: whatever is DDoSing the backend
// DDoSes the collector too.
package limiter

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Policy selects what happens when a configured limit is exceeded.
type Policy string

const (
	// PolicyFailOpen skips fingerprinting/recording for the excess
	// connection/request, but keeps forwarding it to the backend
	// normally - the collector is never, by default, the reason a site
	// goes down. This is the safe default; the other two policies are
	// opt-in.
	PolicyFailOpen Policy = "fail_open"
	// PolicyFailClosed rejects the excess connection/request outright.
	PolicyFailClosed Policy = "fail_closed"
	// PolicyThrottle queues the excess connection/request (bounded by
	// Config.ThrottleQueueSize) until capacity frees up, falling back to
	// fail-closed behavior if the queue itself is full.
	PolicyThrottle Policy = "throttle"
)

// Decision is what a caller should do with one connection/request, based
// on current load and the configured Policy.
type Decision int

const (
	// DecisionProceed: handle normally (fingerprint, record, forward).
	DecisionProceed Decision = iota
	// DecisionDegrade: forward to the backend, but skip fingerprinting/
	// recording (PolicyFailOpen, over limit) to minimize the collector's
	// own overhead while under load it can't otherwise absorb.
	DecisionDegrade
	// DecisionReject: refuse the connection/request entirely
	// (PolicyFailClosed over limit, or PolicyThrottle's queue also full).
	DecisionReject
)

// throttlePollInterval is how often a queued PolicyThrottle caller
// re-checks whether capacity has freed up. A short poll rather than a
// wake-on-release notification: with ThrottleQueueSize bounding how many
// callers can ever be polling at once, the aggregate cost is negligible
// (a few atomic reads per waiter per tick), and it avoids the extra
// bookkeeping a proper wait-queue/broadcast mechanism would need for a
// resource-protection feature that's meant to stay simple.
const throttlePollInterval = 20 * time.Millisecond

// Config configures a Limiter. Zero or negative MaxConcurrentConnections
// or MaxRequestsPerSecond means "no limit" for that dimension.
type Config struct {
	MaxConcurrentConnections int
	MaxRequestsPerSecond     int
	Policy                   Policy
	// ThrottleQueueSize bounds how many callers can be waiting at once
	// under PolicyThrottle. Ignored by the other policies. A caller that
	// would exceed it is rejected immediately rather than joining the
	// queue, so the queue itself can never grow unbounded.
	ThrottleQueueSize int
}

// Limiter is safe for concurrent use.
type Limiter struct {
	cfg Config

	current atomic.Int64
	waiting atomic.Int64
	rate    *globalRateCounter
}

// New creates a Limiter from cfg.
func New(cfg Config) *Limiter {
	return newLimiter(cfg, time.Second)
}

// newLimiter is New with an overridable rate window, so tests can use a
// short window instead of waiting on real 1-second boundaries. The public
// New always uses a real second, matching MaxRequestsPerSecond's name.
func newLimiter(cfg Config, rateWindow time.Duration) *Limiter {
	return &Limiter{cfg: cfg, rate: newGlobalRateCounter(rateWindow)}
}

// Admit should be called once per connection (passthrough mode) or once
// per HTTP request (full mode - see internal/fullproxy, which treats a
// request rather than a connection as the unit of work, since HTTP/2
// multiplexes many requests over one connection). It returns a Decision
// and, for DecisionProceed and DecisionDegrade *both* - degraded
// connections still consume real resources (a goroutine, a socket) even
// though fingerprinting/recording is skipped for them, so they're still
// counted - a release func that must be called exactly once when that
// connection/request finishes; for DecisionReject, release is nil.
//
// Forgetting to release a DecisionDegrade result is an easy mistake (it
// was made once already, writing this package's own tests) and silently
// leaks a concurrency slot forever. Callers should use:
//
//	decision, release := l.Admit(ctx)
//	if release != nil {
//		defer release()
//	}
//	switch decision { ... }
//
// rather than branching on decision before deciding whether to defer.
//
// ctx bounds how long a PolicyThrottle caller will wait; passing
// context.Background() (as passthrough mode does, lacking a natural
// per-connection context) waits indefinitely, bounded only by
// ThrottleQueueSize's cap on how many can be waiting at once - full mode
// passes the request's own context, which is cancelled if the client
// disconnects while queued.
func (l *Limiter) Admit(ctx context.Context) (Decision, func()) {
	if d, release := l.tryProceed(); d == DecisionProceed {
		return d, release
	}

	switch l.cfg.Policy {
	case PolicyFailClosed:
		return DecisionReject, nil
	case PolicyThrottle:
		return l.throttleWait(ctx)
	default: // PolicyFailOpen, and the zero value of Policy
		l.current.Add(1) // still counted, for accurate load observability, just not gated on
		return DecisionDegrade, l.release
	}
}

// tryProceed makes one non-blocking attempt to acquire a concurrency slot
// under both limits. It never blocks and never applies Policy - callers
// interpret a non-Proceed result themselves.
func (l *Limiter) tryProceed() (Decision, func()) {
	if l.overRate() {
		return DecisionReject, nil
	}
	if l.tryAcquire() {
		return DecisionProceed, l.release
	}
	return DecisionReject, nil
}

func (l *Limiter) overRate() bool {
	if l.cfg.MaxRequestsPerSecond <= 0 {
		return false
	}
	now := time.Now()
	l.rate.record(now)
	return l.rate.rate(now) > float64(l.cfg.MaxRequestsPerSecond)
}

func (l *Limiter) tryAcquire() bool {
	if l.cfg.MaxConcurrentConnections <= 0 {
		l.current.Add(1)
		return true
	}
	for {
		cur := l.current.Load()
		if cur >= int64(l.cfg.MaxConcurrentConnections) {
			return false
		}
		if l.current.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

func (l *Limiter) release() {
	l.current.Add(-1)
}

func (l *Limiter) throttleWait(ctx context.Context) (Decision, func()) {
	if l.cfg.ThrottleQueueSize <= 0 {
		return DecisionReject, nil
	}

	waiting := l.waiting.Add(1)
	defer l.waiting.Add(-1)
	if waiting > int64(l.cfg.ThrottleQueueSize) {
		return DecisionReject, nil
	}

	if d, release := l.tryProceed(); d == DecisionProceed {
		return d, release
	}

	ticker := time.NewTicker(throttlePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return DecisionReject, nil
		case <-ticker.C:
			if d, release := l.tryProceed(); d == DecisionProceed {
				return d, release
			}
		}
	}
}

// globalRateCounter is a single (non-keyed) sliding-window request-rate
// counter: the same two-counter algorithm ratestore.MemoryRateStore uses
// per IP (including its "decay to zero after two full idle windows" fix -
// see rotate below, mirroring ratestore's rotatedCounts), applied instead
// to one aggregate count across the whole collector. Deliberately not
// shared with ratestore: it measures a different axis (total throughput,
// not per-IP behavior), and duplicating this much window math is simpler
// than contorting either package to serve both purposes.
type globalRateCounter struct {
	windowSize time.Duration

	mu          sync.Mutex
	prevCount   int
	currCount   int
	windowStart time.Time
}

func newGlobalRateCounter(windowSize time.Duration) *globalRateCounter {
	return &globalRateCounter{windowSize: windowSize}
}

// record counts one arrival at time now.
func (c *globalRateCounter) record(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.windowStart.IsZero() {
		c.windowStart = now.Truncate(c.windowSize)
	}
	c.prevCount, c.currCount, c.windowStart = rotate(c.prevCount, c.currCount, c.windowStart, now, c.windowSize)
	c.currCount++
}

// rate returns the current estimated rate, in events per windowSize, as
// of now. Safe to call without a preceding record (e.g. while polling in
// throttleWait): it performs the same virtual rotation record does,
// without mutating state, so it correctly decays even if nothing has
// called record recently.
func (c *globalRateCounter) rate(now time.Time) float64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.windowStart.IsZero() {
		return 0
	}
	prev, curr, windowStart := rotate(c.prevCount, c.currCount, c.windowStart, now, c.windowSize)
	weight := 1 - float64(now.Sub(windowStart))/float64(c.windowSize)
	if weight < 0 {
		weight = 0
	} else if weight > 1 {
		weight = 1
	}
	return float64(prev)*weight + float64(curr)
}

// rotate virtually advances a sliding window to now without mutating
// anything - see ratestore/memory.go's rotatedCounts for the full
// reasoning (a rate() call with no recent record() shouldn't keep
// reporting a stale, undecayed rate forever).
func rotate(prevCount, currCount int, windowStart, now time.Time, windowSize time.Duration) (newPrev, newCurr int, newWindowStart time.Time) {
	switch elapsed := now.Sub(windowStart); {
	case elapsed >= 2*windowSize:
		return 0, 0, now.Truncate(windowSize)
	case elapsed >= windowSize:
		return currCount, 0, windowStart.Add(windowSize)
	default:
		return prevCount, currCount, windowStart
	}
}
