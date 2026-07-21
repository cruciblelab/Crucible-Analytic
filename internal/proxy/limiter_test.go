package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/limiter"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// startProxyWithLimiter mirrors startProxy but also wires a Limiter, for
// tests specifically exercising overload behavior.
func startProxyWithLimiter(t *testing.T, backendAddr string, store ratestore.RateStore, lim *limiter.Limiter) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	srv := &Server{
		BackendAddr:      backendAddr,
		Store:            store,
		Limiter:          lim,
		HandshakeTimeout: 2 * time.Second,
		DialTimeout:      2 * time.Second,
	}

	ctx, cancel := context.WithCancel(context.Background())
	serveErrCh := make(chan error, 1)
	go func() { serveErrCh <- srv.Serve(ctx, ln) }()

	t.Cleanup(func() {
		cancel()
		if err := <-serveErrCh; err != nil {
			t.Errorf("Serve returned error after shutdown: %v", err)
		}
	})

	return ln.Addr().String()
}

func TestServer_FailClosed_RejectsOverConcurrencyLimit(t *testing.T) {
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen backend: %v", err)
	}
	defer backendLn.Close()
	startEchoBackend(t, backendLn, 1024) // never actually reached by conn1's 1 byte

	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()
	lim := limiter.New(limiter.Config{MaxConcurrentConnections: 1, Policy: limiter.PolicyFailClosed})

	proxyAddr := startProxyWithLimiter(t, backendLn.Addr().String(), store, lim)

	conn1, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("Dial 1: %v", err)
	}
	defer conn1.Close()
	if _, err := conn1.Write([]byte("x")); err != nil { // proves conn1 is actually flowing
		t.Fatalf("conn1 Write: %v", err)
	}
	time.Sleep(150 * time.Millisecond) // let conn1 actually occupy the one slot

	conn2, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("Dial 2: %v", err)
	}
	defer conn2.Close()

	conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	n, err := conn2.Read(buf)
	if n != 0 || err == nil {
		t.Errorf("conn2 Read = (%d bytes, err=%v), want (0, an error) - fail_closed should have closed it immediately", n, err)
	}
}

func TestServer_FailOpen_DegradesButStillProxiesWithoutRecording(t *testing.T) {
	cert := generateSelfSignedCert(t)
	backendLn, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer backendLn.Close()

	want := []byte("second connection data")
	startEchoBackend(t, backendLn, len(want))

	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()
	lim := limiter.New(limiter.Config{MaxConcurrentConnections: 1, Policy: limiter.PolicyFailOpen})

	proxyAddr := startProxyWithLimiter(t, backendLn.Addr().String(), store, lim)

	// conn1: a real TLS handshake, admitted normally (nothing is over
	// limit yet), so it's recorded quickly and deterministically - then
	// held open (no more data either way) to keep occupying the
	// collector's one concurrency slot for the rest of the test.
	conn1, err := tls.Dial("tcp", proxyAddr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls.Dial 1: %v", err)
	}
	defer conn1.Close()

	deadline := time.Now().Add(2 * time.Second)
	var baseline []ratestore.Snapshot
	for {
		baseline = store.Snapshot(time.Time{}, time.Now())
		if len(baseline) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("conn1 was never recorded")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(baseline) != 1 {
		t.Fatalf("baseline snapshot (conn1 only) = %+v, want exactly 1 entry", baseline)
	}
	baseCount := baseline[0].PrevWindowCount + baseline[0].CurrWindowCount

	// conn2 arrives while conn1 still holds the only slot: fail_open means
	// it must still be proxied to the backend transparently. Degrade mode
	// splices raw bytes without peeking at them, so conn2 performs its own
	// real TLS handshake straight through to the backend (indistinguishable
	// from conn1's, from the wire's perspective) rather than sending
	// plaintext, which this TLS-terminating backend would otherwise reject
	// as an invalid record.
	conn2, err := tls.Dial("tcp", proxyAddr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls.Dial 2: %v", err)
	}
	defer conn2.Close()
	if _, err := conn2.Write(want); err != nil {
		t.Fatalf("conn2 Write: %v", err)
	}
	got := make([]byte, len(want))
	conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn2, got); err != nil {
		t.Fatalf("conn2 ReadFull (a degraded connection should still be proxied): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("conn2 echoed = %q, want %q", got, want)
	}

	// ...but must not have added a second recorded request.
	time.Sleep(100 * time.Millisecond)
	final := store.Snapshot(time.Time{}, time.Now())
	if len(final) != 1 {
		t.Fatalf("final snapshot = %+v, want exactly 1 tracked IP (127.0.0.1, both connections)", final)
	}
	finalCount := final[0].PrevWindowCount + final[0].CurrWindowCount
	if finalCount != baseCount {
		t.Errorf("recorded count went from %d to %d after conn2 (a degraded connection); want unchanged - degrade must skip RecordRequest", baseCount, finalCount)
	}
}

func TestServer_Throttle_QueuesThenProceedsWhenSlotFrees(t *testing.T) {
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen backend: %v", err)
	}
	defer backendLn.Close()

	want := []byte("queued connection data")
	startEchoBackend(t, backendLn, len(want))

	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()
	lim := limiter.New(limiter.Config{MaxConcurrentConnections: 1, Policy: limiter.PolicyThrottle, ThrottleQueueSize: 5})

	proxyAddr := startProxyWithLimiter(t, backendLn.Addr().String(), store, lim)

	// conn1 occupies the only slot: it never sends its full message, so
	// the backend's io.ReadFull blocks forever and the slot stays held.
	conn1, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("Dial 1: %v", err)
	}
	time.Sleep(150 * time.Millisecond)

	conn2, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("Dial 2: %v", err)
	}
	defer conn2.Close()
	if _, err := conn2.Write(want); err != nil {
		t.Fatalf("conn2 Write: %v", err)
	}

	// conn2 must not complete yet - conn1 still holds the only slot, so
	// conn2 should be queued (blocked), not immediately rejected or served.
	conn2.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	premature := make([]byte, len(want))
	if _, err := io.ReadFull(conn2, premature); err == nil {
		t.Fatal("conn2 completed before conn1's slot was released; want it queued until then")
	}

	conn1.Close() // frees the slot; conn2 should now be admitted and proxied

	got := make([]byte, len(want))
	conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(conn2, got); err != nil {
		t.Fatalf("conn2 ReadFull after conn1 released its slot: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("conn2 echoed = %q, want %q", got, want)
	}
}
