//go:build !linux

package proxy

import "net"

// kernelView cannot ask the kernel here: TCP_INFO's data timers are
// Linux's. Every connection counts as busy, so a shutdown waits for the
// deadline rather than cutting a request it cannot see. The product runs
// on Linux; this keeps the other platforms building and honest.
func kernelView(net.Conn) (view, bool) { return view{}, false }
