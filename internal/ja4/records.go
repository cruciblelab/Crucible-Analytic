package ja4

const (
	recordTypeHandshake = 0x16
	recordHeaderLen     = 5
)

// ParseClientHelloFromRecords extracts and parses a ClientHello from raw
// bytes that contain one or more complete TLS records (5-byte header +
// payload each), reassembling the handshake message across record
// boundaries if it was fragmented. Unlike ParseClientHello, which expects
// the handshake message bytes directly, this operates on record-framed
// bytes exactly as they appear on the wire - e.g. bytes captured by
// snooping a connection before crypto/tls consumes them, as internal/
// fullproxy does to fingerprint a connection it also terminates TLS on.
//
// Any bytes in raw beyond a complete ClientHello handshake message are
// ignored, matching ParseClientHello's own tolerance of trailing bytes.
func ParseClientHelloFromRecords(raw []byte) (*ClientHello, error) {
	var msg []byte
	for len(raw) > 0 {
		if len(raw) < recordHeaderLen {
			return nil, ErrTruncated
		}
		if raw[0] != recordTypeHandshake {
			return nil, ErrNotClientHello
		}

		recordLen := int(raw[3])<<8 | int(raw[4])
		if len(raw) < recordHeaderLen+recordLen {
			return nil, ErrTruncated
		}
		msg = append(msg, raw[recordHeaderLen:recordHeaderLen+recordLen]...)
		raw = raw[recordHeaderLen+recordLen:]

		if len(msg) >= 4 {
			declaredLen := int(msg[1])<<16 | int(msg[2])<<8 | int(msg[3])
			if len(msg) >= 4+declaredLen {
				break
			}
		}
	}

	if len(msg) == 0 {
		return nil, ErrTruncated
	}
	return ParseClientHello(msg)
}
