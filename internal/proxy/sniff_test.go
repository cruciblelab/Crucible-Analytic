package proxy

import (
	"net"
	"testing"
	"time"
)

func buildTLSRecord(payload []byte) []byte {
	rec := make([]byte, tlsRecordHeaderLen+len(payload))
	rec[0] = tlsRecordTypeHandshake
	rec[1], rec[2] = 0x03, 0x01 // record version, arbitrary for this test
	rec[3] = byte(len(payload) >> 8)
	rec[4] = byte(len(payload))
	copy(rec[5:], payload)
	return rec
}

// buildMinimalClientHelloHandshakeMsg builds a structurally valid but
// minimal ClientHello handshake message (one cipher suite, no
// extensions), independent of the ja4 package's own encoder, so this test
// exercises sniffClientHello's record-reassembly logic against a
// separately-constructed fixture.
func buildMinimalClientHelloHandshakeMsg() []byte {
	var body []byte
	body = append(body, 0x03, 0x03)          // legacy_version: TLS 1.2
	body = append(body, make([]byte, 32)...) // random
	body = append(body, 0x00)                // session_id length = 0
	body = append(body, 0x00, 0x02, 0x13, 0x01)
	body = append(body, 0x01, 0x00) // compression methods: len=1, null
	body = append(body, 0x00, 0x00) // extensions: total length = 0

	msg := []byte{0x01, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	return append(msg, body...)
}

func TestSniffClientHello_MultiRecordReassembly(t *testing.T) {
	handshakeMsg := buildMinimalClientHelloHandshakeMsg()
	// Split across two TLS records to exercise reassembly.
	rec1 := buildTLSRecord(handshakeMsg[:20])
	rec2 := buildTLSRecord(handshakeMsg[20:])

	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()

	go func() {
		defer clientConn.Close()
		// Further fragment the first record's own header across separate
		// writes, simulating arbitrary TCP segmentation.
		clientConn.Write(rec1[:3])
		clientConn.Write(rec1[3:])
		clientConn.Write(rec2)
	}()

	buffered, fp := sniffClientHello(serverConn, 2*time.Second)

	if fp == "" {
		t.Fatal("expected a non-empty JA4 fingerprint for a well-formed, if split, ClientHello")
	}
	if want := len(rec1) + len(rec2); len(buffered) != want {
		t.Errorf("buffered %d bytes, want %d (both records, unmodified)", len(buffered), want)
	}
}

func TestSniffClientHello_NonTLSBailsOutWithoutHanging(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()

	go func() {
		defer clientConn.Close()
		clientConn.Write([]byte("GET / HTTP/1.1\r\n"))
	}()

	start := time.Now()
	buffered, fp := sniffClientHello(serverConn, 2*time.Second)
	elapsed := time.Since(start)

	if fp != "" {
		t.Errorf("fp = %q, want empty for plaintext (non-TLS) traffic", fp)
	}
	if len(buffered) == 0 {
		t.Error("expected the peeked bytes to be preserved so the caller can still forward them")
	}
	if elapsed > time.Second {
		t.Errorf("bailed out after %v, want near-immediate (should not wait for the full deadline)", elapsed)
	}
}

func TestSniffClientHello_NothingSentHonorsDeadline(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer serverConn.Close()
	defer clientConn.Close()

	start := time.Now()
	_, fp := sniffClientHello(serverConn, 100*time.Millisecond)
	elapsed := time.Since(start)

	if fp != "" {
		t.Errorf("fp = %q, want empty when nothing was ever sent", fp)
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("returned after %v, want it to honor the %v deadline", elapsed, 100*time.Millisecond)
	}
	if elapsed > time.Second {
		t.Errorf("returned after %v, deadline should bound this tightly", elapsed)
	}
}
