package web

import (
	"bytes"
	"log/slog"
	"net"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/clientip"
)

func resolverFor(t *testing.T, entries ...string) clientip.Resolver {
	t.Helper()
	cfg := Config{TrustedProxies: entries}
	r, err := cfg.ClientIPResolver()
	if err != nil {
		t.Fatalf("ClientIPResolver(%v): %v", entries, err)
	}
	return r
}

// With nothing trusted - the default, and every install made before
// H12 - the peer is the answer and a header is ignored, however it is
// spelled. What a link was used from, and what a sign-in limit counts,
// is never a value the person making the request chose.
func TestClientAddrIgnoresForwardedHeadersFromAnUntrustedPeer(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.0.2.10:44321"
	r.Header.Set("X-Forwarded-For", "203.0.113.1")
	r.Header.Set("X-Real-IP", "203.0.113.2")

	if got := s.clientAddr(r); got.String() != "192.0.2.10" {
		t.Fatalf("clientAddr = %q; a forwarded header from an untrusted peer was believed", got)
	}
}

// H12: behind the proxy KURULUM section 8 sets up, the visitor is the
// rightmost hop the proxy did not vouch for - not the proxy, and not the
// leftmost hop, which the visitor wrote themselves.
func TestClientAddrBehindATrustedProxyIsTheVisitor(t *testing.T) {
	s := &Server{ClientIP: resolverFor(t, "127.0.0.1/32")}
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:51000"
	// The visitor claimed 8.8.8.8; nginx appended the address it saw.
	r.Header.Set("X-Forwarded-For", "8.8.8.8, 85.105.20.11")

	if got := s.clientAddr(r); got != netip.MustParseAddr("85.105.20.11") {
		t.Fatalf("clientAddr = %q, want the address the proxy saw (85.105.20.11)", got)
	}
}

// Two visitors behind the same proxy are two addresses: the property the
// sign-in limits needed and did not have.
func TestClientAddrTellsVisitorsBehindOneProxyApart(t *testing.T) {
	s := &Server{ClientIP: resolverFor(t, "127.0.0.1")}
	addr := func(xff string) netip.Addr {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = "127.0.0.1:51000"
		r.Header.Set("X-Forwarded-For", xff)
		return s.clientAddr(r)
	}
	if a, b := addr("85.105.20.11"), addr("84.128.1.20"); a == b {
		t.Fatalf("two visitors behind one proxy are one address: %s", a)
	}
}

func TestClientAddrSurvivesRubbish(t *testing.T) {
	s := &Server{ClientIP: resolverFor(t, "127.0.0.1/32")}
	for _, remote := range []string{"", "not-an-address", "[::1]:80", "127.0.0.1"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		r.Header.Set("X-Forwarded-For", "garbage, , 1.2.3")
		// No panic; an unparsable address becomes the zero value, which
		// the store records as unknown.
		_ = s.clientAddr(r)
	}
}

func TestConfigRefusesAnUnparsableTrustedProxy(t *testing.T) {
	cfg := Config{TrustedProxies: []string{"nginx.local"}}
	if _, err := cfg.ClientIPResolver(); err == nil || !strings.Contains(err.Error(), "trusted_proxies") {
		t.Fatalf("an unparsable entry was accepted, or the error does not name the key: %v", err)
	}
}

// The deployment H12 found is one where nothing says anything is wrong.
// A forwarding header from a peer trusted_proxies does not name is the
// sign of it, and the log says so - once, since on a directly exposed
// panel anybody can send that header.
func TestAnUntrustedForwardingPeerIsWarnedAboutOnce(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{Logger: slog.New(slog.NewJSONHandler(&buf, nil))}
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest("POST", LoginPath, nil)
		r.RemoteAddr = "127.0.0.1:51000"
		r.Header.Set("X-Forwarded-For", "85.105.20.11")
		s.clientAddr(r)
	}
	out := buf.String()
	if n := strings.Count(out, "trusted_proxies does not name"); n != 1 {
		t.Fatalf("warned %d times, want once:\n%s", n, out)
	}
	for _, want := range []string{`"level":"WARN"`, `"peer":"127.0.0.1"`, `"how":"if that peer is your reverse proxy, name it in panel.toml trusted_proxies"`} {
		if !strings.Contains(out, want) {
			t.Errorf("the warning does not carry %s:\n%s", want, out)
		}
	}
}

// And not about a request that carries no header, nor about one from a
// trusted proxy: a warning that fires on every working deployment is a
// warning everybody learns to skip.
func TestNoWarningWithoutAReason(t *testing.T) {
	var buf bytes.Buffer
	s := &Server{Logger: slog.New(slog.NewJSONHandler(&buf, nil)), ClientIP: resolverFor(t, "127.0.0.1/32")}
	plain := httptest.NewRequest("GET", "/", nil)
	plain.RemoteAddr = "192.0.2.10:1"
	s.clientAddr(plain)
	proxied := httptest.NewRequest("GET", "/", nil)
	proxied.RemoteAddr = "127.0.0.1:1"
	proxied.Header.Set("X-Forwarded-For", "85.105.20.11")
	s.clientAddr(proxied)
	if buf.Len() != 0 {
		t.Fatalf("warned without a reason:\n%s", buf.String())
	}
}

// The example is what install.sh copies, and the panel it describes
// listens on loopback - so the only proxy that can stand in front of it
// is on loopback too, and the example has to trust it. Without that
// line a fresh install behind the KURULUM section 8 proxy is H12 again.
func TestTheExampleConfigTrustsTheProxyInFrontOfIt(t *testing.T) {
	cfg, err := LoadConfig("../../../panel.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := cfg.ClientIPResolver()
	if err != nil {
		t.Fatal(err)
	}
	host, _, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		t.Fatalf("listen_addr %q: %v", cfg.ListenAddr, err)
	}
	listen, err := netip.ParseAddr(host)
	if err != nil || !listen.IsLoopback() {
		t.Fatalf("the example no longer listens on loopback (%q); decide what it trusts again", cfg.ListenAddr)
	}
	for _, peer := range []string{"127.0.0.1", "::1"} {
		if !resolver.Trusts(netip.MustParseAddr(peer)) {
			t.Errorf("panel.example.toml does not trust %s, the only place a proxy in front of a "+
				"loopback panel can be", peer)
		}
	}
}

// A malformed entry is refused when the file is loaded, not discovered
// at the first sign-in from behind the proxy.
func TestLoadingRefusesAnUnparsableTrustedProxy(t *testing.T) {
	src, err := os.ReadFile("../../../panel.example.toml")
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(src), `trusted_proxies = ["127.0.0.1/32", "::1/128"]`, `trusted_proxies = ["nginx.local"]`, 1)
	if bad == string(src) {
		t.Fatal("the example's trusted_proxies line moved; this test has to follow it")
	}
	path := filepath.Join(t.TempDir(), "panel.toml")
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "trusted_proxies") {
		t.Fatalf("a config naming an unparsable proxy loaded, or the error does not say which key: %v", err)
	}
}
