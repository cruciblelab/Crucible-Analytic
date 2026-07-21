package ja4

import (
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// TestFingerprint_MatchesFoxIOReference cross-validates this package's
// output against FoxIO's own reference implementation (the original
// authors of the JA4 spec), using real ClientHello bytes extracted from
// their official test pcaps and the expected JA4 values from their own
// checked-in test data. See testdata/README.md for exact provenance
// (source commit, extraction command) for every fixture.
//
// This is the validation the Fingerprint doc comment refers to: earlier,
// unvalidated versions of partB/partC/alpnToken passed every
// self-consistency test in ja4_test.go while still disagreeing with the
// reference on two real inputs here (see git history / the fix commit for
// what changed and why).
func TestFingerprint_MatchesFoxIOReference(t *testing.T) {
	cases := []struct {
		name    string
		hexFile string
		wantJA4 string
	}{
		{
			name:    "tls12.pcap: real browser, TLS1.3 via supported_versions",
			hexFile: "testdata/foxio_tls12.hex",
			wantJA4: "t13d1715h2_5b57614c22b0_3d5424432f57",
		},
		{
			name:    "tls-non-ascii-alpn.pcapng: first ALPN byte is non-ASCII (0xba)",
			hexFile: "testdata/foxio_tls_non_ascii_alpn.hex",
			wantJA4: "t13d151699_8daaf6152771_e5627efa2ab1",
		},
		{
			name:    "tls-alpn-h2.pcap: 46 ciphers, explicit h2 ALPN, reserved (non-GREASE) sig-alg codes",
			hexFile: "testdata/foxio_tls_alpn_h2.hex",
			wantJA4: "t12d4605h2_85626a9a5f7f_aaf95bb78ec9",
		},
		{
			name:    "https3-301-get.pcap: TLS1.0, SNI-only extension, empty JA4_c input",
			hexFile: "testdata/foxio_https3_301_get.hex",
			wantJA4: "t10d230100_6a57a6f57151_000000000000",
		},
		{
			name:    "tls-sni.pcapng stream 0: Chrome to googleapis.com",
			hexFile: "testdata/foxio_tls_sni_stream0.hex",
			wantJA4: "t13d1516h2_8daaf6152771_e5627efa2ab1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recordBytes := readHexFixture(t, tc.hexFile)
			// Fixtures are the full TLS record (5-byte header + handshake
			// message); ParseClientHello expects just the handshake
			// message, matching what the proxy package hands it after
			// stripping record framing.
			if len(recordBytes) < 5 {
				t.Fatalf("fixture %s too short to contain a TLS record header", tc.hexFile)
			}
			handshakeMsg := recordBytes[5:]

			ch, err := ParseClientHello(handshakeMsg)
			if err != nil {
				t.Fatalf("ParseClientHello: %v", err)
			}

			if got := Fingerprint(ch); got != tc.wantJA4 {
				t.Errorf("Fingerprint() = %q, want %q (FoxIO reference)", got, tc.wantJA4)
			}
		})
	}
}

func readHexFixture(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", path, err)
	}
	b, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("decoding fixture %s: %v", path, err)
	}
	return b
}
