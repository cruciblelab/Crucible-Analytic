package ratestore

import (
	"net/netip"
	"sync"
	"testing"
	"time"
)

var testIP = netip.MustParseAddr("203.0.113.5")

func TestRecordRequest_AccumulatesWithinWindow(t *testing.T) {
	s := NewMemoryRateStore(10*time.Second, time.Minute, time.Hour)
	defer s.Close()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	stats := s.RecordRequest(testIP, "ja4-a", base)
	if stats.CurrWindowCount != 1 || stats.PrevWindowCount != 0 {
		t.Fatalf("after 1st request: %+v", stats)
	}

	stats = s.RecordRequest(testIP, "ja4-a", base.Add(2*time.Second))
	if stats.CurrWindowCount != 2 || stats.PrevWindowCount != 0 {
		t.Fatalf("after 2nd request: %+v", stats)
	}
}

func TestRecordRequest_RotatesAfterOneWindow(t *testing.T) {
	windowSize := 10 * time.Second
	s := NewMemoryRateStore(windowSize, time.Minute, time.Hour)
	defer s.Close()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Truncate(windowSize)

	for i := 0; i < 5; i++ {
		s.RecordRequest(testIP, "ja4-a", base.Add(time.Duration(i)*time.Second))
	}
	stats := s.RecordRequest(testIP, "ja4-a", base.Add(windowSize+time.Second))
	if stats.PrevWindowCount != 5 {
		t.Errorf("PrevWindowCount = %d, want 5", stats.PrevWindowCount)
	}
	if stats.CurrWindowCount != 1 {
		t.Errorf("CurrWindowCount = %d, want 1", stats.CurrWindowCount)
	}
}

func TestRecordRequest_ResetsAfterTwoWindowsIdle(t *testing.T) {
	windowSize := 10 * time.Second
	s := NewMemoryRateStore(windowSize, time.Minute, time.Hour)
	defer s.Close()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Truncate(windowSize)

	s.RecordRequest(testIP, "ja4-a", base)
	stats := s.RecordRequest(testIP, "ja4-a", base.Add(3*windowSize))
	if stats.PrevWindowCount != 0 {
		t.Errorf("PrevWindowCount = %d, want 0 after long idle", stats.PrevWindowCount)
	}
	if stats.CurrWindowCount != 1 {
		t.Errorf("CurrWindowCount = %d, want 1 after long idle", stats.CurrWindowCount)
	}
}

func TestRecordRequest_EstimatedRateWeighting(t *testing.T) {
	windowSize := 10 * time.Second
	s := NewMemoryRateStore(windowSize, time.Minute, time.Hour)
	defer s.Close()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Truncate(windowSize)

	for i := 0; i < 10; i++ {
		s.RecordRequest(testIP, "ja4-a", base.Add(time.Duration(i)*time.Millisecond))
	}
	// Halfway into the next window: the previous window's count should be
	// weighted at 0.5.
	stats := s.RecordRequest(testIP, "ja4-a", base.Add(windowSize+windowSize/2))

	wantEstimated := 10*0.5 + 1 // prevCount*weight + currCount
	wantRate := wantEstimated / windowSize.Seconds()
	if diff := stats.EstimatedRate - wantRate; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("EstimatedRate = %v, want %v", stats.EstimatedRate, wantRate)
	}
}

func TestRecordRequest_PreservesJA4WhenLaterRequestHasNone(t *testing.T) {
	s := NewMemoryRateStore(10*time.Second, time.Minute, time.Hour)
	defer s.Close()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.RecordRequest(testIP, "ja4-known", base)
	s.RecordRequest(testIP, "", base.Add(time.Second)) // e.g. plaintext HTTP request from same IP

	snaps := s.Snapshot(base.Add(-time.Minute), base.Add(time.Second))
	if len(snaps) != 1 || snaps[0].JA4 != "ja4-known" {
		t.Errorf("Snapshot = %+v, want JA4 preserved from the earlier request", snaps)
	}
}

func TestSnapshot_FiltersBySince(t *testing.T) {
	s := NewMemoryRateStore(10*time.Second, time.Minute, time.Hour)
	defer s.Close()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oldIP := netip.MustParseAddr("198.51.100.1")
	newIP := netip.MustParseAddr("198.51.100.2")

	s.RecordRequest(oldIP, "", base)
	s.RecordRequest(newIP, "", base.Add(20*time.Second))

	snaps := s.Snapshot(base.Add(10*time.Second), base.Add(20*time.Second))
	if len(snaps) != 1 || snaps[0].IP != newIP {
		t.Errorf("Snapshot = %+v, want only newIP", snaps)
	}
}

func TestSnapshot_RateDecaysForStaleEntriesAsOfNow(t *testing.T) {
	windowSize := 10 * time.Second
	s := NewMemoryRateStore(windowSize, 10*time.Minute, time.Hour) // long TTL so it isn't purged mid-test
	defer s.Close()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Truncate(windowSize)
	for i := 0; i < 10; i++ {
		s.RecordRequest(testIP, "ja4-a", base.Add(time.Duration(i)*time.Millisecond))
	}

	atLastSeen := s.Snapshot(time.Time{}, base)
	longAfter := s.Snapshot(time.Time{}, base.Add(5*windowSize))

	if got := atLastSeen[0].EstimatedRate; got != 1.0 {
		t.Errorf("rate at last-seen time = %v, want 1.0 (10 requests / 10s window)", got)
	}
	if got := longAfter[0].EstimatedRate; got != 0 {
		t.Errorf("rate long after last activity = %v, want 0 (fully decayed)", got)
	}
}

func TestRemoveExpired_DropsIdleEntries(t *testing.T) {
	s := NewMemoryRateStore(10*time.Second, time.Minute, time.Hour)
	defer s.Close()

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	staleIP := netip.MustParseAddr("198.51.100.10")
	freshIP := netip.MustParseAddr("198.51.100.11")

	s.RecordRequest(staleIP, "", base)
	s.RecordRequest(freshIP, "", base.Add(50*time.Second))

	removed := s.removeExpired(base.Add(90 * time.Second)) // staleIP idle > 60s TTL, freshIP not
	if removed != 1 {
		t.Fatalf("removeExpired removed %d entries, want 1", removed)
	}

	snaps := s.Snapshot(time.Time{}, base.Add(90*time.Second))
	if len(snaps) != 1 || snaps[0].IP != freshIP {
		t.Errorf("Snapshot after cleanup = %+v, want only freshIP", snaps)
	}
}

func TestClose_StopsCleanupGoroutinePromptly(t *testing.T) {
	s := NewMemoryRateStore(10*time.Second, time.Minute, time.Millisecond)
	done := make(chan struct{})
	go func() {
		s.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return within 5s; cleanup goroutine likely leaked")
	}
}

func TestConcurrentAccess(t *testing.T) {
	s := NewMemoryRateStore(10*time.Second, time.Minute, time.Hour)
	defer s.Close()

	ips := []netip.Addr{
		netip.MustParseAddr("203.0.113.1"),
		netip.MustParseAddr("203.0.113.2"),
		netip.MustParseAddr("203.0.113.3"),
	}

	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip := ips[i%len(ips)]
			s.RecordRequest(ip, "ja4", time.Now())
			s.Snapshot(time.Time{}, time.Now())
		}(i)
	}
	wg.Wait()
}
