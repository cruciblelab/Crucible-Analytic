package fullproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/asnlookup"
	"github.com/cruciblelab/crucible-analytic/internal/limiter"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// fakeResolver returns a canned asnlookup.Result for every IP, regardless
// of what's asked - enough to test GeoBlocklist wiring without needing a
// real loaded range table.
type fakeResolver struct {
	result asnlookup.Result
}

func (f fakeResolver) Resolve(ip netip.Addr) asnlookup.Result {
	return f.result
}

// writeCertKeyFiles generates an in-memory, stdlib-only self-signed TLS
// certificate and writes it to two temp files, the way Server expects to
// load a real backend certificate.
func writeCertKeyFiles(t *testing.T) (certFile, keyFile string) {
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

	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pemEncode("CERTIFICATE", der), 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, pemEncode("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(priv)), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certFile, keyFile
}

func pemEncode(blockType string, der []byte) []byte {
	var buf bytes.Buffer
	_ = pem.Encode(&buf, &pem.Block{Type: blockType, Bytes: der})
	return buf.Bytes()
}

// startBackend starts a plaintext HTTP backend that echoes the request
// path, so responses can be tied back to individual requests.
func startBackend(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "backend saw %s", r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// startFullProxy wires a Server to backendAddr on an ephemeral port and
// serves it in the background until the test ends, returning the bound
// address.
func startFullProxy(t *testing.T, backendAddr, certFile, keyFile string, store ratestore.RateStore) string {
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

func newInsecureClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			// http.Transport is conservative about auto-negotiating HTTP/2:
			// setting a custom TLSClientConfig (needed here for
			// InsecureSkipVerify, since the test uses a self-signed cert)
			// disables the automatic upgrade unless this is also set
			// explicitly (see transport.go's protocols(), which otherwise
			// falls through to HTTP/1-only precisely because
			// TLSClientConfig != nil).
			ForceAttemptHTTP2: true,
		},
		Timeout: 5 * time.Second,
	}
}

// startFullProxyWithGeoBlock is startFullProxy plus GeoBlocklist/Resolver,
// for the geo-blocking tests below - kept separate from startFullProxy
// rather than adding optional params to it, since only these two tests
// need it.
func startFullProxyWithGeoBlock(t *testing.T, backendAddr, certFile, keyFile string, store ratestore.RateStore, blocklist *limiter.GeoBlocklist, resolver Resolver) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	srv := &Server{
		BackendAddr:  backendAddr,
		CertFile:     certFile,
		KeyFile:      keyFile,
		Store:        store,
		DialTimeout:  2 * time.Second,
		GeoBlocklist: blocklist,
		Resolver:     resolver,
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

func TestServer_GeoBlockedRequestGets403AndIsNotRecorded(t *testing.T) {
	backend := startBackend(t)
	certFile, keyFile := writeCertKeyFiles(t)
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()

	blocklist := limiter.NewGeoBlocklist([]string{"CN"}, nil)
	resolver := fakeResolver{result: asnlookup.Result{Country: "CN", Found: true}}
	proxyAddr := startFullProxyWithGeoBlock(t, backend.Listener.Addr().String(), certFile, keyFile, store, blocklist, resolver)

	client := newInsecureClient()
	resp, err := client.Get(fmt.Sprintf("https://%s/blocked", proxyAddr))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want %d (Forbidden)", resp.StatusCode, http.StatusForbidden)
	}

	snaps := store.Snapshot(time.Time{}, time.Now())
	if len(snaps) != 0 {
		t.Errorf("RateStore has %d entries, want 0 - a geo-blocked request must never reach RecordRequest", len(snaps))
	}
}

func TestServer_NonMatchingGeoBlocklistStillProxiesNormally(t *testing.T) {
	backend := startBackend(t)
	certFile, keyFile := writeCertKeyFiles(t)
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()

	// A real blocklist is configured, it just doesn't match this
	// request's (fake-resolved) country - proves GeoBlocklist being
	// non-nil doesn't itself change behavior, only an actual match does.
	blocklist := limiter.NewGeoBlocklist([]string{"CN"}, nil)
	resolver := fakeResolver{result: asnlookup.Result{Country: "US", Found: true}}
	proxyAddr := startFullProxyWithGeoBlock(t, backend.Listener.Addr().String(), certFile, keyFile, store, blocklist, resolver)

	client := newInsecureClient()
	resp, err := client.Get(fmt.Sprintf("https://%s/not-blocked", proxyAddr))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if want := "backend saw /not-blocked"; string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}

	snaps := store.Snapshot(time.Time{}, time.Now())
	if len(snaps) != 1 {
		t.Errorf("RateStore has %d entries, want 1 (the non-blocked request should still be recorded)", len(snaps))
	}
}

func TestServer_ProxiesAndCountsPerRequestNotPerConnection(t *testing.T) {
	backend := startBackend(t)
	certFile, keyFile := writeCertKeyFiles(t)
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()

	proxyAddr := startFullProxy(t, backend.Listener.Addr().String(), certFile, keyFile, store)

	client := newInsecureClient()
	const numRequests = 5
	for i := 0; i < numRequests; i++ {
		resp, err := client.Get(fmt.Sprintf("https://%s/path-%d", proxyAddr, i))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("request %d: reading body: %v", i, err)
		}
		want := fmt.Sprintf("backend saw /path-%d", i)
		if string(body) != want {
			t.Errorf("request %d body = %q, want %q", i, body, want)
		}
	}

	snaps := store.Snapshot(time.Time{}, time.Now())
	if len(snaps) != 1 {
		t.Fatalf("Snapshot = %+v, want exactly 1 tracked IP", snaps)
	}
	got := snaps[0].PrevWindowCount + snaps[0].CurrWindowCount
	if got != numRequests {
		t.Errorf("recorded request count = %d, want %d (one RecordRequest per HTTP request, not per TCP connection)", got, numRequests)
	}
	if want := netip.MustParseAddr("127.0.0.1"); snaps[0].IP != want {
		t.Errorf("recorded IP = %v, want %v", snaps[0].IP, want)
	}
	if !regexp.MustCompile(`^t\w{2}[di]\d{2}\d{2}\w{2}_[0-9a-f]{12}_[0-9a-f]{12}$`).MatchString(snaps[0].JA4) {
		t.Errorf("JA4 = %q, does not match expected shape for a real ClientHello", snaps[0].JA4)
	}
}

func TestServer_NegotiatesHTTP2AndStillCountsPerRequest(t *testing.T) {
	backend := startBackend(t)
	certFile, keyFile := writeCertKeyFiles(t)
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()

	proxyAddr := startFullProxy(t, backend.Listener.Addr().String(), certFile, keyFile, store)

	client := newInsecureClient()
	var gotHTTP2 bool
	for i := 0; i < 3; i++ {
		resp, err := client.Get(fmt.Sprintf("https://%s/h2-%d", proxyAddr, i))
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.ProtoMajor == 2 {
			gotHTTP2 = true
		}
	}

	if !gotHTTP2 {
		t.Error("no request negotiated HTTP/2 (ProtoMajor == 2); expected net/http's automatic ALPN h2 support to kick in")
	}

	snaps := store.Snapshot(time.Time{}, time.Now())
	if len(snaps) != 1 || snaps[0].PrevWindowCount+snaps[0].CurrWindowCount != 3 {
		t.Errorf("Snapshot = %+v, want exactly 1 IP with 3 recorded requests", snaps)
	}
}

func TestServer_MissingCertFileFailsCleanly(t *testing.T) {
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	defer ln.Close()

	srv := &Server{
		BackendAddr: "127.0.0.1:1",
		CertFile:    "/nonexistent/cert.pem",
		KeyFile:     "/nonexistent/key.pem",
		Store:       store,
	}

	if err := srv.Serve(context.Background(), ln); err == nil {
		t.Error("Serve() error = nil, want an error for a missing cert file")
	}
}

func TestServer_ShutsDownCleanlyOnContextCancel(t *testing.T) {
	backend := startBackend(t)
	certFile, keyFile := writeCertKeyFiles(t)
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	srv := &Server{
		BackendAddr: backend.Listener.Addr().String(),
		CertFile:    certFile,
		KeyFile:     keyFile,
		Store:       store,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx, ln) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve() = %v, want nil after clean shutdown", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Serve did not return within 15s of context cancellation")
	}
}

// TestServer_WorksAgainstPlainHTTP11OnlyBackend explicitly verifies full
// mode against a backend that can only ever speak HTTP/1.1 - built
// directly from net.Listen + http.Server rather than httptest.NewServer,
// so that property is visible in this test's own code rather than an
// implicit fact about a helper a reader would have to already know. Go's
// http.Server has no built-in h2c (HTTP/2 over cleartext) support for a
// plain, non-TLS listener, and this project doesn't import
// golang.org/x/net/http2/h2c anywhere - so this backend is a real,
// unambiguous negative case for "does the collector's own Transport to
// the backend ever need or attempt h2c." It doesn't: reverseProxy's
// Transport in Serve (server.go) has no TLSClientConfig and talks to a
// plain "http://" URL, which net/http always speaks as HTTP/1.1 - there's
// no h2c code path in this codebase for it to take in the first place.
// If that ever changed and something did try to upgrade, this backend
// would fail the request rather than silently succeeding.
func TestServer_WorksAgainstPlainHTTP11OnlyBackend(t *testing.T) {
	backendLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen backend: %v", err)
	}
	backendSrv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ProtoMajor != 1 {
				t.Errorf("backend saw request ProtoMajor = %d, want 1 (HTTP/1.x)", r.ProtoMajor)
			}
			fmt.Fprintf(w, "backend saw %s", r.URL.Path)
		}),
	}
	go backendSrv.Serve(backendLn)
	defer backendSrv.Close()

	certFile, keyFile := writeCertKeyFiles(t)
	store := ratestore.NewMemoryRateStore(time.Minute, 5*time.Minute, time.Hour)
	defer store.Close()

	proxyAddr := startFullProxy(t, backendLn.Addr().String(), certFile, keyFile, store)

	client := newInsecureClient()
	resp, err := client.Get(fmt.Sprintf("https://%s/plain-http11", proxyAddr))
	if err != nil {
		t.Fatalf("request through full mode to an HTTP/1.1-only backend: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if want := "backend saw /plain-http11"; string(body) != want {
		t.Errorf("body = %q, want %q", body, want)
	}
}
