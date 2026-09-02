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
