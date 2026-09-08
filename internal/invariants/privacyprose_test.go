package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// The disclosure has one text, and this is the check that keeps it one.
//
// # What goes wrong without it
//
// The page in internal/beacon/privacy.go says what this deployment does
// with a visitor's address, and it says it by branching on the mode that
// is in force at that moment. That is the whole design: change the
// setting, and the next request reads a different page.
//
// A copy anywhere else does not branch. A paragraph pasted into README,
// into a panel template, into KURULUM's walkthrough, into a support
// reply saved as a file - each of those is a sentence about the masked
// mode that goes on being served after a customer switches to full. And
// the copy is the one people find: it is in the document they were
// already reading.
//
// This repository applies the same rule to lists of tables, of servers,
// of fuzz targets: *listeyi türet, ismi ekleme*. The disclosure is the
// case where a stale copy is not an inconvenience but a false statement
// to a stranger about their own data.
//
// # Why the sentences are derived rather than listed
//
// A list of forbidden phrases typed into this file would be a second
// copy of the prose - by this test's own argument, the wrong shape. So
// the sentences come out of the template itself: parsed from the source,
// stripped of markup and template actions, split, and each one long
// enough to be nobody's coincidence. Rewrite the page and this test
// checks the new wording on the next run without being touched.
//
// # What it does not claim
//
// It finds copies, not paraphrases. Somebody who retypes the meaning in
// their own words defeats it, and no test catches that. What it does
// catch is the realistic failure - copy, paste, and forget - which is
// how every stale copy in this repository's history actually happened.

// privacyPageSource is the one file allowed to contain the prose.
const privacyPageSource = "internal/beacon/privacy.go"

// What counts as a sentence worth searching for.
//
// Two thresholds rather than one, because either alone lets something
// through. Short fragments - "Çerez yok", "Adresiniz", a heading - are
// ordinary Turkish, and a file matching one of those would be a failure
// nobody could act on. But length alone would keep a long CSS rule or a
// URL, so a word count sits beside it.
//
// The length is counted in runes: every one of these sentences is
// Turkish, and a byte count would make the threshold silently shorter
// for exactly the text this test exists for.
const (
	minProseRunes = 30
	minProseWords = 4
)

// markup strips what a reader never sees, leaving the words.
//
// The style block goes first and whole. Stripping tags alone would leave
// its declarations behind as text, and "16px/1.6 system-ui,sans-serif"
// is neither prose nor anybody's copy of it - it was the first draft's
// entire extracted output, which is how this line came to exist.
//
// Tags become a separator rather than a space, because a heading and the
// paragraph under it are two sentences. Joined by a space they become
// one long string that no realistic copy contains, and the copy - which
// would be of the paragraph alone - goes unnoticed. That is not a
// hypothetical: the first version of this test cleared a README with the
// page's own paragraph pasted into it.
var (
	styleBlock  = regexp.MustCompile(`(?s)<style[^>]*>.*?</style>`)
	markup      = regexp.MustCompile(`<[^>]*>|{{[^}]*}}`)
	sentenceEnd = regexp.MustCompile("[.!?\u0001]")
)

// TestThePrivacyProseExistsInExactlyOnePlace.
func TestThePrivacyProseExistsInExactlyOnePlace(t *testing.T) {
	root := repoRoot(t)
	sentences := privacySentences(t, filepath.Join(root, privacyPageSource))

	// The measurement's own floor. If the template moves, is renamed, or
	// stops being a raw string literal, the extraction returns nothing
	// and every file in the tree passes a search for no sentences - a
	// green run that checked nothing at all.
	if len(sentences) < 10 {
		t.Fatalf("only %d sentences were extracted from %s, so this test would pass "+
			"against a tree full of copies.\n"+
			"The page is probably no longer a single raw string literal named "+
			"privacyPage; teach privacySentences the new shape rather than lowering "+
			"this floor", len(sentences), privacyPageSource)
	}

	for _, rel := range trackedFiles(t, root) {
		if rel == privacyPageSource {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			// Unreadable is not a pass. A file this test cannot open is
			// a file it cannot clear, and saying so is cheaper than a
			// silent gap.
			t.Errorf("could not read %s: %v", rel, err)
			continue
		}
		if !utf8.Valid(body) {
			continue // a binary; it holds no Turkish paragraph
		}
		flat := collapse(string(body))
		for _, sentence := range sentences {
			if strings.Contains(flat, sentence) {
				t.Errorf("%s carries a sentence from the privacy page:\n  %q\n"+
					"That copy does not read the live mode. When a customer switches "+
					"ip_storage the page changes and this does not, so it becomes a "+
					"statement to visitors about a system that stopped existing.\n"+
					"Link to <prefix>/privacy.html, or read <prefix>/privacy and render "+
					"the facts", rel, sentence)
			}
		}
	}
}

// trackedFiles is every file this repository ships, from git.
//
// From git rather than from a walk of the disk, and the difference is
// not performance. A walk finds dist/, which holds release tarballs and
// six binaries per build - each of which *does* contain the page, since
// the page is compiled into the beacon. Those are build output, ignored
// by .gitignore, and present on a developer's machine and nowhere else.
// A test that failed on them would be failing on something that is not
// part of the repository, and one that skipped a directory by name would
// need editing every time a new ignored directory appeared.
//
// git not being there is a hard failure rather than a skip: a green run
// that checked nothing is the outcome this whole file exists to prevent.
func trackedFiles(t *testing.T, root string) []string {
	t.Helper()

	out, err := runGit(root, "ls-files", "-z")
	if err != nil {
		t.Fatalf("listing the repository's files with git: %v\n%s", err, out)
	}

	var files []string
	for _, name := range strings.Split(out, "\x00") {
		if name != "" {
			files = append(files, name)
		}
	}
	if len(files) < 100 {
		t.Fatalf("git lists %d files in this repository, which cannot be right; "+
			"the search below would clear a tree it never read", len(files))
	}
	return files
}

// collapse squeezes runs of whitespace to one space.
//
// By hand rather than with a regexp: this runs over every tracked file,
// and the fuzz corpora under testdata/ make that tens of megabytes.
func collapse(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	gap := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case ' ', '\t', '\n', '\v', '\f', '\r':
			gap = true
		default:
			if gap && b.Len() > 0 {
				b.WriteByte(' ')
			}
			gap = false
			b.WriteByte(c)
		}
	}
	return b.String()
}

// privacySentences extracts the page's prose from the source.
func privacySentences(t *testing.T, path string) []string {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	var raw string
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if name.Name != "privacyPage" || i >= len(spec.Values) {
				continue
			}
			// The template literal is the one raw string inside the
			// initialiser, whatever chain of calls wraps it.
			ast.Inspect(spec.Values[i], func(inner ast.Node) bool {
				lit, ok := inner.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING || !strings.HasPrefix(lit.Value, "`") {
					return true
				}
				if unquoted, err := strconv.Unquote(lit.Value); err == nil && len(unquoted) > len(raw) {
					raw = unquoted
				}
				return true
			})
		}
		return true
	})
	if raw == "" {
		return nil
	}

	text := markup.ReplaceAllString(styleBlock.ReplaceAllString(raw, " "), "\u0001")

	var out []string
	for _, part := range sentenceEnd.Split(text, -1) {
		part = collapse(part)
		if utf8.RuneCountInString(part) < minProseRunes || len(strings.Fields(part)) < minProseWords {
			continue
		}
		out = append(out, part)
	}
	return out
}
