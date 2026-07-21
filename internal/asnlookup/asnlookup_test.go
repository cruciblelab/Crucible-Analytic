package asnlookup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestResolver builds a Resolver with a real cache and injected range
// tables (split into table4/table6 by each entry's own family), but no
// database pool - exactly the parts Resolve touches, none of the parts
// that need a live TimescaleDB (see writeRanges, which like
// storage.Writer has its own build-tag-gated integration test instead).
func newTestResolver(entries []rangeEntry) *Resolver {
	var v4, v6 []rangeEntry
	for _, e := range entries {
		if e.start.Is4() {
			v4 = append(v4, e)
		} else {
			v6 = append(v6, e)
		}
	}
	r := &Resolver{cache: newTTLCache(1000, time.Hour)}
	r.table4.Store(newRangeTable(v4))
	r.table6.Store(newRangeTable(v6))
	return r
}

func TestResolve_FoundIPv4ReturnsCountry(t *testing.T) {
	r := newTestResolver([]rangeEntry{
		{start: netip.MustParseAddr("192.0.2.0"), end: netip.MustParseAddr("192.0.2.255"), country: "US"},
	})
	got := r.Resolve(netip.MustParseAddr("192.0.2.42"))
	want := Result{IP: netip.MustParseAddr("192.0.2.42"), Country: "US", Found: true}
	if got != want {
		t.Errorf("Resolve() = %+v, want %+v", got, want)
	}
}

func TestResolve_FoundIPv6ReturnsCountry(t *testing.T) {
	r := newTestResolver([]rangeEntry{
		{start: netip.MustParseAddr("2001:db8::"), end: netip.MustParseAddr("2001:db8::ffff"), country: "JP"},
	})
	got := r.Resolve(netip.MustParseAddr("2001:db8::1234"))
	want := Result{IP: netip.MustParseAddr("2001:db8::1234"), Country: "JP", Found: true}
	if got != want {
		t.Errorf("Resolve() = %+v, want %+v", got, want)
	}
}

func TestResolve_IPv4AndIPv6TablesAreIndependent(t *testing.T) {
	// A v4 table covering "everything" must never answer a v6 query, and
	// vice versa - proves Resolve actually routes by family rather than
	// consulting whichever table happens to be non-empty.
	r := newTestResolver([]rangeEntry{
		{start: netip.MustParseAddr("0.0.0.0"), end: netip.MustParseAddr("255.255.255.255"), country: "US"},
	})
	if got := r.Resolve(netip.MustParseAddr("2001:db8::1")); got.Found {
		t.Errorf("Resolve(IPv6) = %+v, want Found: false despite a full-coverage IPv4 table", got)
	}
}

func TestResolve_UnknownIPReturnsNotFound(t *testing.T) {
	r := newTestResolver([]rangeEntry{
		{start: netip.MustParseAddr("192.0.2.0"), end: netip.MustParseAddr("192.0.2.255"), country: "US"},
	})
	got := r.Resolve(netip.MustParseAddr("203.0.113.1"))
	if got.Found {
		t.Errorf("Resolve() = %+v, want Found: false", got)
	}
}

func TestResolve_ASNFieldsAreAlwaysZeroValue(t *testing.T) {
	r := newTestResolver([]rangeEntry{
		{start: netip.MustParseAddr("192.0.2.0"), end: netip.MustParseAddr("192.0.2.255"), country: "US"},
	})
	got := r.Resolve(netip.MustParseAddr("192.0.2.42"))
	if got.ASN != 0 || got.ASNName != "" {
		t.Errorf("Resolve() ASN=%d ASNName=%q, want zero value this phase - see package doc comment", got.ASN, got.ASNName)
	}
}

func TestResolve_NilTablesAreNotFoundNotPanic(t *testing.T) {
	r := &Resolver{cache: newTTLCache(1000, time.Hour)} // tables never populated - simulates "before the first refresh"
	if got := r.Resolve(netip.MustParseAddr("192.0.2.1")); got.Found {
		t.Errorf("Resolve() with no refresh yet = %+v, want Found: false", got)
	}
	if got := r.Resolve(netip.MustParseAddr("2001:db8::1")); got.Found {
		t.Errorf("Resolve(IPv6) with no refresh yet = %+v, want Found: false", got)
	}
}

func TestResolve_CacheIsConsultedBeforeTable(t *testing.T) {
	r := newTestResolver([]rangeEntry{
		{start: netip.MustParseAddr("192.0.2.0"), end: netip.MustParseAddr("192.0.2.255"), country: "US"},
	})
	ip := netip.MustParseAddr("192.0.2.42")

	first := r.Resolve(ip)
	if first.Country != "US" {
		t.Fatalf("first Resolve() = %+v, want country US", first)
	}

	// Swap in a table that no longer covers this IP at all. A second
	// Resolve for the same IP must still come from the cache, not the
	// (now different) table - proving Resolve actually checks the cache
	// first rather than always recomputing.
	r.table4.Store(newRangeTable(nil))
	second := r.Resolve(ip)
	if second != first {
		t.Errorf("second Resolve() = %+v, want cached %+v (unchanged despite the table swap)", second, first)
	}
}

func TestLoadCountryCSV_ReadsAndParsesFromHTTPServer(t *testing.T) {
	const fixture = "1.0.0.0,1.0.0.255,AU\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(fixture))
	}))
	defer srv.Close()

	r := &Resolver{httpClient: srv.Client()}
	entries, err := r.loadCountryCSV(context.Background(), srv.URL, countryIPv4Filename)
	if err != nil {
		t.Fatalf("loadCountryCSV: %v", err)
	}
	if len(entries) != 1 || entries[0].country != "AU" {
		t.Fatalf("entries = %+v, want one AU entry", entries)
	}
}

func TestLoadCountryCSV_NonOKStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	r := &Resolver{httpClient: srv.Client()}
	if _, err := r.loadCountryCSV(context.Background(), srv.URL, countryIPv4Filename); err == nil {
		t.Error("loadCountryCSV() error = nil, want an error for a non-200 response")
	}
}

func TestLoadCountryCSV_LocalPathReadsFromDiskInsteadOfNetwork(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, countryIPv4Filename), []byte("1.0.0.0,1.0.0.255,AU\n"), 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}

	// httpClient is deliberately nil: if loadCountryCSV took the network
	// path despite localCSVPath being set, calling r.httpClient.Do would
	// panic on a nil pointer - failing loudly rather than silently
	// falling through to a real network call.
	r := &Resolver{localCSVPath: dir}
	entries, err := r.loadCountryCSV(context.Background(), countryIPv4URL, countryIPv4Filename)
	if err != nil {
		t.Fatalf("loadCountryCSV: %v", err)
	}
	if len(entries) != 1 || entries[0].country != "AU" {
		t.Fatalf("entries = %+v, want one AU entry", entries)
	}
}

func TestLoadCountryCSV_LocalPathMissingFileIsAnError(t *testing.T) {
	r := &Resolver{localCSVPath: t.TempDir()} // empty dir - the expected file isn't there
	if _, err := r.loadCountryCSV(context.Background(), countryIPv4URL, countryIPv4Filename); err == nil {
		t.Error("loadCountryCSV() error = nil, want an error for a missing local file")
	}
}
