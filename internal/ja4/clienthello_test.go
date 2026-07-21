package ja4

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
)

func u16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func u24(v uint32) []byte {
	return []byte{byte(v >> 16), byte(v >> 8), byte(v)}
}

func buildExtension(typ uint16, body []byte) []byte {
	var buf bytes.Buffer
	buf.Write(u16(typ))
	buf.Write(u16(uint16(len(body))))
	buf.Write(body)
	return buf.Bytes()
}

func buildSNIExtension(host string) []byte {
	var name bytes.Buffer
	name.WriteByte(0) // host_name
	name.Write(u16(uint16(len(host))))
	name.WriteString(host)

	var list bytes.Buffer
	list.Write(u16(uint16(name.Len())))
	list.Write(name.Bytes())

	return buildExtension(extServerName, list.Bytes())
}

func buildALPNExtension(protos ...string) []byte {
	var list bytes.Buffer
	for _, p := range protos {
		list.WriteByte(byte(len(p)))
		list.WriteString(p)
	}
	var body bytes.Buffer
	body.Write(u16(uint16(list.Len())))
	body.Write(list.Bytes())
	return buildExtension(extALPN, body.Bytes())
}

func buildSupportedVersionsExtension(versions ...uint16) []byte {
	var list bytes.Buffer
	for _, v := range versions {
		list.Write(u16(v))
	}
	var body bytes.Buffer
	body.WriteByte(byte(list.Len()))
	body.Write(list.Bytes())
	return buildExtension(extSupportedVersions, body.Bytes())
}

func buildSignatureAlgorithmsExtension(algs ...uint16) []byte {
	var list bytes.Buffer
	for _, a := range algs {
		list.Write(u16(a))
	}
	var body bytes.Buffer
	body.Write(u16(uint16(list.Len())))
	body.Write(list.Bytes())
	return buildExtension(extSignatureAlgorithms, body.Bytes())
}

func buildGenericExtension(typ uint16, bodyLen int) []byte {
	return buildExtension(typ, make([]byte, bodyLen))
}

// buildClientHello constructs a full ClientHello handshake message (4-byte
// handshake header + body) from wire-level fields, for use as a test
// fixture. It deliberately re-implements the wire format independently of
// clienthello.go so the parser is exercised against a separately-written
// encoder rather than mirroring its own assumptions.
func buildClientHello(legacyVersion uint16, ciphers []uint16, extensions [][]byte) []byte {
	var body bytes.Buffer
	body.Write(u16(legacyVersion))
	body.Write(make([]byte, 32)) // random
	body.WriteByte(0)            // session_id length = 0

	var cipherBytes bytes.Buffer
	for _, c := range ciphers {
		cipherBytes.Write(u16(c))
	}
	body.Write(u16(uint16(cipherBytes.Len())))
	body.Write(cipherBytes.Bytes())

	body.WriteByte(1) // compression methods length
	body.WriteByte(0) // null compression

	var extBytes bytes.Buffer
	for _, e := range extensions {
		extBytes.Write(e)
	}
	body.Write(u16(uint16(extBytes.Len())))
	body.Write(extBytes.Bytes())

	var msg bytes.Buffer
	msg.WriteByte(handshakeTypeClientHello)
	msg.Write(u24(uint32(body.Len())))
	msg.Write(body.Bytes())

	return msg.Bytes()
}

func TestParseClientHello_Basic(t *testing.T) {
	msg := buildClientHello(0x0303,
		[]uint16{0x1301, 0x0a0a, 0x1302}, // 0x0a0a is GREASE and must be filtered
		[][]byte{
			buildSNIExtension("example.com"),
			buildSupportedVersionsExtension(0x0304, 0x0a0a),
			buildALPNExtension("h2", "http/1.1"),
			buildSignatureAlgorithmsExtension(0x0403, 0x0804, 0x1a1a),
			buildGenericExtension(0x002d, 2), // psk_key_exchange_modes
			buildGenericExtension(0xdada, 0), // GREASE extension type
		},
	)

	ch, err := ParseClientHello(msg)
	if err != nil {
		t.Fatalf("ParseClientHello returned error: %v", err)
	}

	if want := []uint16{0x1301, 0x1302}; !reflect.DeepEqual(ch.CipherSuites, want) {
		t.Errorf("CipherSuites = %x, want %x (GREASE not filtered)", ch.CipherSuites, want)
	}
	if !ch.HasSNI {
		t.Error("HasSNI = false, want true")
	}
	if want := []uint16{0x0304}; !reflect.DeepEqual(ch.SupportedVersions, want) {
		t.Errorf("SupportedVersions = %x, want %x", ch.SupportedVersions, want)
	}
	if want := []string{"h2", "http/1.1"}; !reflect.DeepEqual(ch.ALPNProtocols, want) {
		t.Errorf("ALPNProtocols = %v, want %v", ch.ALPNProtocols, want)
	}
	if want := []uint16{0x0403, 0x0804}; !reflect.DeepEqual(ch.SignatureAlgorithms, want) {
		t.Errorf("SignatureAlgorithms = %x, want %x (GREASE not filtered)", ch.SignatureAlgorithms, want)
	}
	wantExts := []uint16{extServerName, extSupportedVersions, extALPN, extSignatureAlgorithms, 0x002d}
	if !reflect.DeepEqual(ch.Extensions, wantExts) {
		t.Errorf("Extensions = %x, want %x (GREASE extension type not filtered)", ch.Extensions, wantExts)
	}
}

func TestParseClientHello_NoExtensions(t *testing.T) {
	msg := buildClientHello(0x0301, []uint16{0x002f}, nil)
	ch, err := ParseClientHello(msg)
	if err != nil {
		t.Fatalf("ParseClientHello returned error: %v", err)
	}
	if len(ch.Extensions) != 0 {
		t.Errorf("Extensions = %v, want empty", ch.Extensions)
	}
	if ch.HasSNI {
		t.Error("HasSNI = true, want false")
	}
}

func TestParseClientHello_WrongHandshakeType(t *testing.T) {
	msg := []byte{0x02, 0x00, 0x00, 0x00} // ServerHello type, zero length
	_, err := ParseClientHello(msg)
	if !errors.Is(err, ErrNotClientHello) {
		t.Errorf("err = %v, want ErrNotClientHello", err)
	}
}

func TestParseClientHello_Truncated(t *testing.T) {
	full := buildClientHello(0x0303, []uint16{0x1301, 0x1302}, [][]byte{buildALPNExtension("h2")})
	for cut := 0; cut < len(full); cut++ {
		if _, err := ParseClientHello(full[:cut]); err == nil {
			t.Errorf("cut at %d/%d bytes: expected error, got nil", cut, len(full))
		}
	}
}

func TestParseClientHello_EmptyALPNProtocolName(t *testing.T) {
	// A zero-length ALPN protocol name is malformed but must not panic.
	msg := buildClientHello(0x0303, []uint16{0x1301}, [][]byte{buildALPNExtension("")})
	ch, err := ParseClientHello(msg)
	if err != nil {
		t.Fatalf("ParseClientHello returned error: %v", err)
	}
	if want := []string{""}; !reflect.DeepEqual(ch.ALPNProtocols, want) {
		t.Errorf("ALPNProtocols = %v, want %v", ch.ALPNProtocols, want)
	}
	// Fingerprinting an empty ALPN name must not panic either.
	_ = Fingerprint(ch)
}
