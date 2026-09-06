// Package textsafe holds the one rule about strings that this product
// applies to everything it did not write itself.
//
// # What the rule is
//
// A string is storable when it is valid UTF-8 and carries no control
// characters. Both halves are correctness rather than tidiness, and each
// has its own consequence:
//
//   - PostgreSQL TEXT cannot hold a NUL byte or invalid UTF-8 at all,
//     and rejects the whole statement when handed one. Rows are written
//     in batches, so a single bad value does not spoil its own row - it
//     spoils every row in the batch it landed in.
//   - A control character in a log value splits one JSON line into two,
//     and the second half - chosen by whoever sent the request - parses
//     as a record of its own. That is log injection: forged entries in
//     the file an operator reads to find out what an attacker did.
//
// # Why it is a package
//
// Because it was written out three times, and the third one is how it
// got here.
//
// internal/beacon and internal/logging each carried a copy, identical
// line for line in the part below and differing only in what they do
// afterwards - one trims, the other appends an ellipsis when it cuts.
// Then a fuzz target found that internal/asnlookup had *no* copy: the
// organisation name from a downloaded CSV reached the database with no
// pass at all, and that value is attached to every row both data sources
// write while the table is loaded.
//
// Writing a third copy would have fixed that one field and left the next
// parser to be discovered the same way. This project has already been
// caught by two spellings of one rule drifting apart - see
// backup.MarginFor - and the fix there was the same as the fix here.
//
// # What is not here
//
// Truncation. The two existing callers cut differently on purpose: a
// stored title must not gain a character it did not have, and a log
// value should say that it was cut. One function that did both would
// take a flag, and a flag is two functions wearing one name.
//
// *Bir kuralın iki yazımı varsa, bakılmayan yazım er geç ayrılır.*
package textsafe

import (
	"strings"
	"unicode/utf8"
)

// Storable returns s with invalid UTF-8 removed and control characters
// stripped.
//
// Stripped rather than replaced, and stripped rather than refused. A
// value arriving here has already been accepted by whatever decided it
// was worth keeping; the question at this point is only whether it can
// be written down, and dropping the bytes that cannot be written keeps
// the rest of the value rather than losing the row.
func Storable(s string) string {
	if s == "" {
		return ""
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	return strings.Map(func(r rune) rune {
		// The ASCII control range, DEL, and the two Unicode separators.
		//
		// The separators are here for the same reason as the rest even
		// though they are neither ASCII nor invalid: they are invisible
		// in a panel, some JSON readers treat them as line breaks, and
		// they break line-oriented CSV output downstream.
		if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
			return -1
		}
		return r
	}, s)
}
