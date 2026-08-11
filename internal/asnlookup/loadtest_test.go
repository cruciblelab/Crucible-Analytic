//go:build loadtest

// Real load-behavior verification for internal/asnlookup, gated behind
// the "loadtest" build tag like internal/loadtest's proxy/limiter tests -
// slower and more timing/scale-sensitive than the default suite should
// be, but no external dependency (unlike scale_test.go, which also needs
// a real downloaded dataset). Run with:
//
//	go test -tags loadtest ./internal/asnlookup/... -run TestLoadTest -v

package asnlookup

import (
	"context"
	"encoding/binary"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestLoadTest_TTLCache_HitRatioUnderRealisticAccessPattern drives the
// cache directly (not through Resolve) with a realistic traffic shape -
// a small hot set of repeat IPs plus a long tail of mostly-one-off ones,
// which is what real web traffic actually looks like, rather than
// uniform-random access (which would understate how effective an LRU
// actually is) - and reports the real, measured hit ratio.
func TestLoadTest_TTLCache_HitRatioUnderRealisticAccessPattern(t *testing.T) {
	const (
		cacheSize     = 1000
		uniqueIPs     = 5000
		hotIPs        = 50
		totalRequests = 100_000
	)

	cache := newTTLCache(cacheSize, time.Hour) // long TTL: capacity/LRU eviction is what's under test here, not expiry

	pool := make([]netip.Addr, uniqueIPs)
	for i := range pool {
		var raw [4]byte
		binary.BigEndian.PutUint32(raw[:], uint32(i)+1)
		pool[i] = netip.AddrFrom4(raw)
	}

	rng := rand.New(rand.NewSource(1))
	var hits, misses int
	for i := 0; i < totalRequests; i++ {
		var ip netip.Addr
		if rng.Intn(100) < 80 { // 80% of requests land on the 50-IP hot set
			ip = pool[rng.Intn(hotIPs)]
		} else { // 20% spread across the remaining, much larger long tail
			ip = pool[hotIPs+rng.Intn(uniqueIPs-hotIPs)]
		}

		if _, ok := cache.get(ip); ok {
			hits++
		} else {
			misses++
			cache.set(ip, Result{IP: ip, Found: true})
		}
	}

	hitRatio := float64(hits) / float64(totalRequests)
	t.Logf("requests=%d hits=%d misses=%d hit_ratio=%.2f%% cache_len=%d (capacity=%d)",
		totalRequests, hits, misses, hitRatio*100, cache.len(), cacheSize)

	if hitRatio < 0.5 {
		t.Errorf("hit ratio = %.2f%%, want at least 50%% for an 80/20 hot-set access pattern against a %d-entry cache", hitRatio*100, cacheSize)
	}
	if cache.len() > cacheSize {
		t.Errorf("cache.len() = %d, want <= %d - capacity must be enforced even under sustained real load", cache.len(), cacheSize)
	}
}

// TestLoadTest_LocalCSVPath_NeverContactsNetwork is a stronger version of
// TestLoadCountryCSV_LocalPathReadsFromDiskInsteadOfNetwork: rather than a
// nil http.Client (which only proves a crash didn't happen if the network
// path were taken), this points httpClient at a real, running
// httptest.Server configured to fail the test outright if it ever
// receives a single request, and drives loadCountryCSV and loadASNCSV for
// both address families each - not just one dataset/family. (writeCountryRanges/
// writeASNRanges/refresh's DB-persisting tail is intentionally not
// exercised here - that needs a live TimescaleDB, see integration_test.go
// - this test is specifically about the data-loading path local_csv_path
// affects, which doesn't touch the database at all.)
func TestLoadTest_LocalCSVPath_NeverContactsNetwork(t *testing.T) {
	contacted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted = true
		t.Errorf("network server was contacted (%s %s) - local_csv_path must never touch the network", r.Method, r.URL)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, countryIPv4Filename), []byte("192.0.2.0,192.0.2.255,US\n"), 0o644); err != nil {
		t.Fatalf("writing country ipv4 fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, countryIPv6Filename), []byte("2001:db8::,2001:db8::ffff,JP\n"), 0o644); err != nil {
		t.Fatalf("writing country ipv6 fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, asnIPv4Filename), []byte("192.0.2.0,192.0.2.255,64512,Example Org\n"), 0o644); err != nil {
		t.Fatalf("writing asn ipv4 fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, asnIPv6Filename), []byte("2001:db8::,2001:db8::ffff,64513,Example Org JP\n"), 0o644); err != nil {
		t.Fatalf("writing asn ipv6 fixture: %v", err)
	}

	r := &Resolver{
		httpClient:   srv.Client(), // reachable and would happily serve a request - the point is that it's never asked to
		localCSVPath: dir,
	}
	entries4, err := r.loadCountryCSV(context.Background(), countryIPv4URL, countryIPv4Filename)
	if err != nil {
		t.Fatalf("loadCountryCSV(ipv4): %v", err)
	}
	entries6, err := r.loadCountryCSV(context.Background(), countryIPv6URL, countryIPv6Filename)
	if err != nil {
		t.Fatalf("loadCountryCSV(ipv6): %v", err)
	}
	asnEntries4, err := r.loadASNCSV(context.Background(), asnIPv4URL, asnIPv4Filename)
	if err != nil {
		t.Fatalf("loadASNCSV(ipv4): %v", err)
	}
	asnEntries6, err := r.loadASNCSV(context.Background(), asnIPv6URL, asnIPv6Filename)
	if err != nil {
		t.Fatalf("loadASNCSV(ipv6): %v", err)
	}

	if contacted {
		t.Fatal("loadCountryCSV/loadASNCSV contacted the network server despite local_csv_path being set")
	}
	if len(entries4) != 1 || entries4[0].value != "US" {
		t.Errorf("country ipv4 entries = %+v, want one US entry", entries4)
	}
	if len(entries6) != 1 || entries6[0].value != "JP" {
		t.Errorf("country ipv6 entries = %+v, want one JP entry", entries6)
	}
	if len(asnEntries4) != 1 || asnEntries4[0].value.asn != 64512 || asnEntries4[0].value.org != "Example Org" {
		t.Errorf("asn ipv4 entries = %+v, want one AS64512 Example Org entry", asnEntries4)
	}
	if len(asnEntries6) != 1 || asnEntries6[0].value.asn != 64513 || asnEntries6[0].value.org != "Example Org JP" {
		t.Errorf("asn ipv6 entries = %+v, want one AS64513 Example Org JP entry", asnEntries6)
	}
}

// TestLoadTest_NewResolverAloneStartsNoBackgroundActivity proves
// asn_lookup.enabled = false costs nothing at runtime beyond main.go's
// own "don't call NewResolver at all" branch (already visible by
// inspection): even if a Resolver value exists but Run is never called
// on it (the shape main.go leaves it in when disabled - it simply never
// gets constructed), it has started zero goroutines of its own. Run
// itself is measured to add exactly one and clean it back up on
// cancellation, with no leak.
//
// localCSVPath points at a directory that doesn't exist, so refresh's
// immediate first call fails to load either dataset, either family, and
// returns before ever touching the database - letting this test exercise
// Run's full goroutine lifecycle without needing a live TimescaleDB (see
// integration_test.go for the DB-write path itself).
func TestLoadTest_NewResolverAloneStartsNoBackgroundActivity(t *testing.T) {
	before := runtime.NumGoroutine()

	r := &Resolver{cache: newTTLCache(100, time.Hour), localCSVPath: t.TempDir() + "/does-not-exist"}
	runtime.KeepAlive(r)

	afterConstruct := runtime.NumGoroutine()
	if afterConstruct != before {
		t.Errorf("goroutines after constructing a Resolver without calling Run = %d, want unchanged from %d", afterConstruct, before)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.Run(ctx, time.Hour) // long interval - only the immediate first refresh should fire during this test
	}()
	waitForGoroutineCount(t, before+1, time.Second)

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return within 1s of context cancellation")
	}
	waitForGoroutineCount(t, before, time.Second)
}

func waitForGoroutineCount(t *testing.T, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if got := runtime.NumGoroutine(); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine count = %d, want %d (waited %v)", runtime.NumGoroutine(), want, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
