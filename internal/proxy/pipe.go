package proxy

import (
	"io"
	"log/slog"
	"net"
	"sync"
)

// pipeConns splices bytes bidirectionally between client and backend until
// both directions finish. clientReader must yield exactly the bytes the
// client sent - including any already consumed from client while sniffing
// the ClientHello - so the backend sees an unmodified byte stream.
// Each direction gets its own recoverConn: these are separate goroutines,
// and recover() only sees panics raised on the goroutine it runs on, so
// the one in handleConn does not cover either of them. wg.Done is
// deferred before it so the count still drops on a panic - otherwise a
// contained panic would leave wg.Wait blocked forever and leak the
// connection it was supposed to save.
func pipeConns(logger *slog.Logger, client net.Conn, clientReader io.Reader, backend net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer recoverConn(logger, "pipeConns: client to backend")
		io.Copy(backend, clientReader)
		closeWrite(backend)
	}()
	go func() {
		defer wg.Done()
		defer recoverConn(logger, "pipeConns: backend to client")
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
