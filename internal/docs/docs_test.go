package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// repoRoot walks up from this package to the directory holding go.mod.
//
// Computed rather than hard-coded as "../.." so that moving this package
// does not silently turn these tests into checks of an empty file list -
// which would pass.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above this package; cannot find the repository root")
		}
		dir = parent
	}
}

// markdownFiles is every document at the repository root.
//
// The root only: internal/*/README-style files do not exist, and walking
// the whole tree would pull in testdata and vendored text whose encoding
// is not this project's business.
func markdownFiles(t *testing.T) map[string]string {
	t.Helper()
	root := repoRoot(t)
	names, err := filepath.Glob(filepath.Join(root, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatal("no .md files found at the repository root; this test would pass by checking nothing")
	}
	out := make(map[string]string, len(names))
	for _, name := range names {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(name)] = string(body)
	}
	return out
}

// mojibake is what Turkish looks like after being decoded as Latin-1 and
// re-encoded as UTF-8, plus the replacement character that means a byte
// could not be decoded at all.
//
// Matched as pairs rather than by listing "Ã" alone: a lone Ã is legal
// in a quoted French name, while "Ã§" is what "ç" becomes and has no
// business in any of these documents.
var mojibake = regexp.MustCompile("�" + `|Ã[§¶¼\x87\x96\x9c]|Å[\x9f\x9e]|Ä[\x9f\x9e±°]`)

// TestDocuments_TurkishIsNotCorrupted.
//
// The user's standing requirement, and it has to be checked mechanically
// because the failure is invisible to whoever caused it: an editor or a
// pipe that mangles an encoding produces text that looks fine in the
// diff of the line you changed and wrong three paragraphs away.
func TestDocuments_TurkishIsNotCorrupted(t *testing.T) {
	for name, body := range markdownFiles(t) {
		if !utf8.ValidString(body) {
			t.Errorf("%s is not valid UTF-8", name)
			continue
		}
		for _, line := range strings.Split(body, "\n") {
			if found := mojibake.FindString(line); found != "" {
				t.Errorf("%s: corrupted character %q in: %s", name, found, strings.TrimSpace(line))
			}
		}
	}
}

// TestDocuments_TurkishFilesActuallyContainTurkish.
//
// The check above passes on a file that has lost its Turkish entirely -
// every "ş" replaced with "s" is not mojibake, it is just wrong. This
// asserts the characters are present, so a mangling that strips rather
// than corrupts cannot pass silently.
func TestDocuments_TurkishFilesActuallyContainTurkish(t *testing.T) {
	files := markdownFiles(t)
	// VERSIONING.md and CHANGELOG.md joined the list when they were
	// written. They are Turkish, they are read by operators, and a
	// release note whose "ş" has become "Å" is a release note somebody
	// stops trusting at exactly the moment they are deciding whether to
	// upgrade.
	for _, name := range []string{"KURULUM.md", "PLAN.md", "VERSIONING.md", "CHANGELOG.md"} {
		body, ok := files[name]
		if !ok {
			t.Errorf("%s is missing", name)
			continue
		}
		for _, want := range []string{"ş", "ğ", "ı", "İ", "ç", "ö", "ü"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s contains no %q; Turkish text may have been flattened to ASCII", name, want)
			}
		}
	}
}

// docReference matches a backticked filename like `PLAN.md`.
var docReference = regexp.MustCompile("`([A-Z][A-Z0-9-]*\\.md)`")

// TestDocuments_EveryReferenceResolves.
//
// KURULUM.md pointed readers at VERI-ENVANTERI.md for a year's worth of
// commits. The file had never existed - not deleted, never written - and
// nothing said so, because nobody follows every link in a document they
// are editing one section of.
//
// Names that appear inside a code fence are skipped: those are shell
// commands and file listings, where a name is an example rather than a
// promise.
func TestDocuments_EveryReferenceResolves(t *testing.T) {
	root := repoRoot(t)
	for name, body := range markdownFiles(t) {
		for i, line := range outsideCodeFences(body) {
			for _, m := range docReference.FindAllStringSubmatch(line, -1) {
				target := m[1]
				if _, err := os.Stat(filepath.Join(root, target)); err != nil {
					t.Errorf("%s:%d refers to %s, which does not exist", name, i+1, target)
				}
			}
		}
	}
}

// outsideCodeFences returns the document's lines with fenced blocks
// blanked out, keeping the numbering so a failure can be located.
func outsideCodeFences(body string) []string {
	lines := strings.Split(body, "\n")
	inFence := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			lines[i] = ""
			continue
		}
		if inFence {
			lines[i] = ""
		}
	}
	return lines
}
