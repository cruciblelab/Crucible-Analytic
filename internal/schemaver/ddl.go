package schemaver

import "strings"

// StripComments reduces a schema file to the DDL a database actually
// receives: comments removed, runs of whitespace collapsed to one space.
//
// # Why the fingerprint is taken over this and not over the file
//
// It was taken over the raw bytes until 2026-09-02, and the day two
// schema files had their prose corrected - not a line of their DDL - the
// cost of that showed up in full.
//
// Fingerprint is what State.Matches compares, Matches is what the health
// page and the upgrade button both read, and neither of them can tell a
// comment from a column. So a corrected sentence in a comment would have
// put "your schema does not match" and an upgrade button, gated behind
// the developer password, in front of every installation - for prose the
// database never receives. The obvious alternative, leaving the comment
// wrong, is worse: internal/beacon/schema.sql was describing a storage
// mode that had been replaced, in the file a DBA reads first.
//
// Hashing the DDL answers the question the constant claims to answer.
// "Is what is installed really this?" is about what was installed, and
// what is installed is this.
//
// # The cost, stated plainly
//
// Changing the rule changes every hash once, so this build reports a
// mismatch against every database installed before it, and each of them
// is asked to upgrade once. That is a real cost and it is paid a single
// time; the alternative pays it again on every future comment.
//
// # What is deliberately not normalised
//
// Case, identifier quoting, and statement order are all left alone. Two
// files that differ in those ways are different DDL - `CREATE TABLE foo`
// and `CREATE TABLE "Foo"` name different tables - and a normaliser that
// folded them would be answering a question nobody asked while hiding a
// difference that matters.
func StripComments(sql string) string {
	out, _ := stripComments(sql)
	return out
}

// Complete reports whether every literal and comment in sql is closed.
//
// # Why this is separate, and why anything reads it
//
// stripComments is total: handed a file that ends inside a block comment
// it returns what it had, because a fingerprint function cannot refuse.
// That is the one way this scanner can lose real DDL without saying so -
// a `/*` typed where `*/` was meant swallows everything after it, the
// hash is computed over the truncated remainder, and it looks exactly
// like a hash of a shorter file.
//
// PostgreSQL would reject such a file outright, so it can only reach the
// fingerprint by never having been applied. That is not reassurance
// worth relying on: the gate hashes these files on every run, and
// nothing else in the pipeline reads them at all. So the check lives
// here and the mirror test runs it over the corpus.
func Complete(sql string) bool {
	_, complete := stripComments(sql)
	return complete
}

// The states the scanner can be in. Everything inside a literal is
// copied through untouched, because a `--` between quotes is data.
const (
	stateCode         = iota
	stateSingle       // '...', with '' as the escape
	stateEscapeString // E'...', where a backslash escapes as well
	stateIdentifier   // "...", with "" as the escape
	stateDollar       // $tag$...$tag$
	stateLineComment  // -- to end of line
	stateBlockComment // /* ... */, and PostgreSQL nests these
)

// stripComments is the scanner. The second result is false when the
// input ended inside a literal or an unclosed block comment.
func stripComments(sql string) (string, bool) {
	var out strings.Builder
	out.Grow(len(sql))

	state := stateCode
	depth := 0 // block comment nesting
	tag := ""  // the active dollar-quote tag, including both $

	// The last two bytes emitted in code state. prev is how the scanner
	// tells a dollar-quote opener from a dollar inside a name; both are
	// needed for the E-string prefix, because `E` only introduces one
	// when it is a token of its own. Zero at the start of the file,
	// which is a boundary.
	var prev, pprev byte
	shift := func(c byte) { pprev, prev = prev, c }

	for i := 0; i < len(sql); {
		rest := sql[i:]

		switch state {
		case stateCode:
			switch {
			case strings.HasPrefix(rest, "--"):
				state = stateLineComment
				i += 2
			case strings.HasPrefix(rest, "/*"):
				state, depth = stateBlockComment, 1
				i += 2
			case rest[0] == '\'':
				// E'...' gives backslash its escaping meaning back.
				// Ordinary strings do not have it - PostgreSQL has
				// shipped standard_conforming_strings on since 9.1 -
				// so treating every string as escaping would end
				// literals in the wrong place.
				//
				// The E has to be a token of its own. Measured, because
				// the first version of this check looked only at the
				// byte before the quote: `SELECT type'a\'` ends with an
				// identifier whose last letter is e, and reading that
				// as an E-string made the backslash escape the closing
				// quote - so the string never ended and the rest of the
				// file went with it.
				state = stateSingle
				if (prev == 'E' || prev == 'e') && !identByte(pprev) {
					state = stateEscapeString
				}
				out.WriteByte(rest[0])
				shift(rest[0])
				i++
			case rest[0] == '"':
				state = stateIdentifier
				out.WriteByte(rest[0])
				shift(rest[0])
				i++
			default:
				// A dollar quote opens only at a token boundary. Inside
				// a name it is an ordinary character: `a$b$c` is one
				// identifier, and a scanner that read `$b$` there as an
				// opener would swallow the rest of the file looking for
				// a tag that never closes.
				if !identByte(prev) {
					if t, ok := dollarTag(rest); ok {
						state, tag = stateDollar, t
						out.WriteString(t)
						shift('$')
						i += len(t)
						continue
					}
				}
				out.WriteByte(rest[0])
				shift(rest[0])
				i++
			}

		case stateSingle, stateIdentifier:
			quote := byte('\'')
			if state == stateIdentifier {
				quote = '"'
			}
			out.WriteByte(rest[0])
			if rest[0] == quote {
				// A doubled quote is an escaped one, so consume both
				// and stay inside the literal.
				if len(rest) > 1 && rest[1] == quote {
					out.WriteByte(rest[1])
					i += 2
					continue
				}
				state = stateCode
				shift(quote)
			}
			i++

		case stateEscapeString:
			// A backslash consumes whatever follows it, including a
			// quote and including another backslash.
			if rest[0] == '\\' && len(rest) > 1 {
				out.WriteString(rest[:2])
				i += 2
				continue
			}
			out.WriteByte(rest[0])
			if rest[0] == '\'' {
				if len(rest) > 1 && rest[1] == '\'' {
					out.WriteByte(rest[1])
					i += 2
					continue
				}
				state = stateCode
				shift('\'')
			}
			i++

		case stateDollar:
			if strings.HasPrefix(rest, tag) {
				out.WriteString(tag)
				i += len(tag)
				state, tag = stateCode, ""
				shift('$')
				continue
			}
			out.WriteByte(rest[0])
			i++

		case stateLineComment:
			if rest[0] == '\n' {
				state = stateCode
				// The newline itself is kept: without it two lines of
				// DDL separated only by a trailing comment would run
				// together into one token.
				out.WriteByte('\n')
				shift('\n')
			}
			i++

		case stateBlockComment:
			switch {
			case strings.HasPrefix(rest, "/*"):
				depth++
				i += 2
			case strings.HasPrefix(rest, "*/"):
				depth--
				i += 2
				if depth == 0 {
					state = stateCode
					// Same reason as above: a block comment can sit
					// between two tokens.
					out.WriteByte(' ')
					shift(' ')
				}
			default:
				i++
			}
		}
	}

	// A line comment running to end of file is closed by the file
	// ending, which is what PostgreSQL does too. Every other state means
	// something was left open.
	complete := state == stateCode || state == stateLineComment
	return strings.Join(strings.Fields(out.String()), " "), complete
}

// identByte reports whether c can appear inside an unquoted identifier.
// Dollar included: PostgreSQL allows it after the first character, which
// is the whole reason the boundary check above exists.
func identByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_' || c == '$':
		return true
	// Every byte of a multi-byte UTF-8 sequence has its high bit set,
	// and PostgreSQL allows those in identifiers. Treating them as
	// identifier bytes keeps a dollar after an accented name from
	// looking like a token boundary.
	case c >= 0x80:
		return true
	}
	return false
}

// dollarTag reports whether s opens a dollar-quoted string, and returns
// the tag including both dollars.
//
// The tag is $$ or $name$, where name starts with a letter or underscore
// and continues with letters, digits or underscores. Anything else - $1,
// a lone $ - is not an opener and must fall through to ordinary code, or
// a parameter placeholder would swallow the rest of the file.
func dollarTag(s string) (string, bool) {
	if len(s) == 0 || s[0] != '$' {
		return "", false
	}
	for j := 1; j < len(s); j++ {
		c := s[j]
		if c == '$' {
			return s[:j+1], true
		}
		letter := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
		digit := c >= '0' && c <= '9'
		if letter || (digit && j > 1) {
			continue
		}
		return "", false
	}
	return "", false
}
