package schemaver

import (
	"strings"
	"testing"
)

// TestStripComments.
//
// The cases that are not obvious are the ones where a comment marker is
// not a comment. Getting those wrong is silent in the worst direction: a
// stripper that ate the rest of a file from inside a string literal
// would produce a stable fingerprint over a truncated schema, and every
// real DDL change after that point would go unnoticed.
func TestStripComments(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a line comment goes, the statement stays",
			in:   "-- a table\nCREATE TABLE t (id INT); -- trailing\n",
			want: "CREATE TABLE t (id INT);",
		},
		{
			name: "a comment between two tokens does not join them",
			in:   "CREATE TABLE t -- name\n(id INT);",
			want: "CREATE TABLE t (id INT);",
		},
		{
			name: "a block comment goes",
			in:   "CREATE /* why */ TABLE t (id INT);",
			want: "CREATE TABLE t (id INT);",
		},
		{
			name: "block comments nest, which they do in PostgreSQL and nowhere else",
			in:   "CREATE /* outer /* inner */ still a comment */ TABLE t (id INT);",
			want: "CREATE TABLE t (id INT);",
		},
		{
			name: "two dashes inside a string are data",
			in:   "INSERT INTO t VALUES ('a -- b');",
			want: "INSERT INTO t VALUES ('a -- b');",
		},
		{
			name: "a slash-star inside a string is data",
			in:   "INSERT INTO t VALUES ('a /* b');",
			want: "INSERT INTO t VALUES ('a /* b');",
		},
		{
			name: "a doubled quote does not end the string",
			in:   "INSERT INTO t VALUES ('it''s -- fine'); -- gone\n",
			want: "INSERT INTO t VALUES ('it''s -- fine');",
		},
		{
			name: "a quoted identifier is left alone",
			in:   `CREATE TABLE "odd -- name" (id INT); -- gone`,
			want: `CREATE TABLE "odd -- name" (id INT);`,
		},
		{
			name: "a dollar-quoted body keeps its own comments",
			in: "CREATE FUNCTION f() RETURNS void AS $$\n" +
				"BEGIN\n  -- kept: this is the function's text\n  RAISE NOTICE 'x';\nEND;\n" +
				"$$ LANGUAGE plpgsql; -- gone\n",
			want: "CREATE FUNCTION f() RETURNS void AS $$ BEGIN " +
				"-- kept: this is the function's text RAISE NOTICE 'x'; END; $$ LANGUAGE plpgsql;",
		},
		{
			name: "a tagged dollar quote closes on its own tag",
			in:   "DO $body$ SELECT '$$'; $body$; -- gone",
			want: "DO $body$ SELECT '$$'; $body$;",
		},
		{
			name: "a parameter placeholder is not a dollar quote",
			in:   "SELECT * FROM t WHERE id = $1; -- gone\nSELECT 2;",
			want: "SELECT * FROM t WHERE id = $1; SELECT 2;",
		},
		{
			name: "whitespace is collapsed so reflowing prose changes nothing",
			in:   "CREATE   TABLE\n\n   t (id INT);",
			want: "CREATE TABLE t (id INT);",
		},
		{
			name: "a file that is only comments strips to nothing",
			in:   "-- one\n-- two\n/* three */\n",
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripComments(tc.in); got != tc.want {
				t.Errorf("StripComments()\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestStrippingIsIdempotent.
//
// Stripping something already stripped must not change it. A stripper
// that removed a little more on a second pass would mean the fingerprint
// depended on how many times it had been applied, which is the one
// property a fingerprint cannot have.
func TestStrippingIsIdempotent(t *testing.T) {
	for path, body := range schemaFilesOnDisk(t) {
		once := StripComments(body)
		if twice := StripComments(once); twice != once {
			t.Errorf("%s: stripping twice differs from stripping once", path)
		}
	}
}

// TestTheRealSchemaFilesStripToDDL runs the scanner over the corpus it
// actually has to survive - ten files carrying dollar-quoted PL/pgSQL,
// quoted identifiers and thousands of words of prose.
//
// It asserts two things a table of small cases cannot. That every file
// still has DDL left, so a scanner that ran away inside a literal shows
// up here rather than as a fingerprint nobody questions. And that the
// stripping did real work, because a scanner that removed nothing would
// pass every case above that has nothing to remove.
func TestTheRealSchemaFilesStripToDDL(t *testing.T) {
	files := schemaFilesOnDisk(t)
	if len(files) < 5 {
		t.Fatalf("only %d schema files found; this test is not reading the corpus", len(files))
	}

	for path, body := range files {
		ddl := StripComments(body)
		if ddl == "" {
			t.Errorf("%s strips to nothing at all", path)
			continue
		}
		if !strings.Contains(strings.ToUpper(ddl), "CREATE") {
			t.Errorf("%s has no CREATE left after stripping:\n%.200s", path, ddl)
		}
		if len(ddl) >= len(body) {
			t.Errorf("%s did not shrink (%d -> %d); every schema file in this "+
				"repository is more comment than statement, so a file that does not "+
				"shrink means nothing was stripped", path, len(body), len(ddl))
		}
		// Whatever survives cannot contain a comment opener outside a
		// literal. Checking the whole string would trip over the
		// PL/pgSQL bodies, which keep theirs on purpose, so this checks
		// the part before the first dollar quote.
		head, _, _ := strings.Cut(ddl, "$")
		if strings.Contains(head, "--") || strings.Contains(head, "/*") {
			t.Errorf("%s still carries a comment marker in its plain DDL:\n%.200s", path, head)
		}
	}
}

// TestACommentChangeDoesNotMoveTheFingerprint is the property this whole
// file exists to provide, asserted directly rather than trusted.
func TestACommentChangeDoesNotMoveTheFingerprint(t *testing.T) {
	base := map[string]string{
		"a/schema.sql": "-- first\nCREATE TABLE t (id INT);\n",
	}
	commented := map[string]string{
		"a/schema.sql": "-- first, corrected, and at greater length\n" +
			"-- with a second line nobody had written before\n" +
			"CREATE TABLE t (id INT);\n",
	}
	if FingerprintOf(base) != FingerprintOf(commented) {
		t.Error("rewriting a comment moved the fingerprint, which is what putting " +
			"StripComments in front of it was meant to stop")
	}

	changed := map[string]string{
		"a/schema.sql": "-- first\nCREATE TABLE t (id BIGINT);\n",
	}
	if FingerprintOf(base) == FingerprintOf(changed) {
		t.Error("changing a column type did not move the fingerprint; the stripper " +
			"is removing more than comments")
	}

	moved := map[string]string{
		"b/schema.sql": base["a/schema.sql"],
	}
	if FingerprintOf(base) == FingerprintOf(moved) {
		t.Error("moving a schema file between packages did not move the fingerprint")
	}
}

// TestTheScannerSurvivesTheShapesThatEndScanners.
//
// Everything here is a case where a comment marker is not a comment, or
// a dollar is not a quote. Each one, got wrong, is silent in the same
// direction: the scanner runs away inside a literal, the rest of the
// file is swallowed, and the fingerprint is computed over a truncated
// schema that hashes perfectly stably forever after.
//
// The corpus already contains one of these, in a comment where it is
// harmless: internal/panel/schema.sql documents an argon2id hash as
// $argon2id$v=19$m=...$salt$hash. Moved one line up, out of the comment,
// that string opens a dollar quote that never closes.
func TestTheScannerSurvivesTheShapesThatEndScanners(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "a dollar inside an identifier is not a quote",
			in:   "SELECT a$b$c FROM t; -- gone",
			want: "SELECT a$b$c FROM t;",
		},
		{
			name: "an argon2id hash outside a comment does not open a quote",
			in:   "INSERT INTO t VALUES (x$argon2id$v); -- gone",
			want: "INSERT INTO t VALUES (x$argon2id$v);",
		},
		{
			name: "an E-string's escaped quote does not end it",
			in:   `SELECT E'a\'-- still a string' FROM t; -- gone`,
			want: `SELECT E'a\'-- still a string' FROM t;`,
		},
		{
			name: "an escaped backslash at the end of an E-string still ends it",
			in:   `SELECT E'a\\' FROM t; -- gone`,
			want: `SELECT E'a\\' FROM t;`,
		},
		{
			name: "an ordinary string does not treat backslash as an escape",
			in:   `SELECT 'a\' AS x FROM t; -- gone`,
			want: `SELECT 'a\' AS x FROM t;`,
		},
		{
			name: "an identifier ending in e does not make the next string an E-string",
			in:   `SELECT type'a\' FROM t; -- gone`,
			want: `SELECT type'a\' FROM t;`,
		},
		{
			name: "a dollar quote still opens after an operator",
			in:   "DO $$ SELECT 1; $$; -- gone",
			want: "DO $$ SELECT 1; $$;",
		},
		{
			name: "a dollar quote opens at the very start of the input",
			in:   "$$ x $$ -- gone",
			want: "$$ x $$",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripComments(tc.in); got != tc.want {
				t.Errorf("StripComments()\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestCompleteReportsUnclosedConstructs.
func TestCompleteReportsUnclosedConstructs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"ordinary DDL", "CREATE TABLE t (id INT);", true},
		{"a comment running to end of file", "CREATE TABLE t (id INT); -- and then", true},
		{"an unclosed block comment", "CREATE TABLE t (id INT); /* and then", false},
		{"a block comment closed one level short", "/* a /* b */ CREATE TABLE t ();", false},
		{"an unclosed string", "INSERT INTO t VALUES ('abc", false},
		{"an unclosed quoted identifier", `CREATE TABLE "abc`, false},
		{"an unclosed dollar quote", "DO $$ SELECT 1;", false},
		{"an unclosed E-string", `SELECT E'abc\'`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Complete(tc.in); got != tc.want {
				t.Errorf("Complete(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestEverySchemaFileIsLexicallyComplete.
//
// The one failure this scanner can have that nothing else would notice.
// A file ending inside a block comment or a literal still produces a
// fingerprint - a perfectly stable one, over a truncated file - and
// every real DDL change after the unclosed marker would leave it
// unmoved. PostgreSQL would refuse such a file, but nothing between here
// and PostgreSQL looks at it, so this is the only place it gets caught.
func TestEverySchemaFileIsLexicallyComplete(t *testing.T) {
	for path, body := range schemaFilesOnDisk(t) {
		if !Complete(body) {
			t.Errorf("%s ends inside an unclosed comment or literal.\n"+
				"Its fingerprint is being taken over whatever came before that point, "+
				"and would not move if the rest of the file changed", path)
		}
	}
}

// FuzzStripComments.
//
// The scanner reads bytes nobody validated, and it sits in front of the
// value the upgrade machinery trusts. It cannot crash a service - it
// runs at build time and in the gate, not in a request path - so the
// risk here is not availability but silence: a shape that makes it drop
// DDL it should keep.
//
// Three properties, and the third is the one worth having. If the input
// contains no comment marker at all, the output must be the input with
// whitespace collapsed and nothing else: any byte that goes missing came
// from the scanner mistaking something for a comment.
//
//	go test -run XXX -fuzz FuzzStripComments ./internal/schemaver/
func FuzzStripComments(f *testing.F) {
	for _, body := range schemaFilesOnDisk(f) {
		f.Add(body)
	}
	for _, seed := range []string{
		"", "--", "/*", "'", `"`, "$$", "$a$", "E'\\'",
		"CREATE TABLE t (id INT); -- x\n",
		"DO $$ BEGIN RAISE '-- x'; END; $$;",
		"SELECT a$b$c;",
		"/* a /* b */ c */ SELECT 1;",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, in string) {
		out := StripComments(in)

		if twice := StripComments(out); twice != out {
			t.Errorf("not idempotent\n in: %q\nout: %q\n  2: %q", in, out, twice)
		}
		if len(out) > len(in) {
			t.Errorf("output grew: %d -> %d\n in: %q\nout: %q", len(in), len(out), in, out)
		}
		if !strings.Contains(in, "--") && !strings.Contains(in, "/*") {
			if want := strings.Join(strings.Fields(in), " "); out != want {
				t.Errorf("input carries no comment marker and bytes went missing\n"+
					" in: %q\nout: %q\nwant: %q", in, out, want)
			}
		}
	})
}
