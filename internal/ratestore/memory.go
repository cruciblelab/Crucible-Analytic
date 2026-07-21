package ratestore

import (
	"net/netip"
	"sync"
	"time"
)

// ipState is the minimum per-IP state the MVP scope calls for: the two
// sliding-window counters, last-seen time, and associated JA4 fingerprint.
type ipState struct {
	ja4         string
	prevCount   int
	currCount   int
	windowStart time.Time
	lastSeen    time.Time
}

// MemoryRateStore is a single-process, in-memory RateStore implementation
// guarded by one sync.RWMutex over a plain map. This is intentionally
// simple for the MVP: a sharded map is a known follow-up if lock
// contention becomes measurable under higher connection concurrency, but
// both are equally valid starting points at small/medium scale.
type MemoryRateStore struct {
	windowSize time.Duration
	idleTTL    time.Duration

	mu     sync.RWMutex
	states map[netip.Addr]*ipState

	stopCleanup chan struct{}
	cleanupDone chan struct{}
}

// NewMemoryRateStore creates a RateStore that keeps two windowSize-wide
// sliding-window counters per IP, and automatically drops IPs idle for
// longer than idleTTL via a background sweep run every cleanupInterval.
// Call Close to stop that goroutine.
func NewMemoryRateStore(windowSize, idleTTL, cleanupInterval time.Duration) *MemoryRateStore {
	s := &MemoryRateStore{
		windowSize:  windowSize,
		idleTTL:     idleTTL,
		states:      make(map[netip.Addr]*ipState),
		stopCleanup: make(chan struct{}),
		cleanupDone: make(chan struct{}),
	}
	go s.cleanupLoop(cleanupInterval)
	return s
}

func (s *MemoryRateStore) RecordRequest(ip netip.Addr, ja4 string, now time.Time) WindowStats {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := s.states[ip]
	if !ok {
		st = &ipState{windowStart: now.Truncate(s.windowSize)}
		s.states[ip] = st
	}
	if ja4 != "" {
		st.ja4 = ja4
	}

	st.prevCount, st.currCount, st.windowStart = rotatedCounts(st.prevCount, st.currCount, st.windowStart, now, s.windowSize)
	st.currCount++
	st.lastSeen = now

	return windowStatsAt(st.prevCount, st.currCount, st.windowStart, now, s.windowSize)
}

func (s *MemoryRateStore) Snapshot(since, now time.Time) []Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]Snapshot, 0, len(s.states))
	for ip, st := range s.states {
		if st.lastSeen.Before(since) {
			continue
		}
		prev, curr, windowStart := rotatedCounts(st.prevCount, st.currCount, st.windowStart, now, s.windowSize)
		out = append(out, Snapshot{
			IP:          ip,
			JA4:         st.ja4,
			WindowStats: windowStatsAt(prev, curr, windowStart, now, s.windowSize),
			LastSeen:    st.lastSeen,
		})
	}
	return out
}

// rotatedCounts virtually advances a sliding window to now, returning the
// counts and window start that would result. It does not assume a request
// is being recorded, so both RecordRequest (which persists the result and
// then adds the new request) and Snapshot (read-only, for periodic flush)
// build on it — keeping the "how many window boundaries has this IP been
// idle across" rule in exactly one place instead of two copies that could
// drift apart.
//
// Notably, an IP that's been idle for two or more full windows decays
// straight to (0, 0): without this, a long-dormant-but-not-yet-TTL-purged
// IP would keep reporting its last burst's full current-window count
// forever, since only the *previous* window's count is ever weighted down.
func rotatedCounts(prevCount, currCount int, windowStart, now time.Time, windowSize time.Duration) (newPrev, newCurr int, newWindowStart time.Time) {
	switch elapsed := now.Sub(windowStart); {
	case elapsed >= 2*windowSize:
		return 0, 0, now.Truncate(windowSize)
	case elapsed >= windowSize:
		return currCount, 0, windowStart.Add(windowSize)
	default:
		// Covers both "still within the current window" and the
		// caller-supplied now landing before windowStart (elapsed < 0),
		// which can happen under concurrent calls if lock acquisition
		// order and timestamp order disagree; treat it as no rotation.
		return prevCount, currCount, windowStart
	}
}

// windowStatsAt computes the weighted sliding-window rate estimate,
// assuming counts/windowStart are already rotated as of now (i.e. via
// rotatedCounts). weight is clamped to [0,1] to stay well-defined even if
// elapsed ends up negative (see rotatedCounts).
func windowStatsAt(prevCount, currCount int, windowStart, now time.Time, windowSize time.Duration) WindowStats {
	var rate float64
	if windowSize > 0 {
		weight := 1 - float64(now.Sub(windowStart))/float64(windowSize)
		if weight < 0 {
			weight = 0
		} else if weight > 1 {
			weight = 1
		}
		rate = (float64(prevCount)*weight + float64(currCount)) / windowSize.Seconds()
	}

	return WindowStats{
		PrevWindowCount: prevCount,
		CurrWindowCount: currCount,
		EstimatedRate:   rate,
	}
}

func (s *MemoryRateStore) cleanupLoop(interval time.Duration) {
	defer close(s.cleanupDone)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopCleanup:
			return
		case t := <-ticker.C:
			s.removeExpired(t)
		}
	}
}

// removeExpired drops IP state entries whose last activity is older than
// idleTTL relative to now, so idle IPs don't grow the map unbounded. It's
// unexported and called both by the background cleanup loop and directly
// from tests in this package for deterministic (non-sleep-based) coverage.
func (s *MemoryRateStore) removeExpired(now time.Time) int {
	cutoff := now.Add(-s.idleTTL)
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for ip, st := range s.states {
		if st.lastSeen.Before(cutoff) {
			delete(s.states, ip)
			removed++
		}
	}
	return removed
}

// Close stops the background TTL cleanup goroutine. Safe to call once.
func (s *MemoryRateStore) Close() {
	close(s.stopCleanup)
	<-s.cleanupDone
}
