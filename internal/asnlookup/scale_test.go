//go:build loadtest

// Real memory and timing measurements against the actual, full
// user-country and origin-asn datasets - not an estimate. Gated behind
// both the "loadtest" build tag and an env var pointing at real,
// fully-downloaded CSVs, since committing multi-megabyte external
// datasets to this repo isn't appropriate, but the measurement itself is
// worth keeping runnable on demand as the datasets grow over time. Get
// the files (see the README), then run:
//
//	ASNLOOKUP_SCALE_TEST_DIR=/path/to/dir go test -tags loadtest ./internal/asnlookup/... -run TestScale -bench BenchmarkResolve -v

package asnlookup

import (
	"encoding/binary"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// parseFileOrFatal opens path, runs parse against it, and closes it -
// shared open/parse/close boilerplate for the 4 real dataset files (2
// datasets x 2 address families) this test and loadRealResolver both need.
func parseFileOrFatal[T any](tb testing.TB, parse func(io.Reader) ([]rangeEntry[T], error), path string) []rangeEntry[T] {
	tb.Helper()
	f, err := os.Open(path)
	if err != nil {
		tb.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	entries, err := parse(f)
	if err != nil {
		tb.Fatalf("parse %s: %v", path, err)
	}
	return entries
}

func TestScale_RealDatasetMemoryAndParseTime(t *testing.T) {
	dir := os.Getenv("ASNLOOKUP_SCALE_TEST_DIR")
	if dir == "" {
		t.Skip("ASNLOOKUP_SCALE_TEST_DIR not set - point it at a directory with real user-country-ipv4/6.csv and origin-asn-ipv4/6.csv to run this")
	}

	var baseline runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&baseline)

	countryParseStart := time.Now()
	countryEntries4 := parseFileOrFatal(t, parseCountryCSV, filepath.Join(dir, countryIPv4Filename))
	countryEntries6 := parseFileOrFatal(t, parseCountryCSV, filepath.Join(dir, countryIPv6Filename))
	countryParseDuration := time.Since(countryParseStart)

	countryBuildStart := time.Now()
	countryTable4 := newRangeTable(countryEntries4)
	countryTable6 := newRangeTable(countryEntries6)
	countryBuildDuration := time.Since(countryBuildStart)

	countryIPv4Count, countryIPv6Count := len(countryTable4.entries), len(countryTable6.entries)
	countryEntries4, countryEntries6 = nil, nil // let the raw parsed slices go - only the built tables should remain

	var afterCountry runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&afterCountry)
	countryHeapBytes := int64(afterCountry.HeapAlloc) - int64(baseline.HeapAlloc)

	asnParseStart := time.Now()
	asnEntries4 := parseFileOrFatal(t, parseASNCSV, filepath.Join(dir, asnIPv4Filename))
	asnEntries6 := parseFileOrFatal(t, parseASNCSV, filepath.Join(dir, asnIPv6Filename))
	asnParseDuration := time.Since(asnParseStart)

	asnBuildStart := time.Now()
	asnTable4 := newRangeTable(asnEntries4)
	asnTable6 := newRangeTable(asnEntries6)
	asnBuildDuration := time.Since(asnBuildStart)

	asnIPv4Count, asnIPv6Count := len(asnTable4.entries), len(asnTable6.entries)
	asnEntries4, asnEntries6 = nil, nil

	var afterASN runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&afterASN)
	asnHeapBytes := int64(afterASN.HeapAlloc) - int64(afterCountry.HeapAlloc)
	totalHeapBytes := int64(afterASN.HeapAlloc) - int64(baseline.HeapAlloc)

	t.Logf("country ipv4 ranges: %d", countryIPv4Count)
	t.Logf("country ipv6 ranges: %d", countryIPv6Count)
	t.Logf("country parse time (both files, encoding/csv): %v", countryParseDuration)
	t.Logf("country sort/build time (both tables): %v", countryBuildDuration)
	t.Logf("heap retained by both country range tables: %.2f MB (%d bytes) = %.1f bytes/entry",
		float64(countryHeapBytes)/(1024*1024), countryHeapBytes, float64(countryHeapBytes)/float64(countryIPv4Count+countryIPv6Count))

	t.Logf("asn ipv4 ranges: %d", asnIPv4Count)
	t.Logf("asn ipv6 ranges: %d", asnIPv6Count)
	t.Logf("asn parse time (both files, encoding/csv): %v", asnParseDuration)
	t.Logf("asn sort/build time (both tables): %v", asnBuildDuration)
	t.Logf("heap retained by both asn range tables: %.2f MB (%d bytes) = %.1f bytes/entry",
		float64(asnHeapBytes)/(1024*1024), asnHeapBytes, float64(asnHeapBytes)/float64(asnIPv4Count+asnIPv6Count))

	t.Logf("total heap retained by all four range tables (what a running collector with asn_lookup.enabled actually holds): %.2f MB (%d bytes)",
		float64(totalHeapBytes)/(1024*1024), totalHeapBytes)

	runtime.KeepAlive(countryTable4)
	runtime.KeepAlive(countryTable6)
	runtime.KeepAlive(asnTable4)
	runtime.KeepAlive(asnTable6)
}

// loadRealResolver builds a Resolver with real tables loaded from dir, for
// the benchmarks below - all four tables (country x {v4,v6}, ASN x
// {v4,v6}), matching what a running collector with asn_lookup.enabled
// actually holds.
func loadRealResolver(tb testing.TB, dir string) *Resolver {
	tb.Helper()

	countryEntries4 := parseFileOrFatal(tb, parseCountryCSV, filepath.Join(dir, countryIPv4Filename))
	countryEntries6 := parseFileOrFatal(tb, parseCountryCSV, filepath.Join(dir, countryIPv6Filename))
	asnEntries4 := parseFileOrFatal(tb, parseASNCSV, filepath.Join(dir, asnIPv4Filename))
	asnEntries6 := parseFileOrFatal(tb, parseASNCSV, filepath.Join(dir, asnIPv6Filename))

	r := &Resolver{cache: newTTLCache(50_000, 6*time.Hour)}
	r.countryTable4.Store(newRangeTable(countryEntries4))
	r.countryTable6.Store(newRangeTable(countryEntries6))
	r.asnTable4.Store(newRangeTable(asnEntries4))
	r.asnTable6.Store(newRangeTable(asnEntries6))
	return r
}

func scaleTestDirOrSkip(b *testing.B) string {
	dir := os.Getenv("ASNLOOKUP_SCALE_TEST_DIR")
	if dir == "" {
		b.Skip("ASNLOOKUP_SCALE_TEST_DIR not set - point it at a directory with real user-country-ipv4/6.csv and origin-asn-ipv4/6.csv to run this")
	}
	return dir
}

// BenchmarkResolve_CacheHit measures a warm lookup: the same IP resolved
// repeatedly, always served from the LRU cache, never touching either
// range table after the first call.
func BenchmarkResolve_CacheHit(b *testing.B) {
	dir := scaleTestDirOrSkip(b)
	r := loadRealResolver(b, dir)

	ip := netip.MustParseAddr("8.8.8.8")
	r.Resolve(ip) // warm the cache

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Resolve(ip)
	}
}

// BenchmarkResolve_CacheMiss measures a cold lookup: a distinct,
// never-seen-before IP every call, so every Resolve genuinely falls
// through to the binary search over both real, full-size tables (country
// and ASN) rather than ever hitting the cache.
func BenchmarkResolve_CacheMiss(b *testing.B) {
	dir := scaleTestDirOrSkip(b)
	r := loadRealResolver(b, dir)

	ips := make([]netip.Addr, b.N)
	for i := range ips {
		var raw [4]byte
		binary.BigEndian.PutUint32(raw[:], uint32(i))
		ips[i] = netip.AddrFrom4(raw)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.Resolve(ips[i])
	}
}
