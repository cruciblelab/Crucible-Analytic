package limiter

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestAdmit_UnlimitedByDefault(t *testing.T) {
	l := New(Config{})
	for i := 0; i < 50; i++ {
		d, release := l.Admit(context.Background())
		if d != DecisionProceed {
			t.Fatalf("call %d: Decision = %v, want DecisionProceed", i, d)
		}
		if release == nil {
			t.Fatalf("call %d: release = nil, want a non-nil func for DecisionProceed", i)
		}
		release()
	}
}

func TestAdmit_ConcurrentLimit_ProceedThenReleaseFreesSlot(t *testing.T) {
	l := New(Config{MaxConcurrentConnections: 1})

	d1, release1 := l.Admit(context.Background())
	if d1 != DecisionProceed {
		t.Fatalf("1st Admit = %v, want DecisionProceed", d1)
	}

	d2, release2 := l.Admit(context.Background())
	if d2 != DecisionDegrade { // default policy is fail_open
		t.Fatalf("2nd Admit (over limit) = %v, want DecisionDegrade (default policy is fail_open)", d2)
	}
	// DecisionDegrade still must be released (see Admit's doc comment) -
	// degraded connections are still counted for load observability.
	release2()

	release1()

	d3, release3 := l.Admit(context.Background())
	if d3 != DecisionProceed {
		t.Fatalf("3rd Admit (after release) = %v, want DecisionProceed", d3)
	}
	release3()
}

func TestAdmit_FailClosed_RejectsOverConcurrentLimit(t *testing.T) {
	l := New(Config{MaxConcurrentConnections: 1, Policy: PolicyFailClosed})

	d1, release1 := l.Admit(context.Background())
	defer release1()
	if d1 != DecisionProceed {
		t.Fatalf("1st Admit = %v, want DecisionProceed", d1)
	}

	d2, release2 := l.Admit(context.Background())
	if d2 != DecisionReject {
		t.Fatalf("2nd Admit (over limit) = %v, want DecisionReject", d2)
	}
	if release2 != nil {
		t.Error("release for DecisionReject should be nil")
	}
}

func TestAdmit_FailOpen_DegradesOverConcurrentLimit(t *testing.T) {
	l := New(Config{MaxConcurrentConnections: 1, Policy: PolicyFailOpen})

	d1, release1 := l.Admit(context.Background())
	defer release1()
	if d1 != DecisionProceed {
		t.Fatalf("1st Admit = %v, want DecisionProceed", d1)
	}

	d2, release2 := l.Admit(context.Background())
	if d2 != DecisionDegrade {
		t.Fatalf("2nd Admit (over limit) = %v, want DecisionDegrade", d2)
	}
	if release2 == nil {
		t.Fatal("release for DecisionDegrade should not be nil (still tracked for load observability)")
	}
	release2()
}

func TestAdmit_FailClosed_RejectsOverRateLimit(t *testing.T) {
	l := newLimiter(Config{MaxRequestsPerSecond: 2, Policy: PolicyFailClosed}, 200*time.Millisecond)

	for i := 0; i < 2; i++ {
		d, release := l.Admit(context.Background())
		if d != DecisionProceed {
			t.Fatalf("call %d = %v, want DecisionProceed (under rate limit)", i, d)
		}
		release()
	}

	d, release := l.Admit(context.Background())
	if d != DecisionReject {
		t.Fatalf("3rd call (over rate limit) = %v, want DecisionReject", d)
	}
	if release != nil {
		t.Error("release for DecisionReject should be nil")
	}
}

func TestAdmit_RateLimitRecoversAfterWindowPasses(t *testing.T) {
	window := 100 * time.Millisecond
	l := newLimiter(Config{MaxRequestsPerSecond: 1, Policy: PolicyFailClosed}, window)

	d1, release1 := l.Admit(context.Background())
	if d1 != DecisionProceed {
		t.Fatalf("1st call = %v, want DecisionProceed", d1)
	}
	release1()

	d2, _ := l.Admit(context.Background())
	if d2 != DecisionReject {
		t.Fatalf("2nd call (same window) = %v, want DecisionReject", d2)
	}

	time.Sleep(3 * window)

	d3, release3 := l.Admit(context.Background())
	if d3 != DecisionProceed {
		t.Fatalf("call after waiting out the window = %v, want DecisionProceed", d3)
	}
	release3()
}

func TestAdmit_Throttle_WaitsForSlotThenProceeds(t *testing.T) {
	l := New(Config{MaxConcurrentConnections: 1, Policy: PolicyThrottle, ThrottleQueueSize: 5})

	_, release1 := l.Admit(context.Background())

	resultCh := make(chan Decision, 1)
	go func() {
		d, release := l.Admit(context.Background())
		resultCh <- d
		if release != nil {
			release()
		}
	}()

	// Give the goroutine time to actually enter the wait loop before we
	// free the slot, so this test exercises waiting-then-admitted rather
	// than a race that might immediately succeed.
	time.Sleep(50 * time.Millisecond)
	select {
	case d := <-resultCh:
		t.Fatalf("throttled Admit returned %v before the slot was released", d)
	default:
	}

	release1()

	select {
	case d := <-resultCh:
		if d != DecisionProceed {
			t.Errorf("throttled Admit, after slot freed, = %v, want DecisionProceed", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("throttled Admit did not return within 2s of the slot being released")
	}
}

func TestAdmit_Throttle_QueueFullRejectsImmediately(t *testing.T) {
	l := New(Config{MaxConcurrentConnections: 1, Policy: PolicyThrottle, ThrottleQueueSize: 1})

	_, releaseHeld := l.Admit(context.Background())
	defer releaseHeld()

	// Occupy the one queue slot with a long-running waiter.
	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	defer cancelWaiter()
	waiterStarted := make(chan struct{})
	go func() {
		close(waiterStarted)
		l.Admit(waiterCtx) // blocks until cancelWaiter or a slot frees
	}()
	<-waiterStarted
	time.Sleep(50 * time.Millisecond) // let it actually reach the queue

	d, release := l.Admit(context.Background())
	if d != DecisionReject {
		t.Fatalf("Admit with the queue already full = %v, want DecisionReject", d)
	}
	if release != nil {
		t.Error("release for DecisionReject should be nil")
	}
}

func TestAdmit_Throttle_ContextCancelStopsWaiting(t *testing.T) {
	l := New(Config{MaxConcurrentConnections: 1, Policy: PolicyThrottle, ThrottleQueueSize: 5})

	_, releaseHeld := l.Admit(context.Background())
	defer releaseHeld()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	d, release := l.Admit(ctx)
	elapsed := time.Since(start)

	if d != DecisionReject {
		t.Errorf("Admit with an expiring context (slot never freed) = %v, want DecisionReject", d)
	}
	if release != nil {
		t.Error("release for DecisionReject should be nil")
	}
	if elapsed > time.Second {
		t.Errorf("Admit took %v to give up after context expiry, want well under 1s", elapsed)
	}
}

func TestAdmit_Throttle_ZeroQueueSizeAlwaysRejectsOverLimit(t *testing.T) {
	l := New(Config{MaxConcurrentConnections: 1, Policy: PolicyThrottle, ThrottleQueueSize: 0})

	_, release1 := l.Admit(context.Background())
	defer release1()

	d, release2 := l.Admit(context.Background())
	if d != DecisionReject {
		t.Errorf("Admit over limit with ThrottleQueueSize=0 = %v, want DecisionReject", d)
	}
	if release2 != nil {
		t.Error("release for DecisionReject should be nil")
	}
}

func TestLimiter_ConcurrentUse(t *testing.T) {
	l := New(Config{MaxConcurrentConnections: 10, MaxRequestsPerSecond: 1000, Policy: PolicyFailOpen})

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, release := l.Admit(context.Background())
			if d == DecisionReject {
				return
			}
			time.Sleep(time.Millisecond)
			release()
		}()
	}
	wg.Wait()
}
