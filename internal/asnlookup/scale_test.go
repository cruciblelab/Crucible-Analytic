//go:build loadtest

// Real memory and timing measurements against the actual, full
// user-country dataset - not an estimate. Gated behind both the
// "loadtest" build tag and an env var pointing at real, fully-downloaded
// CSVs, since committing a multi-megabyte external dataset to this repo
// isn't appropriate, but the measurement itself is worth keeping runnable
// on demand as the dataset grows over time. Get the files (see the
// README's "Optional: IP → country lookup" for the URLs), then run:
//
//	ASNLOOKUP_SCALE_TEST_DIR=/path/to/dir go test -tags loadtest ./internal/asnlookup/... -run TestScale -bench BenchmarkResolve -v

package asnlookup

import (
	"encoding/binary"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestScale_RealDatasetMemoryAndParseTime(t *testing.T) {
	dir := os.Getenv("ASNLOOKUP_SCALE_TEST_DIR")
	if dir == "" {
		t.Skip("ASNLOOKUP_SCALE_TEST_DIR not set - point it at a directory with real user-country-ipv4.csv/-ipv6.csv to run this")
	}

	var baseline runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&baseline)

	f4, err := os.Open(filepath.Join(dir, countryIPv4Filename))
	if err != nil {
		t.Fatalf("open ipv4 file: %v", err)
	}
	parseStart := time.Now()
	entries4, err := parseCountryCSV(f4)
	f4.Close()
	if err != nil {
		t.Fatalf("parse ipv4: %v", err)
	}

	f6, err := os.Open(filepath.Join(dir, countryIPv6Filename))
	if err != nil {
		t.Fatalf("open ipv6 file: %v", err)
	}
	entries6, err := parseCountryCSV(f6)
	f6.Close()
	if err != nil {
		t.Fatalf("parse ipv6: %v", err)
	}
	parseDuration := time.Since(parseStart)

	buildStart := time.Now()
	table4 := newRangeTable(entries4)
	table6 := newRangeTable(entries6)
	buildDuration := time.Since(buildStart)

	ipv4Count, ipv6Count := len(table4.entries), len(table6.entries)
	entries4, entries6 = nil, nil // let the raw parsed slices go - only the built tables should remain

	var afterTables runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&afterTables)
	tableHeapBytes := int64(afterTables.HeapAlloc) - int64(baseline.HeapAlloc)

	t.Logf("ipv4 ranges: %d", ipv4Count)
	t.Logf("ipv6 ranges: %d", ipv6Count)
	t.Logf("total ranges: %d", ipv4Count+ipv6Count)
	t.Logf("parse time (both files, encoding/csv): %v", parseDuration)
	t.Logf("sort/build time (both tables): %v", buildDuration)
	t.Logf("heap retained by both range tables: %.2f MB (%d bytes) = %.1f bytes/entry",
		float64(tableHeapBytes)/(1024*1024), tableHeapBytes, float64(tableHeapBytes)/float64(ipv4Count+ipv6Count))

	runtime.KeepAlive(table4)
	runtime.KeepAlive(table6)
}

// loadRealResolver builds a Resolver with real tables loaded from dir,
// for the benchmarks below.
func loadRealResolver(tb testing.TB, dir string) *Resolver {
	tb.Helper()

	f4, err := os.Open(filepath.Join(dir, countryIPv4Filename))
	if err != nil {
		tb.Fatalf("open ipv4 file: %v", err)
	}
	defer f4.Close()
	entries4, err := parseCountryCSV(f4)
	if err != nil {
		tb.Fatalf("parse ipv4: %v", err)
	}

	f6, err := os.Open(filepath.Join(dir, countryIPv6Filename))
	if err != nil {
		tb.Fatalf("open ipv6 file: %v", err)
	}
	defer f6.Close()
	entries6, err := parseCountryCSV(f6)
	if err != nil {
		tb.Fatalf("parse ipv6: %v", err)
	}

	r := &Resolver{cache: newTTLCache(50_000, 6*time.Hour)}
	r.table4.Store(newRangeTable(entries4))
	r.table6.Store(newRangeTable(entries6))
	return r
}

func scaleTestDirOrSkip(b *testing.B) string {
	dir := os.Getenv("ASNLOOKUP_SCALE_TEST_DIR")
	if dir == "" {
		b.Skip("ASNLOOKUP_SCALE_TEST_DIR not set - point it at a directory with real user-country-ipv4.csv/-ipv6.csv to run this")
	}
	return dir
}

// BenchmarkResolve_CacheHit measures a warm lookup: the same IP resolved
// repeatedly, always served from the LRU cache, never touching the range
// table after the first call.
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
// through to the binary search over the real ~550k-entry table rather
// than ever hitting the cache.
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
