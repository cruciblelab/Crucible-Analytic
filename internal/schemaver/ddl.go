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
	var out strings.Builder
	out.Grow(len(sql))

	// The states this scanner can be in. Everything inside a literal is
	// copied through untouched, because a `--` between quotes is data.
	const (
		code         = iota
		single       // '...' with '' as the escape
		identifier   // "..." with "" as the escape
		dollar       // $tag$...$tag$
		lineComment  // -- to end of line
		blockComment // /* ... */, and PostgreSQL nests these
	)

	state := code
	depth := 0 // block comment nesting
	tag := ""  // the active dollar-quote tag, including both $

	for i := 0; i < len(sql); {
		rest := sql[i:]

		switch state {
		case code:
			switch {
			case strings.HasPrefix(rest, "--"):
				state = lineComment
				i += 2
			case strings.HasPrefix(rest, "/*"):
				state, depth = blockComment, 1
				i += 2
			case rest[0] == '\'':
				state = single
				out.WriteByte(rest[0])
				i++
			case rest[0] == '"':
				state = identifier
				out.WriteByte(rest[0])
				i++
			default:
				// A dollar quote opens only where a tag actually
				// closes; a bare $ is legal elsewhere (parameters,
				// operator names), so this must not treat one as an
				// opener.
				if t, ok := dollarTag(rest); ok {
					state, tag = dollar, t
					out.WriteString(t)
					i += len(t)
					continue
				}
				out.WriteByte(rest[0])
				i++
			}

		case single, identifier:
			quote := byte('\'')
			if state == identifier {
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
				state = code
			}
			i++

		case dollar:
			if strings.HasPrefix(rest, tag) {
				out.WriteString(tag)
				i += len(tag)
				state, tag = code, ""
				continue
			}
			out.WriteByte(rest[0])
			i++

		case lineComment:
			if rest[0] == '\n' {
				state = code
				// The newline itself is kept: without it two lines of
				// DDL separated only by a trailing comment would run
				// together into one token.
				out.WriteByte('\n')
			}
			i++

		case blockComment:
			switch {
			case strings.HasPrefix(rest, "/*"):
				depth++
				i += 2
			case strings.HasPrefix(rest, "*/"):
				depth--
				i += 2
				if depth == 0 {
					state = code
					// Same reason as above: a block comment can sit
					// between two tokens.
					out.WriteByte(' ')
				}
			default:
				i++
			}
		}
	}

	return strings.Join(strings.Fields(out.String()), " ")
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
		letter := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		digit := c >= '0' && c <= '9'
		if letter || (digit && j > 1) {
			continue
		}
		return "", false
	}
	return "", false
}
