package ja4

// isGrease reports whether v is one of the 16 reserved GREASE values
// defined in RFC 8701. Clients (mainly Chromium-based browsers) randomly
// insert GREASE values into cipher suite, extension, version, and signature
// algorithm lists to prevent servers/middleboxes from ossifying around a
// fixed set of values. They carry no fingerprinting signal and must be
// filtered out before hashing, per the JA4 spec.
func isGrease(v uint16) bool {
	b := byte(v >> 8)
	return byte(v) == b && b&0x0f == 0x0a
}
