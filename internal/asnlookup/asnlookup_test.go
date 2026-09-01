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
// tables (split into each dataset's v4/v6 table by every entry's own
// family), but no database pool - exactly the parts Resolve touches, none
// of the parts that need a live TimescaleDB (see writeCountryRanges /
// writeASNRanges, which like storage.Writer have their own build-tag-gated
// integration test instead).
func newTestResolver(countryEntries []rangeEntry[string], asnEntries []rangeEntry[asnInfo]) *Resolver {
	var countryV4, countryV6 []rangeEntry[string]
	for _, e := range countryEntries {
		if e.start.Is4() {
			countryV4 = append(countryV4, e)
		} else {
			countryV6 = append(countryV6, e)
		}
	}
	var asnV4, asnV6 []rangeEntry[asnInfo]
	for _, e := range asnEntries {
		if e.start.Is4() {
			asnV4 = append(asnV4, e)
		} else {
			asnV6 = append(asnV6, e)
		}
	}

	r := &Resolver{cache: newTTLCache(1000, time.Hour)}
	r.countryTable4.Store(newRangeTable(countryV4))
	r.countryTable6.Store(newRangeTable(countryV6))
	r.asnTable4.Store(newRangeTable(asnV4))
	r.asnTable6.Store(newRangeTable(asnV6))
	return r
}

func TestResolve_FoundIPv4ReturnsCountry(t *testing.T) {
	r := newTestResolver([]rangeEntry[string]{
		{start: netip.MustParseAddr("192.0.2.0"), end: netip.MustParseAddr("192.0.2.255"), value: "US"},
	}, nil)
	got := r.Resolve(netip.MustParseAddr("192.0.2.42"))
	want := Result{IP: netip.MustParseAddr("192.0.2.42"), Country: "US", Found: true}
	if got != want {
		t.Errorf("Resolve() = %+v, want %+v", got, want)
	}
}

func TestResolve_FoundIPv6ReturnsCountry(t *testing.T) {
	r := newTestResolver([]rangeEntry[string]{
		{start: netip.MustParseAddr("2001:db8::"), end: netip.MustParseAddr("2001:db8::ffff"), value: "JP"},
	}, nil)
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
	r := newTestResolver([]rangeEntry[string]{
		{start: netip.MustParseAddr("0.0.0.0"), end: netip.MustParseAddr("255.255.255.255"), value: "US"},
	}, nil)
	if got := r.Resolve(netip.MustParseAddr("2001:db8::1")); got.Found {
		t.Errorf("Resolve(IPv6) = %+v, want Found: false despite a full-coverage IPv4 table", got)
	}
}

func TestResolve_UnknownIPReturnsNotFound(t *testing.T) {
	r := newTestResolver([]rangeEntry[string]{
		{start: netip.MustParseAddr("192.0.2.0"), end: netip.MustParseAddr("192.0.2.255"), value: "US"},
	}, nil)
	got := r.Resolve(netip.MustParseAddr("203.0.113.1"))
	if got.Found {
		t.Errorf("Resolve() = %+v, want Found: false", got)
	}
}

func TestResolve_FoundASNReturnsASNInfo(t *testing.T) {
	r := newTestResolver(nil, []rangeEntry[asnInfo]{
		{start: netip.MustParseAddr("8.8.8.0"), end: netip.MustParseAddr("8.8.8.255"), value: asnInfo{asn: 15169, org: "GOOGLE"}},
	})
	got := r.Resolve(netip.MustParseAddr("8.8.8.8"))
	want := Result{IP: netip.MustParseAddr("8.8.8.8"), ASN: 15169, ASNName: "GOOGLE", Found: true}
	if got != want {
		t.Errorf("Resolve() = %+v, want %+v", got, want)
	}
}

func TestResolve_FoundInCountryOnly_ASNFieldsStayZero(t *testing.T) {
	// The country and ASN datasets are independent - an IP found in one
	// need not be found in the other. Found must still be true.
	r := newTestResolver([]rangeEntry[string]{
		{start: netip.MustParseAddr("192.0.2.0"), end: netip.MustParseAddr("192.0.2.255"), value: "US"},
	}, []rangeEntry[asnInfo]{
		{start: netip.MustParseAddr("8.8.8.0"), end: netip.MustParseAddr("8.8.8.255"), value: asnInfo{asn: 15169, org: "GOOGLE"}},
	})
	got := r.Resolve(netip.MustParseAddr("192.0.2.42"))
	want := Result{IP: netip.MustParseAddr("192.0.2.42"), Country: "US", Found: true}
	if got != want {
		t.Errorf("Resolve() = %+v, want %+v (found in country only)", got, want)
	}
}

func TestResolve_FoundInASNOnly_CountryFieldStaysZero(t *testing.T) {
	r := newTestResolver([]rangeEntry[string]{
		{start: netip.MustParseAddr("192.0.2.0"), end: netip.MustParseAddr("192.0.2.255"), value: "US"},
	}, []rangeEntry[asnInfo]{
		{start: netip.MustParseAddr("8.8.8.0"), end: netip.MustParseAddr("8.8.8.255"), value: asnInfo{asn: 15169, org: "GOOGLE"}},
	})
	got := r.Resolve(netip.MustParseAddr("8.8.8.8"))
	want := Result{IP: netip.MustParseAddr("8.8.8.8"), ASN: 15169, ASNName: "GOOGLE", Found: true}
	if got != want {
		t.Errorf("Resolve() = %+v, want %+v (found in ASN only)", got, want)
	}
}

func TestResolve_FoundInBothCountryAndASN(t *testing.T) {
	// A realistic case: the same address is covered by both datasets at
	// once (8.8.8.8 really is US-registered and really is AS15169).
	r := newTestResolver([]rangeEntry[string]{
		{start: netip.MustParseAddr("8.8.8.0"), end: netip.MustParseAddr("8.8.8.255"), value: "US"},
	}, []rangeEntry[asnInfo]{
		{start: netip.MustParseAddr("8.8.8.0"), end: netip.MustParseAddr("8.8.8.255"), value: asnInfo{asn: 15169, org: "GOOGLE"}},
	})
	got := r.Resolve(netip.MustParseAddr("8.8.8.8"))
	want := Result{IP: netip.MustParseAddr("8.8.8.8"), Country: "US", ASN: 15169, ASNName: "GOOGLE", Found: true}
	if got != want {
		t.Errorf("Resolve() = %+v, want %+v", got, want)
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
	r := newTestResolver([]rangeEntry[string]{
		{start: netip.MustParseAddr("192.0.2.0"), end: netip.MustParseAddr("192.0.2.255"), value: "US"},
	}, nil)
	ip := netip.MustParseAddr("192.0.2.42")

	first := r.Resolve(ip)
	if first.Country != "US" {
		t.Fatalf("first Resolve() = %+v, want country US", first)
	}

	// Swap in a table that no longer covers this IP at all. A second
	// Resolve for the same IP must still come from the cache, not the
	// (now different) table - proving Resolve actually checks the cache
	// first rather than always recomputing.
	r.countryTable4.Store(newRangeTable[string](nil))
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
	entries, _, err := r.loadCountryCSV(context.Background(), srv.URL, countryIPv4Filename)
	if err != nil {
		t.Fatalf("loadCountryCSV: %v", err)
	}
	if len(entries) != 1 || entries[0].value != "AU" {
		t.Fatalf("entries = %+v, want one AU entry", entries)
	}
}

func TestLoadCountryCSV_NonOKStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	r := &Resolver{httpClient: srv.Client()}
	if _, _, err := r.loadCountryCSV(context.Background(), srv.URL, countryIPv4Filename); err == nil {
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
	entries, _, err := r.loadCountryCSV(context.Background(), countryIPv4URL, countryIPv4Filename)
	if err != nil {
		t.Fatalf("loadCountryCSV: %v", err)
	}
	if len(entries) != 1 || entries[0].value != "AU" {
		t.Fatalf("entries = %+v, want one AU entry", entries)
	}
}

func TestLoadCountryCSV_LocalPathMissingFileIsAnError(t *testing.T) {
	r := &Resolver{localCSVPath: t.TempDir()} // empty dir - the expected file isn't there
	if _, _, err := r.loadCountryCSV(context.Background(), countryIPv4URL, countryIPv4Filename); err == nil {
		t.Error("loadCountryCSV() error = nil, want an error for a missing local file")
	}
}

func TestLoadASNCSV_ReadsAndParsesFromHTTPServer(t *testing.T) {
	const fixture = "1.0.0.0,1.0.0.255,13335,\"Cloudflare, Inc.\"\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(fixture))
	}))
	defer srv.Close()

	r := &Resolver{httpClient: srv.Client()}
	entries, _, err := r.loadASNCSV(context.Background(), srv.URL, asnIPv4Filename)
	if err != nil {
		t.Fatalf("loadASNCSV: %v", err)
	}
	if len(entries) != 1 || entries[0].value.asn != 13335 || entries[0].value.org != "Cloudflare, Inc." {
		t.Fatalf("entries = %+v, want one AS13335 Cloudflare entry", entries)
	}
}

func TestLoadASNCSV_NonOKStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	r := &Resolver{httpClient: srv.Client()}
	if _, _, err := r.loadASNCSV(context.Background(), srv.URL, asnIPv4Filename); err == nil {
		t.Error("loadASNCSV() error = nil, want an error for a non-200 response")
	}
}

func TestLoadASNCSV_LocalPathReadsFromDiskInsteadOfNetwork(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, asnIPv4Filename), []byte("1.0.0.0,1.0.0.255,15169,GOOGLE\n"), 0o644); err != nil {
		t.Fatalf("writing fixture file: %v", err)
	}

	// httpClient is deliberately nil, same reasoning as
	// TestLoadCountryCSV_LocalPathReadsFromDiskInsteadOfNetwork.
	r := &Resolver{localCSVPath: dir}
	entries, _, err := r.loadASNCSV(context.Background(), asnIPv4URL, asnIPv4Filename)
	if err != nil {
		t.Fatalf("loadASNCSV: %v", err)
	}
	if len(entries) != 1 || entries[0].value.asn != 15169 || entries[0].value.org != "GOOGLE" {
		t.Fatalf("entries = %+v, want one AS15169 GOOGLE entry", entries)
	}
}

func TestLoadASNCSV_LocalPathMissingFileIsAnError(t *testing.T) {
	r := &Resolver{localCSVPath: t.TempDir()} // empty dir - the expected file isn't there
	if _, _, err := r.loadASNCSV(context.Background(), asnIPv4URL, asnIPv4Filename); err == nil {
		t.Error("loadASNCSV() error = nil, want an error for a missing local file")
	}
}
