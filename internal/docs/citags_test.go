package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// CONTRIBUTING.md's "What CI runs" block and .github/workflows/ci.yml
// have to agree about which build tags a pull request is tested under.
//
// # What went wrong
//
// The block listed `go test -tags release -count=1 ./release/` under
// the heading "What CI runs, and what a pull request has to pass". CI
// never ran it. It vets that tag - `go vet -tags release ./release/` -
// and vetting is not testing; the tests behind it run once a day in
// nightly.yml, on main, after the fact.
//
// Forty-three tests sat behind that tag, twenty-eight of them reading
// install.sh. A change breaking the installer passed its pull request
// and went red the next night, and the document told the author the
// opposite. Found in a review on 2026-09-21, by asking the workflow
// rather than the sentence.
//
// # Why the rule is about tags and not about lines
//
// The block is prose-with-elisions on purpose: `gosec ... | go run ...`
// stands for a command nobody would paste. Comparing it line by line
// would either fail on the elisions or be written loosely enough to
// pass on anything.
//
// A build tag is the part that cannot be elided and cannot be fudged:
// either the workflow runs `go test` with it or the tests behind it are
// not a pull request's problem. That is exactly the claim the heading
// makes, so that is what is held.
//
// # Why the pattern says `go test` and not `go (test|vet)`
//
// Because vetting a tag is what CI already did for `release`, and
// counting it would have read that as running the tests. Measured
// rather than asserted: loosening the pattern to accept `go vet` is a
// mutation that survives on its own - with the document correct there
// is nothing for it to hide - but applied *together* with the document
// claiming `release` again, the pair survives while the document
// change alone is caught. So the restriction is what turns the
// detector on, and the only way to see that was to mutate both.
func TestTheDocumentedCITagsAreTheTagsCIActuallyTests(t *testing.T) {
	root := repoRoot(t)

	contributing, err := os.ReadFile(filepath.Join(root, "CONTRIBUTING.md"))
	if err != nil {
		t.Fatalf("reading CONTRIBUTING.md: %v", err)
	}
	workflow, err := os.ReadFile(filepath.Join(root,
		".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("reading ci.yml: %v", err)
	}

	block := ciBlock(t, string(contributing))
	claimed := goTestTags(block)
	// Not "at least one": a block that stopped naming any tag would pass
	// an empty comparison, and the failure this test exists for is
	// exactly a claim that quietly stopped being true.
	if len(claimed) == 0 {
		t.Fatal("the \"What CI runs\" block names no `go test -tags` at all - " +
			"either the block changed shape or the pattern below stopped " +
			"matching it, and an empty list compares equal to anything")
	}

	actual := goTestTags(string(workflow))
	for _, tag := range claimed {
		if !contains(actual, tag) {
			t.Errorf("CONTRIBUTING says a pull request has to pass "+
				"`go test -tags %s`, and ci.yml never runs `go test` with that "+
				"tag (it runs: %v).\nEither ci.yml should run it or the block "+
				"should not promise it. Vetting a tag is not testing it, and "+
				"a suite that only the nightly runs is not something a pull "+
				"request has to pass.", tag, actual)
		}
	}
}

// ciBlock returns the fenced code block that follows the "What CI runs"
// heading.
//
// Anchored on the heading rather than on the first fence in the file:
// CONTRIBUTING has several, and the one above this heading is the
// gate's own invocation - which deliberately differs from CI, so
// comparing that one would assert the opposite of what is meant.
func ciBlock(t *testing.T, body string) string {
	t.Helper()

	const heading = "What CI runs, and what a pull request has to pass:"
	i := strings.Index(body, heading)
	if i < 0 {
		t.Fatalf("CONTRIBUTING.md no longer has the %q heading; this test "+
			"reads the block under it and cannot find the block", heading)
	}
	rest := body[i+len(heading):]
	open := strings.Index(rest, "```")
	if open < 0 {
		t.Fatal("no code block follows the \"What CI runs\" heading")
	}
	rest = rest[open+3:]
	close := strings.Index(rest, "```")
	if close < 0 {
		t.Fatal("the code block under \"What CI runs\" is not closed")
	}
	return rest[:close]
}

// goTestTags is every tag passed to `go test -tags` in s, deduplicated
// and in the order it appears.
//
// Only `go test`: `go vet -tags X` matches neither, which is the whole
// distinction this file is about.
var goTestTagPattern = regexp.MustCompile(`go test[^\n|&;]*?-tags[ =]"?([a-z,]+)"?`)

func goTestTags(s string) []string {
	var out []string
	for _, m := range goTestTagPattern.FindAllStringSubmatch(s, -1) {
		for _, tag := range strings.Split(m[1], ",") {
			if tag != "" && !contains(out, tag) {
				out = append(out, tag)
			}
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
