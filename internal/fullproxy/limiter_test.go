package fullproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/limiter"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// startGateBackend starts a plaintext HTTP backend where a request to
// "/held" sends on arrived (so a test can deterministically learn the
// request actually reached the backend - i.e. was admitted and proxied -
// before moving on) and then blocks until release is signaled. Any other
// path responds immediately, echoing the path like startBackend.
func startGateBackend(t *testing.T) (srv *httptest.Server, arrived, release chan struct{}) {
	t.Helper()
	arrived = make(chan struct{})
	release = make(chan struct{})
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/held" {
			arrived <- struct{}{}
			<-release
			fmt.Fprint(w, "held done")
			return
		}
		fmt.Fprintf(w, "backend saw %s", r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv, arrived, release
}

// startFullProxyWithLimiter mirrors startFullProxy but also wires a
// Limiter, for tests specifically exercising overload behavior.
func startFullProxyWithLimiter(t *testing.T, backendAddr, certFile, keyFile string, store ratestore.RateStore, lim *limiter.Limiter) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	srv := &Server{
		BackendAddr: backendAddr,
		CertFile:    certFile,
		KeyFile:     keyFile,
		Store:       store,
		Limiter:     lim,
		DialTimeout: 2 * time.Second,
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
	backend, arrived, release := startGateBackend(t)
	certFile, keyFile := writeCertKeyFiles(t)
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()
	lim := limiter.New(limiter.Config{MaxConcurrentConnections: 1, Policy: limiter.PolicyFailClosed})

	proxyAddr := startFullProxyWithLimiter(t, backend.Listener.Addr().String(), certFile, keyFile, store, lim)
	client := newInsecureClient()

	req1Done := make(chan *http.Response, 1)
	go func() {
		resp, err := client.Get(fmt.Sprintf("https://%s/held", proxyAddr))
		if err != nil {
			t.Errorf("req1: %v", err)
			req1Done <- nil
			return
		}
		req1Done <- resp
	}()

	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("req1 never reached the backend")
	}

	// req1 is now admitted and in flight, holding the only concurrency
	// slot - req2 must be rejected outright, with no backend round trip.
	resp2, err := client.Get(fmt.Sprintf("https://%s/other", proxyAddr))
	if err != nil {
		t.Fatalf("req2: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("req2 status = %d, want %d (fail_closed should reject over the concurrency limit)", resp2.StatusCode, http.StatusServiceUnavailable)
	}

	release <- struct{}{}
	resp1 := <-req1Done
	if resp1 == nil {
		t.Fatal("req1 failed")
	}
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Errorf("req1 status = %d, want 200", resp1.StatusCode)
	}
}

func TestServer_FailOpen_DegradesButStillProxiesWithoutRecording(t *testing.T) {
	backend, arrived, release := startGateBackend(t)
	certFile, keyFile := writeCertKeyFiles(t)
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()
	lim := limiter.New(limiter.Config{MaxConcurrentConnections: 1, Policy: limiter.PolicyFailOpen})

	proxyAddr := startFullProxyWithLimiter(t, backend.Listener.Addr().String(), certFile, keyFile, store, lim)
	client := newInsecureClient()

	req1Done := make(chan *http.Response, 1)
	go func() {
		resp, err := client.Get(fmt.Sprintf("https://%s/held", proxyAddr))
		if err != nil {
			t.Errorf("req1: %v", err)
			req1Done <- nil
			return
		}
		req1Done <- resp
	}()

	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("req1 never reached the backend")
	}

	// req1 has already been recorded by the time its handler reached the
	// backend (recordingHandler records before calling next), so this is
	// a stable baseline to compare against.
	baseline := store.Snapshot(time.Time{}, time.Now())
	if len(baseline) != 1 {
		t.Fatalf("baseline snapshot = %+v, want exactly 1 entry (req1)", baseline)
	}
	baseCount := baseline[0].PrevWindowCount + baseline[0].CurrWindowCount

	// req2 arrives while req1 still holds the only slot: fail_open means
	// it must still be proxied through to the backend and get a normal
	// response...
	resp2, err := client.Get(fmt.Sprintf("https://%s/other", proxyAddr))
	if err != nil {
		t.Fatalf("req2: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("req2 status = %d, want 200 (a degraded request should still be proxied)", resp2.StatusCode)
	}
	body, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatalf("req2: reading body: %v", err)
	}
	if want := "backend saw /other"; string(body) != want {
		t.Errorf("req2 body = %q, want %q", body, want)
	}

	// ...but must not have added a second recorded request.
	final := store.Snapshot(time.Time{}, time.Now())
	if len(final) != 1 {
		t.Fatalf("final snapshot = %+v, want exactly 1 tracked IP (127.0.0.1, both requests)", final)
	}
	finalCount := final[0].PrevWindowCount + final[0].CurrWindowCount
	if finalCount != baseCount {
		t.Errorf("recorded count went from %d to %d after req2 (a degraded request); want unchanged - degrade must skip RecordRequest", baseCount, finalCount)
	}

	release <- struct{}{}
	resp1 := <-req1Done
	if resp1 == nil {
		t.Fatal("req1 failed")
	}
	defer resp1.Body.Close()
}

func TestServer_Throttle_QueuesThenProceedsWhenSlotFrees(t *testing.T) {
	backend, arrived, release := startGateBackend(t)
	certFile, keyFile := writeCertKeyFiles(t)
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()
	lim := limiter.New(limiter.Config{MaxConcurrentConnections: 1, Policy: limiter.PolicyThrottle, ThrottleQueueSize: 5})

	proxyAddr := startFullProxyWithLimiter(t, backend.Listener.Addr().String(), certFile, keyFile, store, lim)
	client := newInsecureClient()

	req1Done := make(chan *http.Response, 1)
	go func() {
		resp, err := client.Get(fmt.Sprintf("https://%s/held", proxyAddr))
		if err != nil {
			t.Errorf("req1: %v", err)
			req1Done <- nil
			return
		}
		req1Done <- resp
	}()

	select {
	case <-arrived:
	case <-time.After(2 * time.Second):
		t.Fatal("req1 never reached the backend")
	}

	req2Done := make(chan *http.Response, 1)
	go func() {
		resp, err := client.Get(fmt.Sprintf("https://%s/other", proxyAddr))
		if err != nil {
			t.Errorf("req2: %v", err)
			req2Done <- nil
			return
		}
		req2Done <- resp
	}()

	// req2 must not complete yet - req1 still holds the only slot, so
	// req2 should be queued inside Admit, not immediately rejected or
	// served.
	select {
	case <-req2Done:
		t.Fatal("req2 completed before req1's slot was released; want it queued until then")
	case <-time.After(200 * time.Millisecond):
	}

	release <- struct{}{} // frees req1's slot; req2 should now be admitted
	resp1 := <-req1Done
	if resp1 == nil {
		t.Fatal("req1 failed")
	}
	defer resp1.Body.Close()

	var resp2 *http.Response
	select {
	case resp2 = <-req2Done:
	case <-time.After(2 * time.Second):
		t.Fatal("req2 never completed after req1's slot was released")
	}
	if resp2 == nil {
		t.Fatal("req2 failed")
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("req2 status = %d, want 200", resp2.StatusCode)
	}
	body, err := io.ReadAll(resp2.Body)
	if err != nil {
		t.Fatalf("req2: reading body: %v", err)
	}
	if want := "backend saw /other"; string(body) != want {
		t.Errorf("req2 body = %q, want %q", body, want)
	}
}
