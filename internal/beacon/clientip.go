package beacon

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// ClientIPResolver works out which address a beacon request really came
// from.
//
// This matters more here than it looks. The beacon is normally deployed
// behind the site's own web server (nginx or Caddy forwarding /_ca/ to
// it), so http.Request.RemoteAddr is 127.0.0.1 for every visitor on
// earth and the real address only exists in a forwarded header. But
// those headers are just request headers: anyone can set them. Trusting
// them unconditionally would let any client choose its own IP, and
// through it its own country, its own ASN, and its own visitor ID -
// which would not merely corrupt the geography breakdown but poison the
// join back to traffic_snapshots, the one thing running both data
// sources is for.
//
// So headers are read only when the immediate peer is a proxy the
// operator has named. With no configured proxies, RemoteAddr is used
// and forwarded headers are ignored entirely.
type ClientIPResolver struct {
	// TrustedProxies are the networks whose forwarded headers are
	// believed. Empty means trust nothing, which is the correct setting
	// when the beacon is exposed directly.
	TrustedProxies []netip.Prefix
}

// ClientIP returns the address to attribute the request to.
func (c ClientIPResolver) ClientIP(r *http.Request) (netip.Addr, bool) {
	peer, ok := peerIP(r.RemoteAddr)
	if !ok {
		return netip.Addr{}, false
	}
	if !c.trusts(peer) {
		return peer, true
	}

	// X-Forwarded-For is "client, proxy1, proxy2": appended left to
	// right, so the rightmost entries are the ones added by
	// infrastructure closest to us and the leftmost is whatever the
	// original client claimed. Walking right to left and stopping at
	// the first address we do NOT trust yields the furthest hop that
	// our own trusted chain actually vouched for. Taking the leftmost
	// entry instead - the obvious-looking choice, and a very common bug
	// - would take a value the client itself is free to invent.
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		hops := strings.Split(forwarded, ",")
		for i := len(hops) - 1; i >= 0; i-- {
			hop, ok := parseHop(hops[i])
			if !ok {
				// A malformed entry breaks the chain of custody: we can
				// no longer tell which side of it is trustworthy, so
				// stop rather than skip past it.
				break
			}
			if !c.trusts(hop) {
				return hop, true
			}
		}
	}

	// nginx's other convention, and unlike X-Forwarded-For it is a
	// single value with no chain to walk.
	if real, ok := parseHop(r.Header.Get("X-Real-IP")); ok {
		return real, true
	}

	// Every hop was itself trusted (or no header was present), so the
	// peer is the best answer available.
	return peer, true
}

func (c ClientIPResolver) trusts(ip netip.Addr) bool {
	for _, prefix := range c.TrustedProxies {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// peerIP parses http.Request.RemoteAddr, which is always host:port for
// TCP listeners.
func peerIP(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// Not host:port - try it as a bare address, which is what an
		// httptest or unix-socket style caller may produce.
		host = remoteAddr
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}

// parseHop parses one X-Forwarded-For element, tolerating the
// surrounding whitespace the header format allows and the bracketed
// [v6]:port form some proxies emit.
func parseHop(hop string) (netip.Addr, bool) {
	hop = strings.TrimSpace(hop)
	if hop == "" {
		return netip.Addr{}, false
	}
	if ip, err := netip.ParseAddr(hop); err == nil {
		return ip.Unmap(), true
	}
	// Fall back to host:port, which appears when a proxy forwards the
	// source port alongside the address.
	if host, _, err := net.SplitHostPort(hop); err == nil {
		if ip, err := netip.ParseAddr(host); err == nil {
			return ip.Unmap(), true
		}
	}
	return netip.Addr{}, false
}

// ParseTrustedProxies converts configured CIDR strings into prefixes. A
// bare address is accepted and treated as a single-host prefix, since
// "trust 10.0.0.5" is a more natural thing to write in a config than
// "trust 10.0.0.5/32".
func ParseTrustedProxies(entries []string) ([]netip.Prefix, error) {
	prefixes := make([]netip.Prefix, 0, len(entries))
	for _, entry := range entries {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			prefixes = append(prefixes, prefix.Masked())
			continue
		}
		ip, err := netip.ParseAddr(entry)
		if err != nil {
			return nil, err
		}
		prefixes = append(prefixes, netip.PrefixFrom(ip.Unmap(), ip.Unmap().BitLen()))
	}
	return prefixes, nil
}
