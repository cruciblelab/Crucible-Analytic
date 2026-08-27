// The glossary names things that exist.
//
// SOZLUK.md explains this project's vocabulary, and a glossary is a
// document with an unusual failure mode: it does not break, it drifts.
// A package gets renamed, a table is dropped, a binary is merged into
// another, and the entry describing it stays there reading perfectly -
// which is worse than no entry, because somebody will act on it.
//
// So the nouns are checked. Not the prose: whether an explanation is
// *good* is not a thing a test can ask, and pretending otherwise would
// be the kind of green that says nothing. What it can ask is whether the
// package, table, binary or SQL file named in a backtick is still there.
//
// No build tag: it reads one Markdown file and a directory listing.
package release

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// backticked pulls every `code span` out of the glossary.
var backticked = regexp.MustCompile("`([^`\n]+)`")

func glossaryTerms(t *testing.T) []string {
	t.Helper()
	body := readFile(t, repoRootFromWD(t), "SOZLUK.md")

	seen := map[string]bool{}
	var out []string
	for _, m := range backticked.FindAllStringSubmatch(body, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

// TestTheGlossaryNamesPackagesThatExist.
func TestTheGlossaryNamesPackagesThatExist(t *testing.T) {
	root := repoRootFromWD(t)
	checked := 0

	for _, term := range glossaryTerms(t) {
		if !strings.HasPrefix(term, "internal/") {
			continue
		}
		// `internal/panel/web/config.go` style references name a file;
		// `internal/ja4` names a directory. Both have to exist, and
		// os.Stat answers for either.
		if _, err := os.Stat(filepath.Join(root, term)); err != nil {
			t.Errorf("SOZLUK.md mentions %s, which is not in the tree: %v", term, err)
			continue
		}
		checked++
	}
	if checked < 5 {
		t.Errorf("only %d internal/ references found in SOZLUK.md; has the glossary stopped naming code?", checked)
	}
}

// TestTheGlossaryNamesTablesThatExist.
//
// Read from the schema files rather than from a live database, so this
// stays in the gate: what has to hold is that the glossary and the
// schemas agree, and both are files.
func TestTheGlossaryNamesTablesThatExist(t *testing.T) {
	root := repoRootFromWD(t)

	schemas, err := filepath.Glob(filepath.Join(root, "internal", "*", "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	createTable := regexp.MustCompile(`(?i)CREATE TABLE (?:IF NOT EXISTS )?([a-z_]+)`)
	for _, path := range schemas {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range createTable.FindAllStringSubmatch(string(body), -1) {
			declared[m[1]] = true
		}
	}
	if len(declared) < 10 {
		t.Fatalf("found %d tables across the schema files, which is fewer than this product has", len(declared))
	}

	// The glossary's own table list, which is the section a reader is
	// most likely to trust literally.
	body := readFile(t, root, "SOZLUK.md")
	checked := 0
	for _, term := range glossaryTerms(t) {
		// A table name is lower-case with underscores and no dots or
		// slashes; anything else in a code span is a config key, a
		// command or a path.
		if !regexp.MustCompile(`^[a-z][a-z0-9_]*$`).MatchString(term) {
			continue
		}
		// Only terms the glossary presents as tables: they appear in
		// its table-listing section, which every row of names in
		// backticks at the start of a line.
		if !strings.Contains(body, "| `"+term+"`") {
			continue
		}
		checked++
		if !declared[term] {
			t.Errorf("SOZLUK.md lists %s as a table, but no schema file creates one by that name", term)
		}
	}
	if checked < 8 {
		t.Errorf("only %d tables recognised in SOZLUK.md's table list; has its shape changed?", checked)
	}
}

// TestTheGlossaryNamesFilesThatExist covers the scripts and SQL it points
// a reader at.
func TestTheGlossaryNamesFilesThatExist(t *testing.T) {
	root := repoRootFromWD(t)

	// Named rather than pattern-matched: these are the paths the
	// glossary tells somebody to go and read, and each one being wrong
	// costs a reader the same five minutes.
	for _, path := range []string{
		"release/build.sh",
		"release/install.sh",
		"release/sql/grants.sql",
		"release/sql/harden.sql",
		"release/sql/verify.sql",
		"docker/compose.yml",
		"Dockerfile",
		"KURULUM.md",
		"NOTES.md",
	} {
		body := readFile(t, root, "SOZLUK.md")
		if !strings.Contains(body, path) {
			continue // the glossary does not mention it, which is allowed
		}
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Errorf("SOZLUK.md points at %s, which does not exist: %v", path, err)
		}
	}
}

// TestTheGlossaryCoversEveryBinary.
//
// The five commands are the first thing anybody meets, so the glossary
// has to name all of them. This is the one completeness check here:
// asking for every package or every config key would turn the glossary
// into a generated file, which is not what it is for.
func TestTheGlossaryCoversEveryBinary(t *testing.T) {
	root := repoRootFromWD(t)
	body := readFile(t, root, "SOZLUK.md")

	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		found++
		if !strings.Contains(body, "**"+e.Name()+"**") {
			t.Errorf("SOZLUK.md has no entry for the %s command", e.Name())
		}
	}
	if found < 5 {
		t.Fatalf("found %d commands under cmd/, which is fewer than this product has", found)
	}
}
