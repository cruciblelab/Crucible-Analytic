// Package ja4 implements a minimal, dependency-free parser for TLS
// ClientHello messages and the JA4 client fingerprint derived from them.
//
// Only the TLS-over-TCP ("t") fingerprint is implemented, since the
// collector never terminates QUIC. Parsing operates purely on bytes handed
// to it; reading those bytes off a live connection is the caller's
// responsibility (see internal/proxy), keeping this package easy to unit
// test and free of I/O concerns.
package ja4

import "errors"

var (
	// ErrTruncated is returned when the supplied bytes end before a
	// complete, well-formed ClientHello could be parsed.
	ErrTruncated = errors.New("ja4: truncated ClientHello data")
	// ErrNotClientHello is returned when the handshake message type is
	// not ClientHello (0x01).
	ErrNotClientHello = errors.New("ja4: not a ClientHello handshake message")
)

const (
	handshakeTypeClientHello = 0x01

	extServerName          = 0x0000
	extALPN                = 0x0010
	extSignatureAlgorithms = 0x000d
	extSupportedVersions   = 0x002b
)

// ClientHello holds the subset of a parsed TLS ClientHello handshake
// message needed to compute a JA4 fingerprint.
type ClientHello struct {
	// LegacyVersion is the top-level "client_version" field. TLS 1.3
	// clients pin this to TLS 1.2 (0x0303) for backwards compatibility and
	// signal the real version via SupportedVersions instead.
	LegacyVersion uint16
	// CipherSuites lists offered cipher suites in wire order, GREASE
	// values already removed.
	CipherSuites []uint16
	// Extensions lists extension type IDs in wire order, GREASE values
	// already removed. Used both for the JA4 extension count and (minus
	// SNI/ALPN) the JA4_c hash input.
	Extensions []uint16
	// SupportedVersions holds the supported_versions extension payload,
	// GREASE values already removed.
	SupportedVersions []uint16
	// SignatureAlgorithms holds the signature_algorithms extension
	// payload in wire order, GREASE values already removed.
	SignatureAlgorithms []uint16
	// ALPNProtocols holds the application_layer_protocol_negotiation
	// extension's protocol list in wire order.
	ALPNProtocols []string
	// HasSNI reports whether a server_name extension was present.
	HasSNI bool
}

// ParseClientHello parses a single TLS handshake message (the 4-byte
// handshake header followed by its body) as a ClientHello. The caller is
// responsible for stripping TLS record headers and reassembling a
// handshake message that may have spanned multiple records.
func ParseClientHello(msg []byte) (*ClientHello, error) {
	c := &cursor{b: msg}

	hsType, ok := c.readUint8()
	if !ok {
		return nil, ErrTruncated
	}
	if hsType != handshakeTypeClientHello {
		return nil, ErrNotClientHello
	}
	hsLen, ok := c.readUint24()
	if !ok {
		return nil, ErrTruncated
	}
	if int(hsLen) > c.remaining() {
		return nil, ErrTruncated
	}
	// Constrain parsing to exactly the declared handshake body so any
	// trailing bytes (e.g. the start of the next handshake message) are
	// never mistaken for ClientHello content.
	c.b = c.b[:4+int(hsLen)]

	legacyVersion, ok := c.readUint16()
	if !ok {
		return nil, ErrTruncated
	}

	if !c.skip(32) { // random
		return nil, ErrTruncated
	}

	sessionIDLen, ok := c.readUint8()
	if !ok || !c.skip(int(sessionIDLen)) {
		return nil, ErrTruncated
	}

	cipherLen, ok := c.readUint16()
	if !ok || cipherLen%2 != 0 {
		return nil, ErrTruncated
	}
	cipherBytes, ok := c.readBytes(int(cipherLen))
	if !ok {
		return nil, ErrTruncated
	}
	ch := &ClientHello{LegacyVersion: legacyVersion}
	for cc := (&cursor{b: cipherBytes}); cc.remaining() > 0; {
		v, ok := cc.readUint16()
		if !ok {
			return nil, ErrTruncated
		}
		if !isGrease(v) {
			ch.CipherSuites = append(ch.CipherSuites, v)
		}
	}

	compressionLen, ok := c.readUint8()
	if !ok || !c.skip(int(compressionLen)) {
		return nil, ErrTruncated
	}

	// Extensions are technically optional (a ClientHello may end right
	// after compression methods), though every modern client sends them.
	if c.remaining() == 0 {
		return ch, nil
	}

	extTotalLen, ok := c.readUint16()
	if !ok || int(extTotalLen) > c.remaining() {
		return nil, ErrTruncated
	}
	extBytes, _ := c.readBytes(int(extTotalLen))

	if err := parseExtensions(extBytes, ch); err != nil {
		return nil, err
	}

	return ch, nil
}

func parseExtensions(b []byte, ch *ClientHello) error {
	ec := &cursor{b: b}
	for ec.remaining() > 0 {
		extType, ok := ec.readUint16()
		if !ok {
			return ErrTruncated
		}
		extLen, ok := ec.readUint16()
		if !ok {
			return ErrTruncated
		}
		extBody, ok := ec.readBytes(int(extLen))
		if !ok {
			return ErrTruncated
		}

		if !isGrease(extType) {
			ch.Extensions = append(ch.Extensions, extType)
		}

		switch extType {
		case extServerName:
			ch.HasSNI = true
		case extALPN:
			ch.ALPNProtocols = parseALPN(extBody)
		case extSupportedVersions:
			ch.SupportedVersions = parseUint16ListWithPrefix8(extBody)
		case extSignatureAlgorithms:
			ch.SignatureAlgorithms = parseUint16ListWithPrefix16(extBody)
		}
	}
	return nil
}

// parseALPN parses a ProtocolNameList: a 2-byte total length followed by
// {1-byte length, bytes} entries.
func parseALPN(body []byte) []string {
	c := &cursor{b: body}
	listLen, ok := c.readUint16()
	if !ok || int(listLen) > c.remaining() {
		return nil
	}
	end := c.pos + int(listLen)
	var protos []string
	for c.pos < end {
		l, ok := c.readUint8()
		if !ok {
			return protos
		}
		nameBytes, ok := c.readBytes(int(l))
		if !ok {
			return protos
		}
		protos = append(protos, string(nameBytes))
	}
	return protos
}

// parseUint16ListWithPrefix8 parses a list of uint16 values prefixed by a
// 1-byte length-in-bytes, as used by supported_versions. GREASE values are
// dropped.
func parseUint16ListWithPrefix8(body []byte) []uint16 {
	c := &cursor{b: body}
	listLen, ok := c.readUint8()
	if !ok || int(listLen) > c.remaining() {
		return nil
	}
	var out []uint16
	for n := int(listLen) / 2; n > 0; n-- {
		v, ok := c.readUint16()
		if !ok {
			return out
		}
		if !isGrease(v) {
			out = append(out, v)
		}
	}
	return out
}

// parseUint16ListWithPrefix16 parses a list of uint16 values prefixed by a
// 2-byte length-in-bytes, as used by signature_algorithms. GREASE values
// are dropped.
func parseUint16ListWithPrefix16(body []byte) []uint16 {
	c := &cursor{b: body}
	listLen, ok := c.readUint16()
	if !ok || int(listLen) > c.remaining() {
		return nil
	}
	var out []uint16
	for n := int(listLen) / 2; n > 0; n-- {
		v, ok := c.readUint16()
		if !ok {
			return out
		}
		if !isGrease(v) {
			out = append(out, v)
		}
	}
	return out
}
