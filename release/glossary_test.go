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

// internalPackage matches the way SOZLUK.md writes a package: the import
// path's tail, in backticks or not. Only lowercase, which is every
// package in this repository and keeps prose like "internal/Foo" from
// being read as a reference.
var internalPackage = regexp.MustCompile(`internal/[a-z0-9]+`)

// TestTheGlossaryDefinesEveryPackageItMentions.
//
// # Why the glossary needed a test of its own
//
// The tests above ask whether everything the glossary names still
// exists. That is one direction, and it is the one that catches a rename
// or a dropped table. It cannot catch the other: a package the glossary
// refers to and never explains passes every one of them, because the
// package is right there in the tree.
//
// So the file fell a whole phase group behind without anything saying
// so - and the checks that were meant to keep it honest all stayed
// green, which is why the gap survived long enough to be found by
// reading. L1,
// L2 and L3 built the upgrade machinery - the schema version, the
// fingerprint, the queue, the applier, lock_timeout - and none of it
// reached the glossary. The M phases that came after did get entries,
// which is what made the gap invisible: the document looked current
// because its newest parts were.
//
// The tell was already in the file. An entry on the refresh queue said
// "the same pattern as internal/upgrade, one table over" - referring the
// reader to a package the glossary never defined, as if it had.
//
// # What this checks, and why it is derived rather than listed
//
// Not "every package must be in the glossary": most are plumbing and an
// entry for each would be noise, and the hand-written list of exceptions
// would be the usual trap - wrong is survivable, short is not.
//
// The rule is about self-consistency instead, and it needs no list at
// all: if SOZLUK.md names a package, some entry in SOZLUK.md has to
// define it. That makes a passing mention of an undefined package the
// failure - which is exactly the state the file was in.
//
// # What counts as a definition, and why the first answer was too loose
//
// A definition is the *head* of an entry: the text from the bold term up
// to the em dash that introduces the prose, which is where this file has
// always put an entry's package -
//
//	**uygulayıcı (applier)** (`internal/applier`) — Bu depoda DDL...
//
// The first version of this test accepted a package named anywhere in an
// entry's paragraph. That was wrong in a way its own passing run hid: an
// entry that happens to mention five packages would "define" all five,
// so the glossary could refer to something undefined and stay green as
// long as the reference sat inside somebody else's entry. It was caught
// by a mutation - a dangling mention of internal/storage was dropped into
// the upgrade-queue entry, and the test did not notice. The gap this test
// was written for had been caught only by the luck of sitting in a
// paragraph that did not open in bold.
//
// The tightened rule then found two more real ones: internal/dblock and
// internal/testdb were both referred to from inside other entries and
// neither had one of its own.
func TestTheGlossaryDefinesEveryPackageItMentions(t *testing.T) {
	body := readFile(t, repoRootFromWD(t), "SOZLUK.md")

	mentioned := map[string]bool{}
	for _, m := range internalPackage.FindAllString(body, -1) {
		mentioned[m] = true
	}
	if len(mentioned) == 0 {
		t.Fatal("SOZLUK.md names no packages at all, so this test is comparing " +
			"nothing against nothing - the regexp above has probably stopped " +
			"matching how the file writes them")
	}

	// A paragraph is a run of lines between blank ones, an entry is a
	// paragraph that opens in bold, and its head is everything before the
	// em dash. The head rather than the whole paragraph, for the reason
	// written out above; the term may wrap across lines, so this works on
	// the paragraph rather than the line.
	defined := map[string]bool{}
	for _, para := range strings.Split(body, "\n\n") {
		para = strings.TrimSpace(para)
		if !strings.HasPrefix(para, "**") {
			continue
		}
		head, _, found := strings.Cut(para, "—")
		if !found {
			// No em dash means no prose to separate from, so the whole
			// paragraph is the term. Rare, and treating it as a head keeps
			// a bold line that is only a heading from defining nothing.
			head = para
		}
		for _, m := range internalPackage.FindAllString(head, -1) {
			defined[m] = true
		}
	}

	var undefined []string
	for pkg := range mentioned {
		if !defined[pkg] {
			undefined = append(undefined, pkg)
		}
	}
	sort.Strings(undefined)

	for _, pkg := range undefined {
		t.Errorf("SOZLUK.md mentions %s but no entry defines it.\n"+
			"A reader sent to a package by the glossary has nowhere to look it "+
			"up, and the glossary reads as though it covers ground it does not. "+
			"Either give %s an entry - a paragraph opening with its bold term - "+
			"or stop naming it", pkg, pkg)
	}
}
