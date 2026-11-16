package ratestore

import (
	"net/netip"
	"testing"
	"time"
)

// Counter agrees with RecordRequest for the same arrivals: the one rule,
// read through both doors. The arrivals straddle a window boundary, so the
// weighting of the previous window is exercised, not only the count.
func TestCounterIsRecordRequestsArithmetic(t *testing.T) {
	const size = 10 * time.Second
	s := NewMemoryRateStore(size, time.Hour, time.Hour)
	defer s.Close()
	base := time.Date(2026, 11, 16, 12, 0, 3, 0, time.UTC)
	var c Counter
	for i, off := range []time.Duration{0, time.Second, 4 * time.Second, 8 * time.Second, 12 * time.Second, 13 * time.Second, 19 * time.Second} {
		now := base.Add(off)
		got := c.Add(now, size)
		want := s.RecordRequest(netip.MustParseAddr("192.0.2.1"), "", now).EstimatedRate * size.Seconds()
		if diff := got - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("arrival %d at +%v: Counter holds %.3f, RecordRequest %.3f", i, off, got, want)
		}
	}
}

// Within one window the count is exact; after two idle windows it starts
// again from one, and Idle says so from that moment and not before.
func TestCounterCountsAndForgets(t *testing.T) {
	const size = time.Second
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	var c Counter
	for i := 1; i <= 5; i++ {
		if got := c.Add(now, size); got != float64(i) {
			t.Fatalf("event %d in one instant: %v, want %d", i, got, i)
		}
	}
	if c.Idle(now.Add(2*size-time.Nanosecond), size) {
		t.Error("Idle a nanosecond before two windows had passed")
	}
	if !c.Idle(now.Add(2*size), size) {
		t.Error("not Idle after two whole windows")
	}
	if got := c.Add(now.Add(2*size), size); got != 1 {
		t.Errorf("first event after two idle windows: %v, want 1", got)
	}
}
