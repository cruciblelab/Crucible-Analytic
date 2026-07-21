package asnlookup

import (
	"net/netip"
	"sync"
	"testing"
	"time"
)

func TestTTLCache_SetThenGet(t *testing.T) {
	c := newTTLCache(10, time.Minute)
	ip := netip.MustParseAddr("192.0.2.1")
	want := Result{IP: ip, Country: "US", Found: true}

	c.set(ip, want)
	got, ok := c.get(ip)
	if !ok {
		t.Fatal("get() ok = false, want true")
	}
	if got != want {
		t.Errorf("get() = %+v, want %+v", got, want)
	}
}

func TestTTLCache_MissReturnsNotOK(t *testing.T) {
	c := newTTLCache(10, time.Minute)
	if _, ok := c.get(netip.MustParseAddr("192.0.2.1")); ok {
		t.Error("get() on an empty cache ok = true, want false")
	}
}

func TestTTLCache_ExpiresAfterTTL(t *testing.T) {
	c := newTTLCache(10, time.Minute)
	now := time.Now()
	c.now = func() time.Time { return now }

	ip := netip.MustParseAddr("192.0.2.1")
	c.set(ip, Result{IP: ip, Country: "US", Found: true})

	now = now.Add(59 * time.Second)
	if _, ok := c.get(ip); !ok {
		t.Error("get() before TTL elapsed ok = false, want true")
	}

	now = now.Add(2 * time.Second) // now 61s after set, past the 1-minute TTL
	if _, ok := c.get(ip); ok {
		t.Error("get() after TTL elapsed ok = true, want false")
	}
	if c.len() != 0 {
		t.Errorf("len() after an expired get = %d, want 0 (lazy eviction on read)", c.len())
	}
}

func TestTTLCache_EvictsLeastRecentlyUsedAtCapacity(t *testing.T) {
	c := newTTLCache(2, time.Minute)
	ip1 := netip.MustParseAddr("192.0.2.1")
	ip2 := netip.MustParseAddr("192.0.2.2")
	ip3 := netip.MustParseAddr("192.0.2.3")

	c.set(ip1, Result{IP: ip1, Found: true})
	c.set(ip2, Result{IP: ip2, Found: true})
	c.get(ip1)                               // touch ip1 so ip2 becomes the least recently used
	c.set(ip3, Result{IP: ip3, Found: true}) // should evict ip2, not ip1

	if _, ok := c.get(ip2); ok {
		t.Error("get(ip2) ok = true, want false (should have been evicted as LRU)")
	}
	if _, ok := c.get(ip1); !ok {
		t.Error("get(ip1) ok = false, want true (was touched, should have survived)")
	}
	if _, ok := c.get(ip3); !ok {
		t.Error("get(ip3) ok = false, want true (just inserted)")
	}
	if c.len() != 2 {
		t.Errorf("len() = %d, want 2 (capacity)", c.len())
	}
}

func TestTTLCache_SetOnExistingKeyRefreshesRecencyAndTTL(t *testing.T) {
	c := newTTLCache(10, time.Minute)
	now := time.Now()
	c.now = func() time.Time { return now }

	ip := netip.MustParseAddr("192.0.2.1")
	c.set(ip, Result{IP: ip, Country: "US", Found: true})

	now = now.Add(50 * time.Second)
	c.set(ip, Result{IP: ip, Country: "DE", Found: true}) // re-set resets the TTL clock

	now = now.Add(50 * time.Second) // 100s after the first set, but only 50s after the re-set
	got, ok := c.get(ip)
	if !ok {
		t.Fatal("get() ok = false, want true (TTL should have been refreshed by the second set)")
	}
	if got.Country != "DE" {
		t.Errorf("get().Country = %q, want DE (should reflect the updated value)", got.Country)
	}
	if c.len() != 1 {
		t.Errorf("len() = %d, want 1 (re-setting an existing key must not create a second entry)", c.len())
	}
}

func TestTTLCache_ConcurrentUse(t *testing.T) {
	c := newTTLCache(50, time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip := netip.AddrFrom4([4]byte{192, 0, 2, byte(i)})
			for j := 0; j < 50; j++ {
				c.set(ip, Result{IP: ip, Found: true})
				c.get(ip)
			}
		}(i)
	}
	wg.Wait()
}
