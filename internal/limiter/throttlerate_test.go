package limiter

import (
	"context"
	"testing"
	"time"
)

// A queued caller under PolicyThrottle is one request, not a request per
// poll.
//
// Measured before the fix: every poll went through tryProceed, which
// records an arrival in the rate counter, so each waiter added fifty
// requests a second of its own. Four waiters under a 100/s limit keep
// the rate at 200/s with no traffic at all - the queue holds itself over
// the limit and nobody in it is ever admitted. CI run 416 hung for ten
// minutes on exactly that, four goroutines in throttleWait, inside
// TestConfigChangesAreRaceFree.
//
// In a deployment it is worse than a hang. Passthrough mode admits with
// context.Background(), so a waiter never gives up; with the collector's
// defaults (500/s, a queue of 200) eleven waiters are enough, and from
// then on every new connection finds the rate over the limit and the
// queue full. The collector admits nothing until it is restarted.
//
// Nothing caught it because no test combined the two: every throttle
// test here and in internal/loadtest limits concurrency, and every rate
// test uses fail_closed or fail_open.

// TestAQueuedCallerIsNotItsOwnTraffic: after a burst ends, the queue
// drains once the real rate has decayed.
func TestAQueuedCallerIsNotItsOwnTraffic(t *testing.T) {
	const window = 100 * time.Millisecond
	// Ten a window. Four waiters polling every 20ms would add twenty a
	// window if a poll counted - twice the limit, for as long as they
	// wait.
	l := newLimiter(Config{
		MaxRequestsPerSecond: 10,
		Policy:               PolicyThrottle,
		ThrottleQueueSize:    4,
	}, window)

	// A burst well over the limit, and then nothing.
	now := time.Now()
	for range 30 {
		l.rate.record(now)
	}

	// Thirty windows: with the burst gone after two, a queue that is
	// not feeding itself is empty long before this.
	ctx, cancel := context.WithTimeout(context.Background(), 30*window)
	defer cancel()

	type outcome struct {
		d     Decision
		after time.Duration
	}
	out := make(chan outcome, 4)
	start := time.Now()
	for range 4 {
		go func() {
			d, release := l.Admit(ctx)
			if release != nil {
				release()
			}
			out <- outcome{d, time.Since(start)}
		}()
	}

	admitted := 0
	var last time.Duration
	for range 4 {
		o := <-out
		if o.d == DecisionProceed {
			admitted++
			last = max(last, o.after)
		}
	}
	if admitted != 4 {
		t.Fatalf("%d of 4 queued callers were admitted in %v; the burst was over after "+
			"two windows (%v) and nothing else arrived. A waiter whose polls count as "+
			"requests keeps the rate over the limit by itself.", admitted, 30*window, 2*window)
	}
	// And promptly: the burst decays over two windows, then one poll.
	if last > 10*window {
		t.Errorf("the last waiter was admitted after %v; the rate was clear after about %v",
			last, 2*window+throttlePollInterval)
	}
}

// TestAThrottledRequestIsCountedOnce: one arrival, one count, however
// long it waits and however many times it looks.
//
// The window is long enough that nothing rotates while the caller waits,
// so the counter holds every record it was given - and the limit stays
// exceeded, so the caller polls for the whole of its context and then
// gives up.
func TestAThrottledRequestIsCountedOnce(t *testing.T) {
	l := newLimiter(Config{
		MaxRequestsPerSecond: 5,
		Policy:               PolicyThrottle,
		ThrottleQueueSize:    1,
	}, 1000*time.Hour)

	now := time.Now()
	for range 10 {
		l.rate.record(now)
	}

	// About ten polls' worth.
	ctx, cancel := context.WithTimeout(context.Background(), 10*throttlePollInterval)
	defer cancel()
	d, release := l.Admit(ctx)
	if release != nil {
		release()
	}
	if d != DecisionReject {
		t.Fatalf("decision = %v; the limit was exceeded for the whole wait, so the caller "+
			"should have queued and given up - the count below measures nothing otherwise", d)
	}

	l.rate.mu.Lock()
	recorded := l.rate.prevCount + l.rate.currCount
	l.rate.mu.Unlock()
	if recorded != 11 {
		t.Errorf("the counter holds %d arrivals: the 10 recorded before, and the queued "+
			"request counted %d times. It arrived once.", recorded, recorded-10)
	}
}
