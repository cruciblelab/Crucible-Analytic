package fullproxy

import (
	"bytes"
	"net"
	"sync"
)

// maxSnoopBytes bounds how much of a connection's raw bytes get buffered
// while waiting for the server's GetConfigForClient hook to capture and
// release them - a safety bound in case that hook is never invoked for
// some reason (it always should be, given how the TLS listener is wired
// up), so a long-lived connection can never grow this buffer unbounded.
const maxSnoopBytes = 32 * 1024

// snoopConn wraps a net.Conn to capture a copy of bytes read from it,
// without altering what the caller (crypto/tls) sees or does. This exists
// because tls.ClientHelloInfo only exposes a parsed subset of ClientHello
// fields, not the raw bytes JA4 needs - so the server snoops the raw bytes
// itself via this wrapper, then hands them to the same
// ja4.ParseClientHello* functions the passthrough proxy uses.
type snoopConn struct {
	net.Conn

	mu        sync.Mutex
	buf       bytes.Buffer
	capturing bool
	fp        string
}

func newSnoopConn(c net.Conn) *snoopConn {
	return &snoopConn{Conn: c, capturing: true}
}

func (c *snoopConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.mu.Lock()
		if c.capturing {
			if c.buf.Len()+n <= maxSnoopBytes {
				c.buf.Write(p[:n])
			} else {
				c.capturing = false
			}
		}
		c.mu.Unlock()
	}
	return n, err
}

// stopCapturing returns everything captured so far and stops capturing
// further bytes. Called once, from the server's GetConfigForClient hook,
// after crypto/tls has parsed the ClientHello - anything read after that
// point is encrypted application data, of no use for fingerprinting.
func (c *snoopConn) stopCapturing() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.capturing = false
	b := c.buf.Bytes()
	c.buf = bytes.Buffer{} // release the backing array now that we're done with it
	return b
}

// setFingerprint and fingerprint publish the JA4 computed from the
// captured bytes across goroutines: the write happens once, from the
// handshake, before any request is read; reads happen later, from
// whichever goroutine handles each request (for HTTP/2, that can be a
// different goroutine per stream, so this needs real synchronization, not
// just happens-before-by-construction).
func (c *snoopConn) setFingerprint(fp string) {
	c.mu.Lock()
	c.fp = fp
	c.mu.Unlock()
}

func (c *snoopConn) fingerprint() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fp
}

// snoopListener wraps a net.Listener so every accepted connection is
// wrapped in a snoopConn before crypto/tls (via tls.NewListener) takes it -
// that ordering is what makes tls.ClientHelloInfo.Conn resolve back to a
// *snoopConn later.
type snoopListener struct {
	net.Listener
}

func (l *snoopListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return newSnoopConn(c), nil
}
