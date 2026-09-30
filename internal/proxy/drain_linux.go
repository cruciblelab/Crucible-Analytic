//go:build linux

package proxy

import (
	"net"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// kernelView asks the kernel about one client socket: TCP_INFO.
//
// tcpi_last_data_sent and tcpi_last_data_recv are milliseconds since data
// last went each way - data only: an ACK or a keepalive probe moves
// neither. The byte counters say whether any data has gone each way at
// all (bytes_sent needs 4.19; data_segs_out and bytes_acked cover the
// kernels before it, and a kernel with none of them reads as "nothing
// sent", which only ever makes a connection busy). notsent_bytes and
// unacked are a response still leaving.
//
// ok is false when the connection is not a socket the kernel will
// describe.
func kernelView(c net.Conn) (view, bool) {
	sc, isSyscall := c.(syscall.Conn)
	if !isSyscall {
		return view{}, false
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return view{}, false
	}
	var info *unix.TCPInfo
	var infoErr error
	if err := raw.Control(func(fd uintptr) {
		info, infoErr = unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO)
	}); err != nil || infoErr != nil {
		return view{}, false
	}
	return view{
		toClient:   time.Duration(info.Last_data_sent) * time.Millisecond,
		fromClient: time.Duration(info.Last_data_recv) * time.Millisecond,
		sent:       info.Bytes_sent > 0 || info.Data_segs_out > 0 || info.Bytes_acked > 0,
		received:   info.Bytes_received > 0 || info.Data_segs_in > 0,
		inFlight:   info.Notsent_bytes > 0 || info.Unacked > 0,
	}, true
}

// Asserted here rather than hoped for: a *net.TCPConn is what the
// listener hands Serve, and it has to be a syscall.Conn for the question
// above to be askable at all.
var _ syscall.Conn = (*net.TCPConn)(nil)
