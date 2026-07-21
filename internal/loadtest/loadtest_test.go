//go:build loadtest

// Real, at-scale concurrent load against a live proxy.Server - not the
// single/double-connection, tightly choreographed scenarios in
// internal/proxy/limiter_test.go, which prove each Decision path is
// wired correctly but don't exercise dozens of genuinely concurrent
// goroutines hammering the same Limiter at once. This package proves the
// same mechanism holds up under that kind of load, aggregating outcomes
// across many real TCP connections rather than asserting on one
// choreographed pair.
//
// internal/limiter is mode-agnostic - proxy.Server and fullproxy.Server
// both just call the same Limiter.Admit - so this exercises it through
// passthrough (plain TCP, cheaper to drive at real concurrency than a
// TLS or HTTP handshake per connection); internal/fullproxy's own tests
// already separately confirm the identical Limiter integrates correctly
// at HTTP-request granularity.
//
// Gated behind the "loadtest" build tag, like internal/storage's
// "integration" tag: real concurrent I/O at this scale is slower and
// more timing-sensitive than the default `go test -race ./...` suite
// should be. Run with:
//
//	go test -tags loadtest ./internal/loadtest/... -v -timeout 120s

package loadtest

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/limiter"
	"github.com/cruciblelab/crucible-analytic/internal/proxy"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// startEchoBackend starts a backend that echoes every byte it reads back
// to the client, full-duplex, until the client closes its side - it never
// closes a connection on its own, so a test controls exactly how long
// each connection stays "in flight" by choosing when to close it.
func startEchoBackend(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen backend: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				io.Copy(c, c)
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return ln
}

func startLoadTestProxy(t *testing.T, backendAddr string, store ratestore.RateStore, lim *limiter.Limiter) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen proxy: %v", err)
	}
	srv := &proxy.Server{
		BackendAddr: backendAddr,
		Store:       store,
		Limiter:     lim,
		// Short on purpose: every connection this package's tests send is
		// plain non-TLS bytes (one 'x'), so sniffClientHello's io.ReadFull
		// always blocks for the full HandshakeTimeout before giving up
		// and forwarding - a longer value here would inflate every
		// admitted connection's round-trip time well past what the
		// tests' own read deadlines below expect, for no benefit (this
		// package cares about Limiter behavior, not sniff timing).
		HandshakeTimeout: 50 * time.Millisecond,
		DialTimeout:      2 * time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return ln.Addr().String()
}

// fireConcurrentConnections dials proxyAddr n times, releasing all of
// them from a start barrier at once for maximum real concurrency, each
// writing one byte and trying to read it back within readTimeout before
// holding the connection open for holdOpen (simulating a client mid-
// request) and then closing. Returns how many got their byte echoed back.
func fireConcurrentConnections(t *testing.T, proxyAddr string, n int, holdOpen, readTimeout time.Duration) (succeeded int64) {
	t.Helper()
	var wg sync.WaitGroup
	var ok atomic.Int64
	start := make(chan struct{})

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
			if err != nil {
				return // connection-level rejection (fail_closed) can look like this too
			}
			defer conn.Close()

			if _, err := conn.Write([]byte{'x'}); err != nil {
				return
			}
			buf := make([]byte, 1)
			conn.SetReadDeadline(time.Now().Add(readTimeout))
			if _, err := io.ReadFull(conn, buf); err != nil {
				return
			}
			ok.Add(1)
			time.Sleep(holdOpen)
		}()
	}
	close(start)
	wg.Wait()
	return ok.Load()
}

func TestLoadTest_FailClosed_BoundsConcurrencyUnderRealLoad(t *testing.T) {
	backendLn := startEchoBackend(t)
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()
	lim := limiter.New(limiter.Config{MaxConcurrentConnections: 10, Policy: limiter.PolicyFailClosed})
	proxyAddr := startLoadTestProxy(t, backendLn.Addr().String(), store, lim)

	const attempts = 50
	succeeded := fireConcurrentConnections(t, proxyAddr, attempts, 500*time.Millisecond, time.Second)

	// Real scheduling means this won't land on exactly 10, but a hard
	// concurrency cap of 10 under 50 truly-simultaneous attempts should
	// keep it well clear of "most of them got through."
	if succeeded < 5 || succeeded > 20 {
		t.Errorf("succeeded = %d out of %d attempts, want roughly the 10-connection limit (allowing scheduling slack), not close to all %d", succeeded, attempts, attempts)
	}
	t.Logf("fail_closed: %d/%d succeeded with MaxConcurrentConnections=10", succeeded, attempts)
}

func TestLoadTest_FailOpen_StillProxiesEveryoneButUnderRecordsUnderRealLoad(t *testing.T) {
	backendLn := startEchoBackend(t)
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()
	lim := limiter.New(limiter.Config{MaxConcurrentConnections: 10, Policy: limiter.PolicyFailOpen})
	proxyAddr := startLoadTestProxy(t, backendLn.Addr().String(), store, lim)

	const attempts = 50
	succeeded := fireConcurrentConnections(t, proxyAddr, attempts, 500*time.Millisecond, 2*time.Second)

	if succeeded != attempts {
		t.Errorf("succeeded = %d out of %d, want all %d (fail_open must never refuse to proxy)", succeeded, attempts, attempts)
	}

	time.Sleep(100 * time.Millisecond) // let any last RecordRequest calls land
	snaps := store.Snapshot(time.Time{}, time.Now())
	var recorded int
	for _, s := range snaps {
		recorded += s.PrevWindowCount + s.CurrWindowCount
	}
	if recorded >= attempts {
		t.Errorf("recorded = %d out of %d proxied connections, want meaningfully fewer (degrade must skip RecordRequest for the excess even under real concurrent load)", recorded, attempts)
	}
	t.Logf("fail_open: %d/%d proxied, %d recorded, with MaxConcurrentConnections=10", succeeded, attempts, recorded)
}

func TestLoadTest_Throttle_EventuallyServesUpToQueueCapacityUnderRealLoad(t *testing.T) {
	backendLn := startEchoBackend(t)
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()
	lim := limiter.New(limiter.Config{MaxConcurrentConnections: 5, Policy: limiter.PolicyThrottle, ThrottleQueueSize: 10})
	proxyAddr := startLoadTestProxy(t, backendLn.Addr().String(), store, lim)

	const attempts = 30 // 5 concurrent + 10 queued = 15 should eventually succeed; 15 should be rejected
	succeeded := fireConcurrentConnections(t, proxyAddr, attempts, 100*time.Millisecond, 5*time.Second)

	if succeeded < 10 || succeeded > 20 {
		t.Errorf("succeeded = %d out of %d attempts, want roughly 15 (5 concurrent + 10 queued, with scheduling slack)", succeeded, attempts)
	}
	t.Logf("throttle: %d/%d eventually succeeded with MaxConcurrentConnections=5, ThrottleQueueSize=10", succeeded, attempts)
}

func TestLoadTest_MaxRequestsPerSecond_TripsUnderRealBurst(t *testing.T) {
	backendLn := startEchoBackend(t)
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()
	// Concurrency deliberately unbounded (1000, far above what this test
	// sends) so only the requests/second ceiling can be the bottleneck.
	lim := limiter.New(limiter.Config{MaxConcurrentConnections: 1000, MaxRequestsPerSecond: 20, Policy: limiter.PolicyFailClosed})
	proxyAddr := startLoadTestProxy(t, backendLn.Addr().String(), store, lim)

	const attempts = 100
	succeeded := fireConcurrentConnections(t, proxyAddr, attempts, 0, time.Second)

	if succeeded >= attempts {
		t.Errorf("succeeded = %d out of %d, want meaningfully fewer - a 20 req/s ceiling should reject a good fraction of 100 truly-concurrent attempts", succeeded, attempts)
	}
	if succeeded == 0 {
		t.Error("succeeded = 0, want at least the requests/second budget's worth to get through")
	}
	t.Logf("max_requests_per_second: %d/%d succeeded with MaxRequestsPerSecond=20", succeeded, attempts)
}
