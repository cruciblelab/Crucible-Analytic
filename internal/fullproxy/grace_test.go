package fullproxy

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// A request in flight when the server stops is given the grace and no
// more: Serve returns when it runs out, saying so. The passthrough proxy
// promises the same wait (proxy.DrainTimeout), and the test that ties the
// two constants means something only if each mode is held to its own.
func TestARequestInFlightIsGivenTheGraceAndNoMore(t *testing.T) {
	backend, arrived, release := startGateBackend(t)
	defer close(release)
	certFile, keyFile := writeCertKeyFiles(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	store := ratestore.NewMemoryRateStore(time.Minute, time.Minute, time.Minute)
	defer store.Close()
	const grace = 300 * time.Millisecond
	srv := &Server{BackendAddr: backend.Listener.Addr().String(), CertFile: certFile, KeyFile: keyFile,
		Store: store, DialTimeout: 2 * time.Second, shutdownGrace: grace}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	go func() {
		resp, err := newInsecureClient().Get("https://" + ln.Addr().String() + "/held")
		if err == nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("the held request never reached the backend")
	}

	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Serve returned %v; want the shutdown's deadline, since the request was still held", err)
		}
		if took := time.Since(start); took < grace-50*time.Millisecond {
			t.Errorf("Serve returned after %s, before the %s grace: the request was not waited for", took, grace)
		}
	case <-time.After(grace + 3*time.Second):
		t.Fatalf("Serve had not returned %s after a shutdown with a %s grace", grace+3*time.Second, grace)
	}
}
