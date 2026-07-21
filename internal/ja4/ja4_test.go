package ja4

import (
	"regexp"
	"testing"
)

func TestIsGrease(t *testing.T) {
	greaseValues := []uint16{
		0x0a0a, 0x1a1a, 0x2a2a, 0x3a3a, 0x4a4a, 0x5a5a, 0x6a6a, 0x7a7a,
		0x8a8a, 0x9a9a, 0xaaaa, 0xbaba, 0xcaca, 0xdada, 0xeaea, 0xfafa,
	}
	for _, v := range greaseValues {
		if !isGrease(v) {
			t.Errorf("isGrease(%#04x) = false, want true", v)
		}
	}

	nonGrease := []uint16{0x1301, 0x1302, 0x1303, 0xc02b, 0x0000, 0x0a0b, 0x002d}
	for _, v := range nonGrease {
		if isGrease(v) {
			t.Errorf("isGrease(%#04x) = true, want false", v)
		}
	}
}

func TestPartA(t *testing.T) {
	cases := []struct {
		name string
		ch   *ClientHello
		want string
	}{
		{
			name: "tls13 with sni and alpn",
			ch: &ClientHello{
				LegacyVersion:     0x0303,
				SupportedVersions: []uint16{0x0304},
				HasSNI:            true,
				CipherSuites:      []uint16{0x1301, 0x1302, 0x1303},
				Extensions:        []uint16{extServerName, extSupportedVersions, extALPN, 0x002d, 0x0033},
				ALPNProtocols:     []string{"h2"},
			},
			want: "t13d0305h2",
		},
		{
			name: "tls12 no sni no alpn",
			ch: &ClientHello{
				LegacyVersion: 0x0303,
				CipherSuites:  []uint16{0x002f, 0xc02b},
				Extensions:    []uint16{0x000b},
			},
			want: "t12i0201" + "00",
		},
		{
			name: "http/1.1 alpn uses first+last char",
			ch: &ClientHello{
				LegacyVersion: 0x0301,
				ALPNProtocols: []string{"http/1.1"},
			},
			want: "t10i0000h1",
		},
		{
			name: "single-char alpn repeats the char",
			ch: &ClientHello{
				LegacyVersion: 0x0301,
				ALPNProtocols: []string{"a"},
			},
			want: "t10i0000aa",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := partA(tc.ch); got != tc.want {
				t.Errorf("partA() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPartA_CountsAreCapped(t *testing.T) {
	ciphers := make([]uint16, 150)
	for i := range ciphers {
		ciphers[i] = uint16(0x1000 + i)
	}
	ch := &ClientHello{LegacyVersion: 0x0303, CipherSuites: ciphers}
	a := partA(ch)
	if got := a[4:6]; got != "99" {
		t.Errorf("cipher count = %q, want %q (capped at 99)", got, "99")
	}
}

func TestPartB_SortsCiphersBeforeHashing(t *testing.T) {
	ch1 := &ClientHello{CipherSuites: []uint16{0x1303, 0x1301, 0x1302}}
	ch2 := &ClientHello{CipherSuites: []uint16{0x1301, 0x1302, 0x1303}}
	if partB(ch1) != partB(ch2) {
		t.Errorf("partB differs for reordered cipher list: %s vs %s", partB(ch1), partB(ch2))
	}

	want := truncatedSHA256("1301,1302,1303")
	if got := partB(ch1); got != want {
		t.Errorf("partB() = %s, want %s", got, want)
	}
}

func TestPartC_ExcludesSNIAndALPN_KeepsSigAlgWireOrder(t *testing.T) {
	ch := &ClientHello{
		Extensions:          []uint16{extServerName, 0x0033, extALPN, 0x002d},
		SignatureAlgorithms: []uint16{0x0804, 0x0403}, // deliberately unsorted; must stay in wire order
	}
	want := truncatedSHA256("002d,0033" + "_" + "0804,0403")
	if got := partC(ch); got != want {
		t.Errorf("partC() = %s, want %s", got, want)
	}
}

func TestPartC_NoSignatureAlgorithms(t *testing.T) {
	ch := &ClientHello{Extensions: []uint16{0x0033}}
	want := truncatedSHA256("0033")
	if got := partC(ch); got != want {
		t.Errorf("partC() = %s, want %s (no trailing underscore when sig algs absent)", got, want)
	}
}

func TestFingerprint_Shape(t *testing.T) {
	ch := &ClientHello{
		LegacyVersion: 0x0303,
		CipherSuites:  []uint16{0x1301, 0x1302},
		Extensions:    []uint16{0x0033},
	}
	fp := Fingerprint(ch)
	if !regexp.MustCompile(`^t\w{2}[di]\d{2}\d{2}\w{2}_[0-9a-f]{12}_[0-9a-f]{12}$`).MatchString(fp) {
		t.Errorf("Fingerprint() = %q, does not match expected JA4 shape", fp)
	}
}

func TestFingerprint_Deterministic(t *testing.T) {
	ch := &ClientHello{
		LegacyVersion:       0x0303,
		SupportedVersions:   []uint16{0x0304},
		HasSNI:              true,
		CipherSuites:        []uint16{0x1301, 0x1302},
		Extensions:          []uint16{extServerName, 0x0033},
		SignatureAlgorithms: []uint16{0x0403},
		ALPNProtocols:       []string{"h2"},
	}
	if Fingerprint(ch) != Fingerprint(ch) {
		t.Error("Fingerprint() is not deterministic for identical input")
	}
}

func TestNegotiatedVersionToken_UnknownVersionFallsBack(t *testing.T) {
	ch := &ClientHello{LegacyVersion: 0x9999}
	if got := negotiatedVersionToken(ch); got != "00" {
		t.Errorf("negotiatedVersionToken() = %q, want %q for unrecognized version", got, "00")
	}
}
