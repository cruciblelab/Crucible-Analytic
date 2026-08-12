package beacon

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func requestFrom(remoteAddr string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/_ca/event", nil)
	r.RemoteAddr = remoteAddr
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	return r
}

func mustResolve(t *testing.T, c ClientIPResolver, r *http.Request) netip.Addr {
	t.Helper()
	ip, ok := c.ClientIP(r)
	if !ok {
		t.Fatalf("ClientIP(%q) reported failure", r.RemoteAddr)
	}
	return ip
}

func mustPrefixes(t *testing.T, entries ...string) []netip.Prefix {
	t.Helper()
	prefixes, err := ParseTrustedProxies(entries)
	if err != nil {
		t.Fatalf("ParseTrustedProxies(%v): %v", entries, err)
	}
	return prefixes
}

// The whole point of the trusted-proxy list. A client that could set
// its own X-Forwarded-For could choose its own country, its own ASN and
// its own visitor ID, and would poison the join back to
// traffic_snapshots - the one thing running two data sources is for.
func TestClientIP_ForwardedHeadersFromAnUntrustedPeerAreIgnored(t *testing.T) {
	c := ClientIPResolver{} // trusts nothing, the default

	got := mustResolve(t, c, requestFrom("203.0.113.9:41234", map[string]string{
		"X-Forwarded-For": "1.2.3.4",
		"X-Real-IP":       "5.6.7.8",
	}))
	if got != netip.MustParseAddr("203.0.113.9") {
		t.Errorf("ClientIP = %v, want the real peer 203.0.113.9 - a spoofed header was believed", got)
	}
}

func TestClientIP_TrustedProxyForwardsTheRealClient(t *testing.T) {
	c := ClientIPResolver{TrustedProxies: mustPrefixes(t, "127.0.0.1/32")}

	got := mustResolve(t, c, requestFrom("127.0.0.1:5555", map[string]string{
		"X-Forwarded-For": "198.51.100.23",
	}))
	if got != netip.MustParseAddr("198.51.100.23") {
		t.Errorf("ClientIP = %v, want 198.51.100.23", got)
	}
}

// "client, proxy1, proxy2": the leftmost entry is whatever the client
// itself claimed, so taking it - the obvious-looking choice - would
// trust an attacker-supplied value even behind a correct proxy setup.
func TestClientIP_TakesTheRightmostUntrustedHop(t *testing.T) {
	c := ClientIPResolver{TrustedProxies: mustPrefixes(t, "127.0.0.1/32", "10.0.0.0/8")}

	got := mustResolve(t, c, requestFrom("127.0.0.1:5555", map[string]string{
		// 1.1.1.1 is a value the client invented; 198.51.100.23 is what
		// the outermost trusted proxy actually observed.
		"X-Forwarded-For": "1.1.1.1, 198.51.100.23, 10.0.0.7",
	}))
	if got != netip.MustParseAddr("198.51.100.23") {
		t.Errorf("ClientIP = %v, want 198.51.100.23 (the furthest hop our own chain vouched for)", got)
	}
}

func TestClientIP_AllHopsTrustedFallsBackToThePeer(t *testing.T) {
	c := ClientIPResolver{TrustedProxies: mustPrefixes(t, "10.0.0.0/8")}

	got := mustResolve(t, c, requestFrom("10.0.0.1:5555", map[string]string{
		"X-Forwarded-For": "10.0.0.9, 10.0.0.7",
	}))
	if got != netip.MustParseAddr("10.0.0.1") {
		t.Errorf("ClientIP = %v, want the peer 10.0.0.1", got)
	}
}

// A hop we cannot parse breaks the chain of custody: we can no longer
// tell which side of it was added by trusted infrastructure, so the
// walk stops rather than skipping past it into client-controlled text.
func TestClientIP_MalformedHopStopsTheWalk(t *testing.T) {
	c := ClientIPResolver{TrustedProxies: mustPrefixes(t, "127.0.0.1/32", "10.0.0.0/8")}

	got := mustResolve(t, c, requestFrom("127.0.0.1:5555", map[string]string{
		"X-Forwarded-For": "9.9.9.9, garbage, 10.0.0.7",
	}))
	if got == netip.MustParseAddr("9.9.9.9") {
		t.Error("the walk continued past an unparseable hop into client-controlled values")
	}
	if got != netip.MustParseAddr("127.0.0.1") {
		t.Errorf("ClientIP = %v, want the peer 127.0.0.1", got)
	}
}

func TestClientIP_FallsBackToXRealIP(t *testing.T) {
	c := ClientIPResolver{TrustedProxies: mustPrefixes(t, "127.0.0.1/32")}

	got := mustResolve(t, c, requestFrom("127.0.0.1:5555", map[string]string{
		"X-Real-IP": "198.51.100.44",
	}))
	if got != netip.MustParseAddr("198.51.100.44") {
		t.Errorf("ClientIP = %v, want 198.51.100.44", got)
	}
}

func TestClientIP_HandlesIPv6AndPortedHops(t *testing.T) {
	c := ClientIPResolver{TrustedProxies: mustPrefixes(t, "::1/128")}

	got := mustResolve(t, c, requestFrom("[::1]:5555", map[string]string{
		// Some proxies append the source port to each hop.
		"X-Forwarded-For": "[2001:db8::5]:41234",
	}))
	if got != netip.MustParseAddr("2001:db8::5") {
		t.Errorf("ClientIP = %v, want 2001:db8::5", got)
	}
}

func TestClientIP_UnmapsIPv4MappedAddresses(t *testing.T) {
	// A dual-stack listener spells an IPv4 peer as ::ffff:a.b.c.d. Left
	// mapped, the same visitor would hash to two different visitor IDs
	// depending on which socket they landed on.
	got := mustResolve(t, ClientIPResolver{}, requestFrom("[::ffff:203.0.113.9]:41234", nil))
	if got != netip.MustParseAddr("203.0.113.9") {
		t.Errorf("ClientIP = %v, want the unmapped 203.0.113.9", got)
	}
}

func TestClientIP_RejectsAnUnparseablePeer(t *testing.T) {
	if _, ok := (ClientIPResolver{}).ClientIP(requestFrom("not-an-address", nil)); ok {
		t.Error("ClientIP accepted an unparseable RemoteAddr")
	}
}

func TestParseTrustedProxies_AcceptsBareAddressesAndCIDRs(t *testing.T) {
	prefixes := mustPrefixes(t, "10.0.0.5", "192.168.0.0/16", " 172.16.0.0/12 ", "")
	if len(prefixes) != 3 {
		t.Fatalf("parsed %d prefixes, want 3 (blank entries skipped)", len(prefixes))
	}

	c := ClientIPResolver{TrustedProxies: prefixes}
	if !c.trusts(netip.MustParseAddr("10.0.0.5")) {
		t.Error("a bare address was not treated as a single-host prefix")
	}
	if c.trusts(netip.MustParseAddr("10.0.0.6")) {
		t.Error("a bare address widened into a network")
	}
	if !c.trusts(netip.MustParseAddr("192.168.44.1")) {
		t.Error("a /16 did not contain an address inside it")
	}
}

func TestParseTrustedProxies_RejectsGarbage(t *testing.T) {
	if _, err := ParseTrustedProxies([]string{"definitely not a network"}); err == nil {
		t.Error("ParseTrustedProxies accepted an unparseable entry")
	}
}
