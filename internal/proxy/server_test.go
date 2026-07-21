package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/netip"
	"regexp"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// generateSelfSignedCert creates an in-memory, stdlib-only self-signed TLS
// certificate for the test backend - no fixture files, no dependency on
// external tooling.
func generateSelfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}

	certPEM := pemEncode("CERTIFICATE", der)
	keyPEM := pemEncode("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(priv))
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	return cert
}

func pemEncode(blockType string, der []byte) []byte {
	var buf bytes.Buffer
	_ = pem.Encode(&buf, &pem.Block{Type: blockType, Bytes: der})
	return buf.Bytes()
}

// startEchoBackend starts a one-shot-per-connection echo server: it reads
// exactly msgLen bytes and writes them back. It reads with io.ReadFull
// rather than a single Read call because the proxy in front of it is a
// raw byte-stream passthrough - like any TCP link, it's free to deliver
// one logical message as multiple separate reads (its peek-then-replay
// mechanism for ClientHello sniffing makes that the common case, not just
// a theoretical one), and a correct TCP consumer can't assume otherwise.
func startEchoBackend(t *testing.T, ln net.Listener, msgLen int) {
	t.Helper()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, msgLen)
				if _, err := io.ReadFull(conn, buf); err != nil {
					return
				}
				conn.Write(buf)
			}()
		}
	}()
}

// startProxy wires a Server to backendAddr on an ephemeral port and serves
// it in the background until the test ends, returning the proxy's bound
// address.
func startProxy(t *testing.T, backendAddr string, store ratestore.RateStore) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	srv := &Server{
		BackendAddr:      backendAddr,
		Store:            store,
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

func TestServer_TLSPassthroughAndFingerprint(t *testing.T) {
	cert := generateSelfSignedCert(t)
	backendLn, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	defer backendLn.Close()

	want := []byte("hello through the proxy")
	startEchoBackend(t, backendLn, len(want))

	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()

	proxyAddr := startProxy(t, backendLn.Addr().String(), store)

	clientConn, err := tls.Dial("tcp", proxyAddr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("tls.Dial through proxy: %v", err)
	}
	defer clientConn.Close()

	if _, err := clientConn.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(clientConn, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("echoed data = %q, want %q", got, want)
	}

	// The TLS handshake (and thus RecordRequest, which happens before the
	// proxy starts piping bytes) has already completed by the time the
	// echo round-trip above succeeds, but poll briefly rather than assume
	// exact goroutine timing.
	var snaps []ratestore.Snapshot
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snaps = store.Snapshot(time.Time{}, time.Now())
		if len(snaps) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if len(snaps) != 1 {
		t.Fatalf("Snapshot = %+v, want exactly 1 tracked IP", snaps)
	}
	if want := netip.MustParseAddr("127.0.0.1"); snaps[0].IP != want {
		t.Errorf("recorded IP = %v, want %v", snaps[0].IP, want)
	}
	if !regexp.MustCompile(`^t\w{2}[di]\d{2}\d{2}\w{2}_[0-9a-f]{12}_[0-9a-f]{12}$`).MatchString(snaps[0].JA4) {
		t.Errorf("JA4 = %q, does not match expected shape for a real ClientHello", snaps[0].JA4)
	}
}

func TestServer_PlaintextPassthroughUnaffected(t *testing.T) {
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen backend: %v", err)
	}
	defer backendLn.Close()

	want := []byte("plain tcp hello, no tls here")
	startEchoBackend(t, backendLn, len(want))

	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()

	proxyAddr := startProxy(t, backendLn.Addr().String(), store)

	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("echoed data = %q, want %q (plaintext must pass through unmodified)", got, want)
	}
}
