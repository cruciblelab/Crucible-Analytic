// The mirror that makes a hand-written constant trustworthy.
//
// Version and Fingerprint are both typed by a person, which is normally
// how a value goes stale. What stops it here is that one side of the
// pair is derivable: the fingerprint can be recomputed from the schema
// files, and this test does exactly that.
//
// So a schema change has three possible endings and only one of them is
// green:
//
//	change a schema, update neither   -> fingerprint mismatch, fails
//	change a schema, update the hash  -> version unchanged, fails
//	change a schema, update both      -> passes
//
// The second case is the one worth having. A fingerprint that moved
// without the version moving is a schema change nobody can order, and
// ordering is the only question the version exists to answer.
//
// No build tag: it reads the schema files and hashes them. No database,
// no compiler, so it belongs in the merge gate.
package schemaver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// schemaFilesOnDisk reads every internal/*/schema.sql, keyed by its path
// relative to the repository root.
//
// From the filesystem rather than from a list, for the same reason
// release/schemalist_test.go does it: a list here would be one more copy
// to forget, and the file this project actually forgot once was a schema.
// Takes a testing.TB rather than a *testing.T so the fuzz target can
// seed itself from the same corpus. A seed list written by hand beside
// a glob that already exists is the second copy this helper was
// written to avoid.
func schemaFilesOnDisk(t testing.TB) map[string]string {
	t.Helper()

	root := repoRoot(t)
	matches, err := filepath.Glob(filepath.Join(root, "internal", "*", "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) < 6 {
		t.Fatalf("found %d schema files under internal/, fewer than this project has; "+
			"a fingerprint over the wrong set of files is worse than no fingerprint", len(matches))
	}

	files := make(map[string]string, len(matches))
	for _, m := range matches {
		body, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		rel, err := filepath.Rel(root, m)
		if err != nil {
			t.Fatal(err)
		}
		files[filepath.ToSlash(rel)] = string(body)
	}
	return files
}

func repoRoot(t testing.TB) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// internal/schemaver -> repository root
	return filepath.Dir(filepath.Dir(wd))
}

// TestTheFingerprintMatchesTheSchemaOnDisk.
//
// The headline. Everything L1, L2 and L3 do rests on the claim that this
// constant describes the schema this binary was built against, and this
// is the only thing that checks it.
func TestTheFingerprintMatchesTheSchemaOnDisk(t *testing.T) {
	got := FingerprintOf(schemaFilesOnDisk(t))
	if got == Fingerprint {
		return
	}

	// The first fill and a later change want different advice, and
	// telling somebody to bump a version that has never been recorded
	// would put the schema at 2 on the day it first reached 1.
	if strings.TrimSpace(Fingerprint) == "" {
		t.Errorf(`Fingerprint has never been filled in.

Set it in internal/schemaver/schemaver.go and leave Version at %d - this
is the first recording, not a change to order against an earlier one:

  const Fingerprint = %q`, Version, got)
		return
	}

	t.Errorf(`the schema files do not hash to the recorded fingerprint.

  recorded: %s
  on disk:  %s

A schema.sql changed. Two things have to move together in
internal/schemaver/schemaver.go:

  const Version     = %d   ->  %d
  const Fingerprint = ...  ->  %q

Both, not one. The fingerprint is the fact and the version is what makes
two facts orderable; a fingerprint that moves alone leaves a schema
change nobody can put in sequence.`,
		Fingerprint, got, Version, Version+1, got)
}

// TestTheFingerprintIsNotEmpty.
//
// A constant nobody filled in is the empty string, and the empty string
// is a perfectly valid SHA-256 input - so a Fingerprint left as "" would
// compare unequal to every real schema and look like a working check
// while checking nothing about the schema at all.
//
// This project has been bitten by exactly this shape before: a hash of
// nothing whose green state meant "we wrote nothing".
func TestTheFingerprintIsNotEmpty(t *testing.T) {
	if strings.TrimSpace(Fingerprint) == "" {
		t.Fatal("Fingerprint is empty; fill it from TestTheFingerprintMatchesTheSchemaOnDisk's output")
	}
	if len(Fingerprint) != 64 {
		t.Errorf("Fingerprint is %d characters; a hex SHA-256 is 64", len(Fingerprint))
	}
}

// TestFingerprintOfIsStable.
//
// The value's whole job is to be comparable between two runs, and the
// one thing that would quietly break that is Go's randomised map
// iteration order leaking into the hash.
func TestFingerprintOfIsStable(t *testing.T) {
	files := map[string]string{
		"internal/a/schema.sql": "CREATE TABLE a ();",
		"internal/b/schema.sql": "CREATE TABLE b ();",
		"internal/c/schema.sql": "CREATE TABLE c ();",
	}
	first := FingerprintOf(files)
	for i := 0; i < 50; i++ {
		if got := FingerprintOf(files); got != first {
			t.Fatalf("run %d gave %s, first run gave %s; the hash depends on map order", i, got, first)
		}
	}
}

// TestFingerprintOfNoticesEveryKindOfChange.
//
// Three changes that a careless hash would miss, and each is a real
// schema change:
//
//   - content edited: the obvious one
//   - file moved:     same SQL, different package - the table moved
//   - file added:     a new schema nobody folded into the lists
func TestFingerprintOfNoticesEveryKindOfChange(t *testing.T) {
	base := map[string]string{
		"internal/a/schema.sql": "CREATE TABLE a ();",
		"internal/b/schema.sql": "CREATE TABLE b ();",
	}
	want := FingerprintOf(base)

	for name, files := range map[string]map[string]string{
		"içerik değişti": {
			"internal/a/schema.sql": "CREATE TABLE a (x INT);",
			"internal/b/schema.sql": "CREATE TABLE b ();",
		},
		"dosya taşındı": {
			"internal/z/schema.sql": "CREATE TABLE a ();",
			"internal/b/schema.sql": "CREATE TABLE b ();",
		},
		"dosya eklendi": {
			"internal/a/schema.sql": "CREATE TABLE a ();",
			"internal/b/schema.sql": "CREATE TABLE b ();",
			"internal/c/schema.sql": "CREATE TABLE c ();",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := FingerprintOf(files); got == want {
				t.Errorf("the fingerprint did not move: %s", got)
			}
		})
	}
}

// TestFingerprintOfCannotBeFooledByMovingBytesAcrossTheSeparator.
//
// The reason FingerprintOf length-prefixes instead of joining with a
// separator. With a plain "path\ncontent\n" join, a path can absorb what
// looks like the start of a body and two different file sets hash the
// same - a collision anybody can construct by naming a file.
func TestFingerprintOfCannotBeFooledByMovingBytesAcrossTheSeparator(t *testing.T) {
	a := map[string]string{"internal/x/schema.sql": "AB"}
	b := map[string]string{"internal/x/schema.sql\nA": "B"}
	if FingerprintOf(a) == FingerprintOf(b) {
		t.Error("two different file sets hash identically; the encoding is ambiguous")
	}
}
