package logging

import (
	"strings"
	"unicode/utf8"
)

// Log lines are written from values this project does not control: user
// agents, paths, referrers, campaign parameters, the email somebody
// typed into a login form. Two separate things have to be true about
// every one of them before it reaches a file.

// maxValueLen bounds one logged string. Long enough for a real user
// agent or URL, short enough that a client cannot choose how much disk
// one record costs.
const maxValueLen = 2048

// secretKeys are attribute names whose value is never written, whatever
// it contains.
//
// This is a backstop, not the mechanism. The mechanism is not passing
// secrets to a logger in the first place, and every call site in this
// project follows it. Backstops exist because the call site added in a
// hurry two years from now will not, and a password in a log file is
// permanent in a way most mistakes are not - it survives in backups,
// in whatever the panel later ships to us, and in the operator's
// terminal scrollback.
var secretKeys = []string{
	"password", "passwd", "secret", "token", "authorization", "auth",
	"cookie", "session", "csrf", "totp", "otp", "key", "credential",
	"salt", "hash", "signature", "bearer", "api_key", "apikey",
}

// Redacted replaces a secret value. Deliberately not empty: "the field
// was present and withheld" and "the field was absent" are different
// facts, and collapsing them would make a log harder to read during the
// incident it exists for.
const Redacted = "[redacted]"

// IsSecretKey reports whether an attribute name looks like it names a
// credential. Substring matching, so "session_token" and "user_password"
// are both caught.
func IsSecretKey(key string) bool {
	lower := strings.ToLower(key)
	for _, secret := range secretKeys {
		if strings.Contains(lower, secret) {
			return true
		}
	}
	return false
}

// SanitizeValue makes an arbitrary string safe to write as one JSON
// line: valid UTF-8, free of control characters, and bounded.
//
// The control-character pass is what keeps a log line a line. A value
// containing a newline would otherwise split one record into two, and
// the second half - chosen entirely by whoever sent the request - would
// parse as a record of its own. That is log injection: an attacker
// forging entries in the file the operator reads to find out what an
// attacker did.
func SanitizeValue(s string) string {
	if s == "" {
		return ""
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = strings.Map(func(r rune) rune {
		// The ASCII control range plus the Unicode line separators,
		// which some JSON readers treat as line breaks.
		if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
			return -1
		}
		return r
	}, s)
	return truncateRunes(s, maxValueLen)
}

func truncateRunes(s string, max int) string {
	if len(s) <= max { // bytes >= runes, so this is a cheap fast path
		return s
	}
	count := 0
	for i := range s {
		count++
		if count > max {
			return s[:i] + "…"
		}
	}
	return s
}
