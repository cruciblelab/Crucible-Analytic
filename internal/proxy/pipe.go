package proxy

import (
	"io"
	"net"
	"sync"
)

// pipeConns splices bytes bidirectionally between client and backend until
// both directions finish. clientReader must yield exactly the bytes the
// client sent - including any already consumed from client while sniffing
// the ClientHello - so the backend sees an unmodified byte stream.
func pipeConns(client net.Conn, clientReader io.Reader, backend net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(backend, clientReader)
		closeWrite(backend)
	}()
	go func() {
		defer wg.Done()
		io.Copy(client, backend)
		closeWrite(client)
	}()

	wg.Wait()
}

// closeWrite half-closes the write side (TCP FIN) if the connection
// supports it, so protocols that rely on half-close - e.g. a client that
// shuts its write side right after sending a request - still behave
// correctly across the proxy instead of hanging.
func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}
