package limiter

import (
	"sync"
	"testing"
)

// TestAnEmptyBlocklistIsUsableAndInactive.
//
// This used to assert that empty input produced a nil blocklist, so a
// caller could skip resolving country and ASN entirely. A5.2 made the
// lists replaceable while connections are served, and a nil cannot be
// filled in later - so the constructor always returns something and the
// question moved to Active, which callers ask per connection.
//
// The optimisation itself is unchanged and is what this pins: a
// deployment blocking nothing must still report inactive, because that
// is what stops it paying for a geography lookup on every connection.
func TestAnEmptyBlocklistIsUsableAndInactive(t *testing.T) {
	for _, b := range []*GeoBlocklist{
		NewGeoBlocklist(nil, nil),
		NewGeoBlocklist([]string{}, []int{}),
		// Entries that mean nothing: a blank line, and the zero ASN that
		// is asnlookup's "not resolved". Both would match traffic nobody
		// asked to block.
		NewGeoBlocklist([]string{"", "   "}, []int{0}),
	} {
		if b == nil {
			t.Fatal("NewGeoBlocklist returned nil; it must always return a usable blocklist")
		}
		if b.Active() {
			t.Error("an empty blocklist reports active, so every connection would pay for a lookup")
		}
		if b.Blocked("CN", 64512) {
			t.Error("an empty blocklist blocked something")
		}
	}
	// And nil stays safe, because a server may have no blocklist at all.
	var none *GeoBlocklist
	if none.Active() || none.Blocked("CN", 64512) {
		t.Error("a nil blocklist is not inert")
	}
}

func TestGeoBlocklist_CountryMatch(t *testing.T) {
	b := NewGeoBlocklist([]string{"CN", "RU"}, nil)
	if !b.Blocked("CN", 0) {
		t.Error("Blocked(CN, 0) = false, want true")
	}
	if !b.Blocked("RU", 12345) {
		t.Error("Blocked(RU, 12345) = false, want true (country match alone is enough)")
	}
	if b.Blocked("US", 0) {
		t.Error("Blocked(US, 0) = true, want false")
	}
}

func TestGeoBlocklist_CountryMatchIsCaseInsensitive(t *testing.T) {
	b := NewGeoBlocklist([]string{"cn"}, nil)
	if !b.Blocked("CN", 0) {
		t.Error("Blocked(CN, 0) = false, want true (blocklist entry was lowercase)")
	}

	b2 := NewGeoBlocklist([]string{"CN"}, nil)
	if !b2.Blocked("cn", 0) {
		t.Error("Blocked(cn, 0) = false, want true (queried value was lowercase)")
	}
}

func TestGeoBlocklist_ASNMatch(t *testing.T) {
	b := NewGeoBlocklist(nil, []int{64512, 64513})
	if !b.Blocked("", 64512) {
		t.Error("Blocked(\"\", 64512) = false, want true")
	}
	if !b.Blocked("US", 64513) {
		t.Error("Blocked(US, 64513) = false, want true (ASN match alone is enough)")
	}
	if b.Blocked("US", 64514) {
		t.Error("Blocked(US, 64514) = true, want false")
	}
}

func TestGeoBlocklist_UnresolvedValuesNeverMatch(t *testing.T) {
	// A blocklist that would match "" or 0 if they were treated as real
	// values would be a bug: "" / 0 mean "not resolved" (asnlookup's own
	// zero-value convention), not "matched an empty rule".
	b := NewGeoBlocklist([]string{"CN"}, []int{64512})
	if b.Blocked("", 0) {
		t.Error("Blocked(\"\", 0) = true, want false - unresolved country/ASN must never match")
	}
}

func TestGeoBlocklist_NilBlocklistNeverBlocks(t *testing.T) {
	var b *GeoBlocklist
	if b.Blocked("CN", 64512) {
		t.Error("nil GeoBlocklist blocked a lookup, want it to always report false")
	}
}

// TestSetReplacesTheRulesWhileBlockedIsRunning.
//
// The whole point of A5.2: "we are being hit from there, block it" has
// to work without a restart. Run under -race, which is what makes the
// atomic publication meaningful rather than decorative.
func TestSetReplacesTheRulesWhileBlockedIsRunning(t *testing.T) {
	b := NewGeoBlocklist(nil, nil)

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers, as a server's connections would be.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					b.Blocked("CN", 64512)
					b.Active()
				}
			}
		}()
	}

	// Writers, as the settings poll would be.
	for i := range 100 {
		if i%2 == 0 {
			b.Set([]string{"CN"}, []int{64512})
		} else {
			b.Set(nil, nil)
		}
	}
	close(stop)
	wg.Wait()

	// And the last write is what stands.
	b.Set([]string{"RU"}, nil)
	if !b.Active() {
		t.Error("a blocklist with a country in it reports inactive")
	}
	if !b.Blocked("RU", 0) {
		t.Error("the country just set is not blocked")
	}
	if b.Blocked("CN", 64512) {
		t.Error("a rule from a previous version of the list is still in force")
	}
}

// TestSetIsSafeOnNil, because a caller may hold one it never built.
func TestSetIsSafeOnNil(t *testing.T) {
	var b *GeoBlocklist
	b.Set([]string{"CN"}, []int{64512})
	if b.Active() {
		t.Error("a nil blocklist became active")
	}
}
