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
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/asnlookup"
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

// --- A5.2: the blocklist changing while traffic is flowing ---

// fixedResolver reports the same country for every address, which is
// what lets a load test decide whether a connection is blocked without
// standing up real range tables.
type fixedResolver struct{ country string }

func (r fixedResolver) Resolve(netip.Addr) asnlookup.Result {
	return asnlookup.Result{Country: r.country, Found: true}
}

func startGeoLoadTestProxy(t *testing.T, backendAddr string, store ratestore.RateStore,
	blocklist *limiter.GeoBlocklist, resolver proxy.Resolver) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen proxy: %v", err)
	}
	srv := &proxy.Server{
		BackendAddr:      backendAddr,
		Store:            store,
		GeoBlocklist:     blocklist,
		Resolver:         resolver,
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

// phaseCounts tallies connection attempts per phase of a stream.
//
// An attempt counts for a phase only if that phase was in force both
// when the dial began and when the result came back; one that straddles
// a change is counted separately and asserted about by neither side.
// That is not bookkeeping fussiness, it is the difference between a
// real result and a false one: the first run of this test reported 3
// connections "served while blocked" out of 9803, and all three were
// attempts tagged when the block was on that dialed after it had been
// lifted. Attributing those to the blocked phase would have reported a
// leak in the blocklist that did not exist.
//
// The reverse - blaming the blocklist for a connection that was already
// past the check when the rules changed - is excluded by the same rule,
// and that one would be asserting that an atomic store reaches
// backwards in time.
//
// The straddled count is worth reading when this test is run: it lands
// on exactly workers x changes (16 for 8 workers across 2 changes), one
// in-flight attempt per worker per change, which is what it should be
// and is itself a check that the brackets are doing what they claim.
type phaseCounts struct {
	attempted [3]atomic.Int64
	served    [3]atomic.Int64
	straddled atomic.Int64
}

// streamConnections keeps a steady stream of real connections flowing to
// proxyAddr from workers goroutines until stop is closed, recording each
// under whichever phase was current at dial time.
//
// Unlike fireConcurrentConnections, which fires one all-at-once burst
// and then stops, this keeps dialing - which is what makes it possible
// to change something on the server and prove that traffic *already in
// flight* saw the change, rather than proving only that a later burst
// did.
func streamConnections(proxyAddr string, workers int, phase *atomic.Int64,
	counts *phaseCounts, stop <-chan struct{}) {
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}

				// Bracket the attempt: it belongs to a phase only if the
				// same phase was in force at both ends.
				began := phase.Load()
				served := connectionServed(proxyAddr, 2*time.Second)
				if ended := phase.Load(); ended != began {
					counts.straddled.Add(1)
					continue
				}
				counts.attempted[began].Add(1)
				if served {
					counts.served[began].Add(1)
				}
			}
		}()
	}
	wg.Wait()
}

// connectionServed dials, writes one byte and reports whether the echo
// backend sent it back - the same success test fireConcurrentConnections
// applies, factored out so the streaming loop can share it.
func connectionServed(proxyAddr string, readTimeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{'x'}); err != nil {
		return false
	}
	buf := make([]byte, 1)
	conn.SetReadDeadline(time.Now().Add(readTimeout))
	_, err = io.ReadFull(conn, buf)
	return err == nil
}

// TestLoadTest_BlocklistChangesWhileConnectionsAreFlowing is the phase.
//
// A5.2 exists so that "we are being hit from there, block it" does not
// mean SSH, an edit and a restart - the longest possible path while an
// attack is in progress. Everything else in this phase is a registry
// entry and a setter; this is the only test that shows the thing
// actually works, and it has to run against real concurrent TCP rather
// than a unit-test pair.
//
// Three phases against one server that is never restarted, with a real
// stream of connections running unbroken across both changes: nothing
// blocked, the country added, then removed again. The removal matters as
// much as the addition - a switch that only goes one way is a customer
// locked out of their own site until somebody restarts the collector.
//
// # Why a continuous stream rather than three bursts
//
// This started as three separate bursts with the change made between
// them, and it passed - but it proved less than it looked like it did.
// The burst racing the change reported zero connections served, which is
// equally consistent with "the block took effect instantly" and with
// "the goroutine was scheduled after Set returned and no connection was
// ever in flight during the change at all". A test that passes the same
// way whether or not the interesting thing happened is not evidence.
// Streaming instead, and asserting that each phase actually carried
// traffic before asserting what happened to it, removes that reading.
func TestLoadTest_BlocklistChangesWhileConnectionsAreFlowing(t *testing.T) {
	backendLn := startEchoBackend(t)
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()

	// Empty at first, exactly as a deployment that blocks nothing starts.
	blocklist := limiter.NewGeoBlocklist(nil, nil)
	if blocklist.Active() {
		t.Fatal("an empty blocklist reports active")
	}
	proxyAddr := startGeoLoadTestProxy(t, backendLn.Addr().String(), store,
		blocklist, fixedResolver{country: "CN"})

	const (
		open    = 0 // nothing blocked
		blocked = 1 // CN on the list
		lifted  = 2 // taken off again
	)
	var phase atomic.Int64
	counts := &phaseCounts{}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		streamConnections(proxyAddr, 8, &phase, counts, stop)
	}()

	// Long enough for several rounds per worker at the proxy's 50ms
	// sniff timeout, short enough that the whole test stays about a
	// second.
	const settle = 400 * time.Millisecond
	time.Sleep(settle)

	// The change, made the way the settings poll makes it: on a live
	// server, with connections in flight. phase moves only after Set has
	// returned, so everything tagged "blocked" is guaranteed to have hit
	// the check with the new rules already published.
	blocklist.Set([]string{"CN"}, nil)
	phase.Store(blocked)
	if !blocklist.Active() {
		t.Fatal("the blocklist reports inactive after a country was added")
	}
	time.Sleep(settle)

	// And back off again, on the same running server.
	blocklist.Set(nil, nil)
	phase.Store(lifted)
	if blocklist.Active() {
		t.Error("the blocklist still reports active after being cleared")
	}
	time.Sleep(settle)

	close(stop)
	<-done

	for _, p := range []struct {
		i    int
		name string
	}{{open, "before the block"}, {blocked, "while blocked"}, {lifted, "after the block was lifted"}} {
		if counts.attempted[p.i].Load() == 0 {
			t.Fatalf("no connections were even attempted %s; the stream stalled, so this test "+
				"proves nothing about what the blocklist did", p.name)
		}
	}

	if served := counts.served[open].Load(); served == 0 {
		t.Fatalf("0 of %d connections got through with nothing blocked; the harness is wrong, "+
			"not the blocklist", counts.attempted[open].Load())
	}
	if served := counts.served[blocked].Load(); served != 0 {
		t.Errorf("%d of %d connections dialed after the country was blocked were still served; "+
			"the change did not take effect on a running server",
			served, counts.attempted[blocked].Load())
	}
	if served := counts.served[lifted].Load(); served == 0 {
		t.Errorf("0 of %d connections were served after the block was lifted; a switch that "+
			"only goes one way locks a customer out until somebody restarts the collector",
			counts.attempted[lifted].Load())
	}

	t.Logf("served/attempted: %d/%d open, %d/%d blocked, %d/%d lifted (%d straddled a change)",
		counts.served[open].Load(), counts.attempted[open].Load(),
		counts.served[blocked].Load(), counts.attempted[blocked].Load(),
		counts.served[lifted].Load(), counts.attempted[lifted].Load(),
		counts.straddled.Load())
}
