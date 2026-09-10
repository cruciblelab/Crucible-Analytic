package docs

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
)

// The documents name things that live in the code, and the code moves.
//
// # Why this exists, and what it is for
//
// docs/VERI-ENVANTERI.md is the file a lawyer reads. Every claim in it
// ends with a pointer: this section is drawn from that source file, that
// behaviour is this setting. Those pointers are the whole reason the
// document can be trusted rather than believed - and they are also the
// part that rots silently. A renamed file leaves the sentence intact and
// the pointer dangling; a removed setting leaves a paragraph describing
// something a deployment cannot do.
//
// Measured, and not hypothetical: on 2026-09-08 two sections of that
// document were months out of date. One said the system had no retention
// policy at all - it has had one for weeks, applied by both writers and
// verified against a real TimescaleDB. The other said the visitor-facing
// disclosure was not written; three phases of it were. Both errors
// described the product as worse than it is, which is the direction
// nobody notices, and neither was caught by anything.
//
// This check cannot read a paragraph. What it can do is hold the
// pointers: every path these documents point at exists, and every
// setting they name is one the panel actually defines. That is the half
// of document rot a machine can see.
//
// # The lists are derived, never written
//
// Both checks read the documents themselves, and the setting families
// come from the panel's registry. A list of paths or prefixes beside the
// test would be a second copy of the thing being checked, and the second
// copy is the one nobody updates - the exact failure above, arrived at
// from a different direction.

// docClass says whether a document can have its pointers checked.
type docClass struct {
	checkable bool
	why       string
}

// documentClass separates the documents that describe the system as it
// is from the ones that record what happened to it.
//
// Only the first kind can be held to its pointers. PLAN.md and NOTES.md
// name `net/http`, `pgx/v5`, `internal/config` and a dozen files that
// have since moved - every one of them correct as history and
// unresolvable as a path today. A record naming a file that no longer
// exists is not wrong; it is a record.
//
// Every tracked document has to appear here, and one that appears in
// neither state fails the tests below rather than being skipped. That is
// what keeps a map like this from becoming the place where an
// inconvenient document quietly goes.
var documentClass = map[string]docClass{
	"docs/VERI-ENVANTERI.md": {true, "the data inventory: the document a lawyer reads, and the reason this test exists"},
	"README.md":              {true, "what somebody installs from"},
	"KURULUM.md":             {true, "the same, for the operator"},
	"SOZLUK.md":              {true, "the glossary: every entry points at the code that implements the term"},
	"SECURITY.md":            {true, "describes the current reporting process and the current guarantees"},
	"CONTRIBUTING.md":        {true, "describes how the repository works today"},
	"VERSIONING.md":          {true, "describes the scheme in force"},
	"KARSILASTIRMA.md": {true, "compares this product against Umami as both are today; " +
		"its measurements name the file that produced them and must keep resolving"},

	"PLAN.md":  {false, "a record: phases as they were planned and carried out, naming the files of the day"},
	"NOTES.md": {false, "a record: why each decision was made, at the time it was made"},
	"CHANGELOG.md": {false, "a record: what each release changed, written when it changed and " +
		"not rewritten afterwards"},
	"THIRD-PARTY.md":     {false, "names modules in other repositories, which are not paths here"},
	"CLA.md":             {false, "legal text, and it points at nothing in the code"},
	"CLA-SIGNATURES.md":  {false, "signatures"},
	"CONDUCT.md":         {false, "conduct text, and it points at nothing in the code"},
	"CODE_OF_CONDUCT.md": {false, "conduct text, and it points at nothing in the code"},

	"internal/ja4/testdata/README.md":    {true, "says where each fixture came from and which test reads it"},
	"internal/panel/ui/static/VENDOR.md": {true, "says which vendored file is which, and where it came from"},
}

// pathReference matches a backticked token with a slash in it that does
// not begin with one.
//
// The leading-slash exclusion keeps endpoint paths out: `/_ca/privacy`
// is a route, not a file, and statting it would fail on a document that
// is perfectly correct.
//
// A match here is a candidate, not yet a repository path - see
// looksLikeARepositoryPath.
var pathReference = regexp.MustCompile("`(\\.?[a-z][A-Za-z0-9_.-]*(?:/[A-Za-z0-9_.-]+)+)`")

// goSymbol matches a qualified Go identifier at the end of a candidate:
// `internal/storage.Flusher`, `internal/schemaver.Version`.
//
// These begin with a real directory and are not paths at all. The tell is
// the capital: package names in this project are lower case and file
// extensions are too, so a dot followed by an upper-case letter is a
// symbol every time.
var goSymbol = regexp.MustCompile(`\.[A-Z][A-Za-z0-9_]*$`)

// settingReference matches a backticked dotted lowercase token, which is
// the shape every setting key in this project has.
//
// Narrowed below by prefix rather than here, because this pattern also
// matches `beacon.js` and `upgrader.toml`. Widening the regex to exclude
// those would mean spelling out extensions; asking the registry which
// first segments are settings asks the authority instead.
var settingReference = regexp.MustCompile("`([a-z][a-z0-9_]*(?:\\.[a-z0-9_]+)+)`")

// fileEndings are the extensions a token can carry and still be a file
// rather than a setting, in a family the registry also owns.
//
// `beacon.js` is the one that exists today. Named here rather than in
// the pattern because it is a short, closed list with a reason, and
// because a pattern that knew about extensions would stop being a
// pattern for setting keys.
var fileEndings = []string{".go", ".js", ".sql", ".toml", ".sh", ".html", ".json", ".md"}

func TestEveryPathTheDocumentsPointAtExists(t *testing.T) {
	root := repoRoot(t)
	top := topLevelNames(t, root)
	seen, skipped := 0, 0
	for name, body := range checkableDocuments(t) {
		for i, line := range outsideCodeFences(body) {
			for _, m := range pathReference.FindAllStringSubmatch(line, -1) {
				target := m[1]
				if !looksLikeARepositoryPath(target, top) {
					skipped++
					continue
				}
				seen++
				if _, err := os.Stat(filepath.Join(root, target)); err != nil {
					t.Errorf("%s:%d points at %s, which is not in the repository.\n"+
						"A document whose pointers do not resolve is a document nobody "+
						"can check against the code", name, i+1, target)
				}
			}
		}
	}
	if seen == 0 {
		t.Fatal("no repository paths found in any of these documents; this test would " +
			"pass by checking nothing, which is how it would look the day the pattern " +
			"stopped matching")
	}
	// Both numbers, because the second one is this check's own limit: a
	// candidate whose first segment is not a directory here is passed
	// over, so a reference misspelled in its first segment goes
	// unchecked. Saying how many were passed over is cheaper than
	// pretending the number is zero.
	t.Logf("%d repository paths resolve; %d candidates were not repository paths "+
		"(import paths, Go symbols, external files)", seen, skipped)
}

// looksLikeARepositoryPath separates a path in this repository from the
// other things that carry a slash in prose.
//
// Two rules, both derived rather than listed. The first segment has to be
// something git actually has at the top level, which excludes every
// import path (`net/http`, `pgx/v5`, `golang.org/x/text`), every external
// file (`python/ja4.py`) and every git ref (`refs/tags/v0.23.0`) without
// naming any of them. And the whole thing must not end in a qualified Go
// identifier, which is what `internal/storage.Flusher` is.
func looksLikeARepositoryPath(candidate string, top map[string]bool) bool {
	first, _, _ := strings.Cut(candidate, "/")
	return top[first] && !goSymbol.MatchString(candidate)
}

// topLevelNames is what git has at the repository root.
//
// From git rather than from the filesystem: a build directory, a
// scratch file or an unpacked tarball would otherwise make a wrong
// reference resolve on the machine that had one and fail everywhere
// else.
func topLevelNames(t *testing.T, root string) map[string]bool {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "ls-tree", "--name-only", "HEAD").Output()
	if err != nil {
		t.Fatalf("listing the repository's top level: %v", err)
	}
	names := map[string]bool{}
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name != "" {
			names[name] = true
		}
	}
	if len(names) == 0 {
		t.Fatal("git lists nothing at the top level; every path below would then be " +
			"skipped and this test would pass over an empty set")
	}
	return names
}

// TestEverySettingTheDocumentsNameIsARealSetting.
//
// One direction, like internal/settings' own key check and for the same
// reason: a document naming a key the panel does not define is describing
// a switch nobody can throw. The reverse - a setting no document mentions
// - is ordinary, since most of the registry is operational and belongs in
// KURULUM rather than in a data inventory.
func TestEverySettingTheDocumentsNameIsARealSetting(t *testing.T) {
	prefixes := settingPrefixes()
	if len(prefixes) == 0 {
		t.Fatal("the panel registry defines no dotted keys; every check below would " +
			"then skip every candidate and pass")
	}

	checked := 0
	for name, body := range checkableDocuments(t) {
		for i, line := range outsideCodeFences(body) {
			for _, m := range settingReference.FindAllStringSubmatch(line, -1) {
				token := m[1]
				if !prefixes[token[:strings.Index(token, ".")]] {
					// Not a family the registry uses at all:
					// `crucible.disabled`, `upgrader.toml`, `go.mod`.
					continue
				}
				if isFileName(token) {
					continue
				}
				checked++
				if _, ok := panel.DefinitionFor(panel.Key(token)); !ok {
					t.Errorf("%s:%d names the setting %q, and the panel defines no such "+
						"key.\nEither it was renamed and this sentence now describes "+
						"something a customer cannot do, or the document invented it",
						name, i+1, token)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no setting names found in any of these documents. The inventory and " +
			"KURULUM both name settings, so this is the pattern having stopped matching " +
			"rather than a repository that stopped naming them")
	}
	t.Logf("%d setting references are real settings", checked)
}

func isFileName(token string) bool {
	for _, ending := range fileEndings {
		if strings.HasSuffix(token, ending) {
			return true
		}
	}
	return false
}

// checkableDocuments is the tracked Markdown that describes the present.
//
// It also asserts the map above accounts for every tracked document, so
// a new one is classified deliberately rather than skipped by silence.
func checkableDocuments(t *testing.T) map[string]string {
	t.Helper()
	root := repoRoot(t)

	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "*.md", "**/*.md").Output()
	if err != nil {
		t.Fatalf("listing tracked documents: %v", err)
	}
	names := strings.Split(strings.TrimRight(string(bytes.ReplaceAll(out, []byte{0}, []byte{'\n'})), "\n"), "\n")
	if len(names) == 0 || names[0] == "" {
		t.Fatal("git lists no tracked Markdown; every check here would pass by " +
			"checking nothing")
	}

	// git rather than a glob, and tracked rather than present: CLAUDE.md
	// sits at the root behind .git/info/exclude, so a glob would check a
	// file the build never sees - a test that fails on one machine and
	// passes on CI.
	docs := map[string]string{}
	for _, name := range names {
		class, known := documentClass[name]
		if !known {
			t.Errorf("%s is tracked and this test does not know what kind of document "+
				"it is.\nAdd it to documentClass: true if it describes the system as it "+
				"is and its pointers should resolve, false with a reason if it records "+
				"what happened", name)
			continue
		}
		if !class.checkable {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		docs[name] = string(body)
	}
	if len(docs) == 0 {
		t.Fatal("no document is marked checkable; these tests would pass over an empty set")
	}
	return docs
}

// settingPrefixes is the set of first segments the registry uses.
//
// Derived from the registry, so a new family of settings needs nothing
// here. That matters more than it looks: a hand-written prefix list would
// quietly stop checking a whole family the day somebody added one.
func settingPrefixes() map[string]bool {
	out := map[string]bool{}
	for _, def := range panel.AllDefinitions() {
		key := string(def.Key)
		if i := strings.Index(key, "."); i > 0 {
			out[key[:i]] = true
		}
	}
	return out
}
