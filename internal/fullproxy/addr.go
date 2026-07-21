package fullproxy

import (
	"net"
	"net/netip"
)

// ipFromAddr extracts a connection's remote IP as a netip.Addr, unmapped
// to plain IPv4 when applicable - see internal/proxy's identical helper
// for why this matters (it avoids the same client silently splitting
// across two RateStore entries depending on IPv4-mapped-IPv6 spelling).
// Duplicated rather than shared: internal/proxy's version is unexported,
// and four lines don't justify a shared package.
func ipFromAddr(addr net.Addr) (netip.Addr, bool) {
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return netip.Addr{}, false
	}
	ip, ok := netip.AddrFromSlice(tcpAddr.IP)
	if !ok {
		return netip.Addr{}, false
	}
	return ip.Unmap(), true
}
