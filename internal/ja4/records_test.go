package ja4

import (
	"errors"
	"testing"
)

func tlsRecord(payload []byte) []byte {
	rec := make([]byte, recordHeaderLen+len(payload))
	rec[0] = recordTypeHandshake
	rec[1], rec[2] = 0x03, 0x01
	rec[3] = byte(len(payload) >> 8)
	rec[4] = byte(len(payload))
	copy(rec[5:], payload)
	return rec
}

// minimalHandshakeMsg builds a structurally valid, minimal ClientHello
// handshake message (one cipher suite, no extensions).
func minimalHandshakeMsg() []byte {
	var body []byte
	body = append(body, 0x03, 0x03) // legacy_version: TLS 1.2
	body = append(body, make([]byte, 32)...)
	body = append(body, 0x00)                   // session_id length
	body = append(body, 0x00, 0x02, 0x13, 0x01) // cipher_suites: len=2, one suite
	body = append(body, 0x01, 0x00)             // compression: len=1, null
	body = append(body, 0x00, 0x00)             // extensions: total length 0

	msg := []byte{0x01, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	return append(msg, body...)
}

func TestParseClientHelloFromRecords_SingleRecord(t *testing.T) {
	raw := tlsRecord(minimalHandshakeMsg())

	ch, err := ParseClientHelloFromRecords(raw)
	if err != nil {
		t.Fatalf("ParseClientHelloFromRecords: %v", err)
	}
	if Fingerprint(ch) == "" {
		t.Error("Fingerprint() = \"\", want a non-empty JA4 for a well-formed ClientHello")
	}
}

func TestParseClientHelloFromRecords_SplitAcrossTwoRecords(t *testing.T) {
	msg := minimalHandshakeMsg()
	raw := append(tlsRecord(msg[:20]), tlsRecord(msg[20:])...)

	ch, err := ParseClientHelloFromRecords(raw)
	if err != nil {
		t.Fatalf("ParseClientHelloFromRecords: %v", err)
	}

	direct, err := ParseClientHello(msg)
	if err != nil {
		t.Fatalf("ParseClientHello (direct): %v", err)
	}
	if Fingerprint(ch) != Fingerprint(direct) {
		t.Errorf("Fingerprint from split records = %q, want %q (same as parsing the unsplit message directly)", Fingerprint(ch), Fingerprint(direct))
	}
}

func TestParseClientHelloFromRecords_IgnoresTrailingBytes(t *testing.T) {
	raw := append(tlsRecord(minimalHandshakeMsg()), []byte{0xde, 0xad, 0xbe, 0xef}...)

	if _, err := ParseClientHelloFromRecords(raw); err != nil {
		t.Fatalf("ParseClientHelloFromRecords with trailing garbage: %v", err)
	}
}

func TestParseClientHelloFromRecords_Empty(t *testing.T) {
	if _, err := ParseClientHelloFromRecords(nil); !errors.Is(err, ErrTruncated) {
		t.Errorf("err = %v, want ErrTruncated for empty input", err)
	}
}

func TestParseClientHelloFromRecords_NotAHandshakeRecord(t *testing.T) {
	raw := []byte{0x17, 0x03, 0x01, 0x00, 0x01, 0xff} // content type 0x17 = application_data
	if _, err := ParseClientHelloFromRecords(raw); !errors.Is(err, ErrNotClientHello) {
		t.Errorf("err = %v, want ErrNotClientHello", err)
	}
}

func TestParseClientHelloFromRecords_TruncatedMidRecord(t *testing.T) {
	full := tlsRecord(minimalHandshakeMsg())
	for cut := 0; cut < len(full); cut++ {
		if _, err := ParseClientHelloFromRecords(full[:cut]); err == nil {
			t.Errorf("cut at %d/%d bytes: expected error, got nil", cut, len(full))
		}
	}
}

// TestParseClientHelloFromRecords_MatchesFoxIOFixture cross-checks against
// one of the real FoxIO reference fixtures (see foxio_reference_test.go /
// testdata/README.md for provenance): those .hex files are full TLS
// records, so they're valid direct input here, unlike
// TestFingerprint_MatchesFoxIOReference which strips the record header
// first to call ParseClientHello directly.
func TestParseClientHelloFromRecords_MatchesFoxIOFixture(t *testing.T) {
	raw := readHexFixture(t, "testdata/foxio_tls12.hex")

	ch, err := ParseClientHelloFromRecords(raw)
	if err != nil {
		t.Fatalf("ParseClientHelloFromRecords: %v", err)
	}

	const want = "t13d1715h2_5b57614c22b0_3d5424432f57"
	if got := Fingerprint(ch); got != want {
		t.Errorf("Fingerprint() = %q, want %q", got, want)
	}
}
