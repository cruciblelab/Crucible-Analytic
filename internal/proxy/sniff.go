package proxy

import (
	"bytes"
	"io"
	"net"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/ja4"
)

const (
	tlsRecordHeaderLen     = 5
	tlsRecordTypeHandshake = 0x16

	// maxClientHelloBytes bounds how much we'll buffer while assembling a
	// ClientHello, so a peer streaming an endless run of handshake-typed
	// records can't grow memory unbounded for one connection. Real
	// ClientHellos are typically well under 4KB even with many extensions.
	maxClientHelloBytes = 32 * 1024
)

// sniffClientHello reads just enough bytes from conn to extract a complete
// TLS ClientHello handshake message (which may span multiple TLS records),
// returning the JA4 fingerprint and every byte read so the caller can
// replay them verbatim to the backend.
//
// If the connection doesn't yield a recognizable, complete TLS ClientHello
// - plaintext traffic, a truncated/malformed handshake, or simply nothing
// arriving before deadline - ja4Hash is "" but buffered still contains
// whatever bytes were read. Fingerprinting is strictly best-effort: its
// failure must never drop or delay the connection, so callers always
// proceed to forward buffered+conn to the backend regardless of the
// returned fingerprint.
func sniffClientHello(conn net.Conn, deadline time.Duration) (buffered []byte, ja4Hash string) {
	if deadline > 0 {
		_ = conn.SetReadDeadline(time.Now().Add(deadline))
		defer conn.SetReadDeadline(time.Time{})
	}

	var buf bytes.Buffer
	tee := io.TeeReader(conn, &buf)

	var handshakeMsg []byte
	for {
		var hdr [tlsRecordHeaderLen]byte
		if _, err := io.ReadFull(tee, hdr[:]); err != nil {
			return buf.Bytes(), ""
		}
		if hdr[0] != tlsRecordTypeHandshake {
			return buf.Bytes(), ""
		}

		recordLen := int(hdr[3])<<8 | int(hdr[4])
		if recordLen <= 0 || buf.Len()+recordLen > maxClientHelloBytes {
			return buf.Bytes(), ""
		}

		payload := make([]byte, recordLen)
		if _, err := io.ReadFull(tee, payload); err != nil {
			return buf.Bytes(), ""
		}
		handshakeMsg = append(handshakeMsg, payload...)

		if len(handshakeMsg) < 4 {
			continue // the 4-byte handshake header itself was split across TLS records
		}
		declaredLen := int(handshakeMsg[1])<<16 | int(handshakeMsg[2])<<8 | int(handshakeMsg[3])
		if len(handshakeMsg) >= 4+declaredLen {
			break
		}
	}

	ch, err := ja4.ParseClientHello(handshakeMsg)
	if err != nil {
		return buf.Bytes(), ""
	}
	return buf.Bytes(), ja4.Fingerprint(ch)
}
