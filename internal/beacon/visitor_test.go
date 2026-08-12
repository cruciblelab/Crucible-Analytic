package beacon

import (
	"net/netip"
	"sync"
	"testing"
	"time"
)

const (
	chromeUA  = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Safari/537.36"
	firefoxUA = "Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0"
)

func newTestVisitorIDs(t *testing.T) *VisitorIDs {
	t.Helper()
	v, err := NewVisitorIDs()
	if err != nil {
		t.Fatalf("NewVisitorIDs: %v", err)
	}
	return v
}

func mustID(t *testing.T, v *VisitorIDs, site string, ip netip.Addr, ua string) string {
	t.Helper()
	id, err := v.ID(site, ip, ua)
	if err != nil {
		t.Fatalf("ID: %v", err)
	}
	return id
}

func TestVisitorIDs_SameVisitorGetsOneID(t *testing.T) {
	v := newTestVisitorIDs(t)
	ip := netip.MustParseAddr("198.51.100.7")

	first := mustID(t, v, "acme", ip, chromeUA)
	second := mustID(t, v, "acme", ip, chromeUA)
	if first != second {
		t.Errorf("same visitor got two IDs: %q and %q", first, second)
	}
	if len(first) != visitorIDBytes*2 {
		t.Errorf("ID is %d hex characters, want %d", len(first), visitorIDBytes*2)
	}
}

// The reason this type exists: Turkish mobile carriers run CGNAT, so
// very many real people share one address. Counting DISTINCT ip would
// report them as one visitor.
func TestVisitorIDs_TwoPeopleBehindOneAddressAreSeparated(t *testing.T) {
	v := newTestVisitorIDs(t)
	shared := netip.MustParseAddr("100.64.12.1") // RFC 6598 CGNAT space

	chrome := mustID(t, v, "acme", shared, chromeUA)
	firefox := mustID(t, v, "acme", shared, firefoxUA)
	if chrome == firefox {
		t.Errorf("two browsers behind one CGNAT address collapsed into one visitor: %q", chrome)
	}
}

func TestVisitorIDs_SitesDoNotShareVisitors(t *testing.T) {
	v := newTestVisitorIDs(t)
	ip := netip.MustParseAddr("198.51.100.7")

	if mustID(t, v, "acme", ip, chromeUA) == mustID(t, v, "other", ip, chromeUA) {
		t.Error("the same person on two sites got one shared ID; a visitor should not be linkable across customers")
	}
}

// IPv6 privacy extensions (RFC 8981) rotate the low 64 bits regularly.
// Hashing the whole address would turn one person into a brand new
// "unique visitor" every time their OS rotated its temporary address.
func TestVisitorIDs_IPv6PrivacyRotationStaysOneVisitor(t *testing.T) {
	v := newTestVisitorIDs(t)
	before := netip.MustParseAddr("2001:db8:abcd:1234::1111:2222")
	after := netip.MustParseAddr("2001:db8:abcd:1234:9999:8888:7777:6666")

	if mustID(t, v, "acme", before, chromeUA) != mustID(t, v, "acme", after, chromeUA) {
		t.Error("two addresses in one /64 produced different visitors; privacy-extension rotation would inflate unique counts")
	}
}

func TestVisitorIDs_DifferentIPv6PrefixesAreDifferentVisitors(t *testing.T) {
	v := newTestVisitorIDs(t)
	one := netip.MustParseAddr("2001:db8:abcd:1234::1")
	two := netip.MustParseAddr("2001:db8:abcd:5678::1")

	if mustID(t, v, "acme", one, chromeUA) == mustID(t, v, "acme", two, chromeUA) {
		t.Error("two distinct /64s collapsed into one visitor")
	}
}

// Plain concatenation would hash ("1.2.3.4" + "5") and ("1.2.3.45" + "")
// to the same input. Both are legitimate values - the first is an
// address with a one-character user agent, the second a different
// address with none - so this is a real collision, not a contrived one.
func TestVisitorIDs_FieldBoundariesAreUnambiguous(t *testing.T) {
	v := newTestVisitorIDs(t)

	shifted := mustID(t, v, "acme", netip.MustParseAddr("1.2.3.4"), "5")
	unshifted := mustID(t, v, "acme", netip.MustParseAddr("1.2.3.45"), "")
	if shifted == unshifted {
		t.Error("field boundaries are ambiguous: two different (ip, user agent) pairs hashed identically")
	}
}

func TestVisitorIDs_RotatingTheSaltChangesEveryID(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	v := &VisitorIDs{
		SaltPeriod: time.Hour,
		Now:        func() time.Time { return now },
	}
	ip := netip.MustParseAddr("198.51.100.7")

	before := mustID(t, v, "acme", ip, chromeUA)
	// Still inside the period: same visitor, same ID.
	now = now.Add(59 * time.Minute)
	if mid := mustID(t, v, "acme", ip, chromeUA); mid != before {
		t.Errorf("ID changed within one salt period: %q then %q", before, mid)
	}
	// Past it: the old salt is gone, so the old ID is unreachable.
	now = now.Add(2 * time.Minute)
	if after := mustID(t, v, "acme", ip, chromeUA); after == before {
		t.Error("ID survived a salt rotation; it would be a durable cross-day identifier")
	}
}

func TestVisitorIDs_ZeroValueWorks(t *testing.T) {
	// Server.visitors() relies on this: a nil Visitors field is replaced
	// with &VisitorIDs{}, whose first use has to draw its own salt.
	var v VisitorIDs
	if _, err := v.ID("acme", netip.MustParseAddr("198.51.100.7"), chromeUA); err != nil {
		t.Fatalf("zero-value VisitorIDs failed: %v", err)
	}
}

func TestVisitorIDs_ConcurrentUseIsConsistent(t *testing.T) {
	v := newTestVisitorIDs(t)
	ip := netip.MustParseAddr("198.51.100.7")
	want := mustID(t, v, "acme", ip, chromeUA)

	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := v.ID("acme", ip, chromeUA)
			if err != nil {
				errs <- err.Error()
				return
			}
			if got != want {
				errs <- "concurrent ID mismatch: " + got + " != " + want
			}
		}()
	}
	wg.Wait()
	close(errs)
	for msg := range errs {
		t.Error(msg)
	}
}

func TestVisitorNetwork_IPv4IsUsedWhole(t *testing.T) {
	ip := netip.MustParseAddr("203.0.113.45")
	if got := visitorNetwork(ip); got != ip {
		t.Errorf("visitorNetwork(%v) = %v, want it unchanged", ip, got)
	}
}
