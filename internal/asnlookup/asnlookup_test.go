package asnlookup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

// newTestResolver builds a Resolver with a real cache and an injected
// range table, but no database pool - exactly the parts Resolve touches,
// none of the parts that need a live TimescaleDB (see writeRanges, which
// like storage.Writer has no automated test for the same reason: this
// repo was built without a usable Docker daemon to test against).
func newTestResolver(entries []rangeEntry) *Resolver {
	r := &Resolver{cache: newTTLCache(1000, time.Hour)}
	r.table.Store(newRangeTable(entries))
	return r
}

func TestResolve_FoundIPReturnsCountry(t *testing.T) {
	r := newTestResolver([]rangeEntry{{start: 0xC0000200, end: 0xC00002FF, country: "US"}})
	got := r.Resolve(netip.MustParseAddr("192.0.2.42"))
	want := Result{IP: netip.MustParseAddr("192.0.2.42"), Country: "US", Found: true}
	if got != want {
		t.Errorf("Resolve() = %+v, want %+v", got, want)
	}
}

func TestResolve_UnknownIPReturnsNotFound(t *testing.T) {
	r := newTestResolver([]rangeEntry{{start: 0xC0000200, end: 0xC00002FF, country: "US"}})
	got := r.Resolve(netip.MustParseAddr("203.0.113.1"))
	if got.Found {
		t.Errorf("Resolve() = %+v, want Found: false", got)
	}
}

func TestResolve_ASNFieldsAreAlwaysZeroValue(t *testing.T) {
	r := newTestResolver([]rangeEntry{{start: 0xC0000200, end: 0xC00002FF, country: "US"}})
	got := r.Resolve(netip.MustParseAddr("192.0.2.42"))
	if got.ASN != 0 || got.ASNName != "" {
		t.Errorf("Resolve() ASN=%d ASNName=%q, want zero value this phase - see package doc comment", got.ASN, got.ASNName)
	}
}

func TestResolve_IPv6InputIsAlwaysNotFound(t *testing.T) {
	// Even a table that (hypothetically) covered the whole IPv4 space
	// can't answer an IPv6 query - this phase only indexes IPv4.
	r := newTestResolver([]rangeEntry{{start: 0, end: 0xFFFFFFFF, country: "US"}})
	got := r.Resolve(netip.MustParseAddr("2600::1"))
	if got.Found {
		t.Errorf("Resolve(IPv6) = %+v, want Found: false regardless of table contents", got)
	}
}

func TestResolve_NilTableIsNotFoundNotPanic(t *testing.T) {
	r := &Resolver{cache: newTTLCache(1000, time.Hour)} // table never populated - simulates "before the first refresh"
	got := r.Resolve(netip.MustParseAddr("192.0.2.1"))
	if got.Found {
		t.Errorf("Resolve() with no refresh yet = %+v, want Found: false", got)
	}
}

func TestResolve_CacheIsConsultedBeforeTable(t *testing.T) {
	r := newTestResolver([]rangeEntry{{start: 0xC0000200, end: 0xC00002FF, country: "US"}})
	ip := netip.MustParseAddr("192.0.2.42")

	first := r.Resolve(ip)
	if first.Country != "US" {
		t.Fatalf("first Resolve() = %+v, want country US", first)
	}

	// Swap in a table that no longer covers this IP at all. A second
	// Resolve for the same IP must still come from the cache, not the
	// (now different) table - proving Resolve actually checks the cache
	// first rather than always recomputing.
	r.table.Store(newRangeTable(nil))
	second := r.Resolve(ip)
	if second != first {
		t.Errorf("second Resolve() = %+v, want cached %+v (unchanged despite the table swap)", second, first)
	}
}

func TestFetchAndParse_ReadsAndParsesFromHTTPServer(t *testing.T) {
	const fixture = "arin|US|ipv4|192.0.2.0|256|20000101|allocated|x\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(fixture))
	}))
	defer srv.Close()

	r := &Resolver{httpClient: srv.Client()}
	entries, err := r.fetchAndParse(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetchAndParse: %v", err)
	}
	if len(entries) != 1 || entries[0].country != "US" {
		t.Fatalf("entries = %+v, want one US entry", entries)
	}
}

func TestFetchAndParse_NonOKStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	r := &Resolver{httpClient: srv.Client()}
	if _, err := r.fetchAndParse(context.Background(), srv.URL); err == nil {
		t.Error("fetchAndParse() error = nil, want an error for a non-200 response")
	}
}
