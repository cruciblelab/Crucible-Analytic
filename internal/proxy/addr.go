package proxy

import (
	"net"
	"net/netip"
)

// ipFromAddr extracts the connection's remote IP as a netip.Addr, unmapped
// to plain IPv4 when applicable. Unmapping matters: a dual-stack listener
// can hand back an IPv4 client's address as an IPv4-in-IPv6 form
// (::ffff:a.b.c.d), and without normalizing it, the same real-world client
// could silently split across two different RateStore entries depending on
// which form a given connection happened to arrive as.
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
