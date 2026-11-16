package beacon

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/privacy"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// EventRateLimits is how many events one address may send before the
// beacon counts them as automated, and before it stops writing them (W2b).
//
// # Why the beacon judges a rate at all
//
// The collector's signed verdict (W2a) reaches the beacon only when a
// full-proxy collector stands in front of the site. In passthrough mode,
// or with no collector, nothing but the User-Agent spoke for an event,
// and 303 page views sent with curl from one address were counted as
// people (live rig, 2026-11-03).
//
// # Why two windows, and why one is 100 ms
//
// Measured on the live rig with real Chromium and the real snippet
// (2026-11-16, per address, the most events in any window):
//
//	                                        100 ms   1 s   60 s
//	fastest one person (gallery, 5 clicks/s)     3     9    277
//	30 people, one office address, fast         10    35  1,327
//	curl forger (305 events in 0.14 s)        ~218  ~285    305
//
// A crowd's events arrive spread out; a script's arrive back to back. The
// shorter the window, the wider the gap: 22 times at 100 ms, 8 at one
// second, and in a minute the office sends four times what the forger
// did. The first version used one second, with a mark of 100 and 3,000 a
// minute; a heavier office than the one these numbers came from (forty
// people, 66 events a second) had 5,413 of its 8,411 events marked.
//
// And a burst window alone would let a flood paced just under it run all
// day, so the minute holds the sustained rate.
//
// # Why a mark and a drop
//
// Marked events are written with auto_reason 'rate': left out of the
// counts, still in the table, still visible with bots=only - a person
// counted wrongly can be counted again. Above the drop thresholds the
// beacon writes nothing: a flood must not fill the customer's disk.
//
// A zero field is a limit that is off. The defaults are DefaultEventRate.
// The burst fields count events in any 100 ms (burstWindow).
type EventRateLimits struct {
	MarkBurst, MarkPerMinute int
	DropBurst, DropPerMinute int
}

// DefaultEventRate is the limits a beacon runs with when its file names
// none, from the measurements above and nothing else:
//
//   - the burst mark is five times the office's 10 in 100 ms, and the
//     burst drop twenty times;
//   - the minute mark is fifty people each as fast as the fastest one
//     (50 x 277 = 13,850, rounded up), and the minute drop twice that:
//     500 events a second, sustained, from one address.
var DefaultEventRate = EventRateLimits{
	MarkBurst:     50,
	MarkPerMinute: 15000,
	DropBurst:     200,
	DropPerMinute: 30000,
}

// burstWindow is the short window: see EventRateLimits for why 100 ms.
const burstWindow = 100 * time.Millisecond

// rateAction is what the limits make of one event.
type rateAction int

const (
	rateFine rateAction = iota
	rateMark
	rateDrop
)

func (l EventRateLimits) judge(burst, perMinute float64) rateAction {
	over := func(n float64, limit int) bool { return limit > 0 && n > float64(limit) }
	switch {
	case over(burst, l.DropBurst), over(perMinute, l.DropPerMinute):
		return rateDrop
	case over(burst, l.MarkBurst), over(perMinute, l.MarkPerMinute):
		return rateMark
	}
	return rateFine
}

// any reports whether a limit is on at all.
func (l EventRateLimits) any() bool {
	return l.MarkBurst > 0 || l.MarkPerMinute > 0 || l.DropBurst > 0 || l.DropPerMinute > 0
}

// maxRateAddresses bounds the table. Measured at about 160 bytes an
// address (see TestTheRateTableStaysInItsBudget), so the table at its
// ceiling is some 8 MB - inside the memory a beacon is given, which a
// table without one would not be: every IPv6 /64 is a new key.
const maxRateAddresses = 50_000

// rateSweepEvery is how often the table forgets addresses whose windows
// have emptied. Inline, under the lock, rather than in a goroutine of its
// own: a Server has no Close to stop one with.
const rateSweepEvery = 30 * time.Second

// addrRate is one address's two windows.
type addrRate struct {
	burst, minute ratestore.Counter
	// dropping is whether this address's last event was dropped, so the
	// log says when a flood begins rather than once per event of it.
	dropping bool
}

// eventRates is the table: in memory only, the address never leaves it.
type eventRates struct {
	mu        sync.Mutex
	addrs     map[netip.Addr]addrRate
	lastSweep time.Time
	// untracked is events not judged because the table was full.
	untracked atomic.Uint64
}

// rateKey is the address a rate is kept for: the whole IPv4 address, and
// an IPv6 /64. A /64 is one subscriber's network; keyed by the whole
// address, IPv6 would hand anyone 2^64 fresh addresses to flood from.
func rateKey(ip netip.Addr) netip.Addr {
	ip = ip.Unmap()
	if ip.Is6() {
		p, err := ip.Prefix(64)
		if err == nil {
			return p.Addr()
		}
	}
	return ip
}

// record counts one event from ip at now and returns what the limits make
// of it, and whether this event began a run of drops for its address.
// When the table is full and ip is not in it, the event is not judged:
// a full table is this server's limit, not the visitor's doing.
func (e *eventRates) record(ip netip.Addr, now time.Time, l EventRateLimits) (action rateAction, dropStarted bool) {
	key := rateKey(ip)
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.addrs == nil {
		e.addrs = make(map[netip.Addr]addrRate)
	}
	if now.Sub(e.lastSweep) >= rateSweepEvery {
		for k, st := range e.addrs {
			if st.minute.Idle(now, time.Minute) {
				delete(e.addrs, k)
			}
		}
		e.lastSweep = now
	}
	st, ok := e.addrs[key]
	if !ok && len(e.addrs) >= maxRateAddresses {
		e.untracked.Add(1)
		return rateFine, false
	}
	action = l.judge(st.burst.Add(now, burstWindow), st.minute.Add(now, time.Minute))
	dropStarted = action == rateDrop && !st.dropping
	st.dropping = action == rateDrop
	e.addrs[key] = st
	return action, dropStarted
}

// judgeRate records one event from ip and returns what the limits make of
// it. A Server whose limits are all off keeps no table.
func (s *Server) judgeRate(ip netip.Addr, now time.Time) rateAction {
	if !s.EventRate.any() {
		return rateFine
	}
	action, started := s.rates.record(ip, now, s.EventRate)
	if started {
		// Once per flood, not per event, and the masked network rather
		// than the address: this line goes to a log file, and the address
		// is the one thing the table exists not to write down.
		s.logger().Warn("beacon: an address is over the event rate's drop threshold; its events are not written until it slows",
			"network", privacy.MaskIP(ip, privacy.IPMasked).String(),
			"drop_burst", s.EventRate.DropBurst, "drop_per_minute", s.EventRate.DropPerMinute)
	}
	return action
}
