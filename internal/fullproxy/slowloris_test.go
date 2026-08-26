package fullproxy

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// TestUnfinishedHandshakeIsClosed is the slowloris bound, measured.
//
// gosec flagged this server for having no ReadHeaderTimeout (G112), and
// unlike the open-redirect findings it flagged in the panel - which turned
// out to be false positives, since rawNext already rejects "//host" and
// "/\host" - this one was real, and worse than the rule's own description.
//
// net/http derives the TLS handshake deadline from the smallest non-zero
// of ReadHeaderTimeout, ReadTimeout and WriteTimeout, and applies none at
// all when every one of them is zero (net/http/server.go,
// tlsHandshakeTimeout, and its only caller in conn.serve). This server set
// none of the three. So the exposure was not merely slow *headers* after a
// completed handshake: a client could open a TCP connection, send one byte
// of a ClientHello, and hold a goroutine, a socket and up to
// maxSnoopBytes of capture buffer open indefinitely - no valid
// certificate, no request, no traffic required.
//
// That matters more here than the rule's generic severity suggests. This
// process exists to keep a site up under hostile load. A trivially
// available way to exhaust it from a single host contradicts the reason
// the customer installed it.
func TestUnfinishedHandshakeIsClosed(t *testing.T) {
	certFile, keyFile := writeCertKeyFiles(t)
	backend := startBackend(t)
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	srv := &Server{
		BackendAddr: backend.Listener.Addr().String(),
		CertFile:    certFile,
		KeyFile:     keyFile,
		Store:       store,
		DialTimeout: 2 * time.Second,
		// Short enough to keep the test quick; the default is 20s. That
		// the bound exists and is honoured is what's under test, not the
		// particular number.
		ReadHeaderTimeout: 300 * time.Millisecond,
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

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// One byte: a TLS handshake record type, and then nothing. A real
	// client would follow it with a length and a ClientHello. This one
	// never will.
	if _, err := conn.Write([]byte{0x16}); err != nil {
		t.Fatalf("write partial ClientHello: %v", err)
	}

	// The server must hang up on its own. The read blocks until it does;
	// the deadline is the assertion, generous enough not to be flaky but
	// far below "forever", which is what this used to be.
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	start := time.Now()
	_, err = io.Copy(io.Discard, conn)
	elapsed := time.Since(start)

	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("server never closed an unfinished TLS handshake after %v: a client can hold connections open indefinitely by sending a single byte", elapsed)
	}
	if err != nil {
		// Any other error means the peer went away, which is the point.
		// Distinguishing EOF from RST from "use of closed connection"
		// would only be asserting how it hung up, not that it did.
		t.Logf("connection ended with: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("handshake closed after %v, far beyond the %v bound: the deadline is not the thing ending it", elapsed, srv.ReadHeaderTimeout)
	}
	t.Logf("unfinished handshake closed after %v (ReadHeaderTimeout %v)", elapsed.Round(time.Millisecond), srv.ReadHeaderTimeout)
}
