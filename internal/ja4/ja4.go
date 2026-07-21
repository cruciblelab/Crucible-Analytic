package ja4

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// versionTokens maps a TLS wire version to its two-character JA4 token.
var versionTokens = map[uint16]string{
	0x0304: "13",
	0x0303: "12",
	0x0302: "11",
	0x0301: "10",
	0x0300: "s3",
	0x0002: "s2",
}

// Fingerprint computes the JA4 client TLS fingerprint for a parsed
// ClientHello, following the public JA4 specification
// (https://github.com/FoxIO-LLC/ja4). Only the "t" (TLS-over-TCP) transport
// is supported.
//
// This is a from-scratch implementation written to keep the collector
// dependency-free. It has been cross-validated against two independent
// implementations - FoxIO's own reference (python/ja4.py in FoxIO-LLC/ja4,
// the spec's original authors) and Wireshark/tshark's native JA4 dissector
// (a separate codebase) - using several of FoxIO's official test pcaps.
// See foxio_reference_test.go for the fixtures, exact source commit, and
// how the raw bytes were extracted.
//
// That process is also how the empty-hash-segment special case below and
// the exact ALPN-sanitization rule were derived: both diverged from an
// earlier, unvalidated version of this function, and both now match
// FoxIO. It also surfaced a real, unresolved disagreement between FoxIO
// and Wireshark on those same two edge cases; this implementation follows
// FoxIO (it wrote the spec), and TestFingerprint_WiresharkCrossValidation
// pins the discrepancy with an explanation rather than hiding it.
func Fingerprint(ch *ClientHello) string {
	return partA(ch) + "_" + partB(ch) + "_" + partC(ch)
}

// partA builds the 10-character human-readable prefix: transport, TLS
// version, SNI presence, cipher count, extension count, first ALPN value.
func partA(ch *ClientHello) string {
	sni := "i"
	if ch.HasSNI {
		sni = "d"
	}

	alpn := "00"
	if len(ch.ALPNProtocols) > 0 {
		alpn = alpnToken(ch.ALPNProtocols[0])
	}

	return fmt.Sprintf("t%s%s%02d%02d%s",
		negotiatedVersionToken(ch), sni, capCount(len(ch.CipherSuites)), capCount(len(ch.Extensions)), alpn)
}

// negotiatedVersionToken picks the highest advertised TLS version (GREASE
// already excluded from SupportedVersions by the parser) and maps it to
// its JA4 token, falling back to LegacyVersion for pre-TLS-1.3 clients that
// omit the supported_versions extension.
func negotiatedVersionToken(ch *ClientHello) string {
	best := ch.LegacyVersion
	for _, v := range ch.SupportedVersions {
		if v > best {
			best = v
		}
	}
	if tok, ok := versionTokens[best]; ok {
		return tok
	}
	return "00"
}

func capCount(n int) int {
	if n > 99 {
		return 99
	}
	return n
}

// alpnToken returns the first and last byte of the first offered ALPN
// protocol string, matching python/ja4.py's to_ja4() exactly: only the
// first (truncated) byte is checked, and if its value is non-ASCII
// (> 0x7f), the whole token becomes "99" rather than substituting byte by
// byte. A non-ASCII *last* byte with an ASCII first byte is not specially
// handled by the reference either, so neither is it here. A zero-length
// protocol name (malformed input) falls back to "00".
func alpnToken(proto string) string {
	if proto == "" {
		return "00"
	}
	first, last := proto[0], proto[len(proto)-1]
	if first > 0x7f {
		return "99"
	}
	return string([]byte{first, last})
}

// partB is the truncated SHA256 of the sorted, comma-joined, 4-hex-digit
// cipher suite list - or the literal "000000000000" if there are no
// ciphers, matching the reference implementation's explicit empty-input
// special case (it never hashes an empty string).
func partB(ch *ClientHello) string {
	if len(ch.CipherSuites) == 0 {
		return emptyHashSegment
	}
	sorted := sortedCopy(ch.CipherSuites)
	return truncatedSHA256(joinHex4(sorted))
}

// partC is the truncated SHA256 of the sorted, comma-joined, 4-hex-digit
// extension list (SNI and ALPN excluded), plus the wire-order signature
// algorithm list when present - or the literal "000000000000" if that
// combined input is empty (e.g. SNI was the only extension, and no
// signature_algorithms extension was sent), matching the reference
// implementation's explicit empty-input special case.
func partC(ch *ClientHello) string {
	var filtered []uint16
	for _, e := range ch.Extensions {
		if e == extServerName || e == extALPN {
			continue
		}
		filtered = append(filtered, e)
	}
	sorted := sortedCopy(filtered)

	raw := joinHex4(sorted)
	if len(ch.SignatureAlgorithms) > 0 {
		raw += "_" + joinHex4(ch.SignatureAlgorithms) // signature algorithms keep wire order, not sorted
	}
	if raw == "" {
		return emptyHashSegment
	}
	return truncatedSHA256(raw)
}

// emptyHashSegment is what the reference implementation emits in place of
// a JA4_b/JA4_c hash when the corresponding input list is empty.
const emptyHashSegment = "000000000000"

func sortedCopy(values []uint16) []uint16 {
	out := append([]uint16(nil), values...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func joinHex4(values []uint16) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = fmt.Sprintf("%04x", v)
	}
	return strings.Join(parts, ",")
}

func truncatedSHA256(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}
