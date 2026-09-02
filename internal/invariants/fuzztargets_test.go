package invariants

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A fuzz target that nothing runs is a function, not a check.
//
// `go test ./...` executes a Fuzz function's seed corpus and stops
// there - a handful of inputs, in well under a second. The millions of
// executions that make fuzzing worth anything happen only under
// `-fuzz`, and `-fuzz` takes one target per invocation, so the nightly
// workflow names them one line at a time.
//
// That is a hand-written list beside a set that grows, which is the
// shape this package exists for. It has already gone stale once in this
// repository in a different form: the Dockerfile's schema COPY list
// stopped at six while the schema reached ten, and every container
// install was missing four tables until somebody hit the error.
//
// The failure here would be quieter than that. Nothing breaks, nothing
// goes red; a target simply never runs, and the coverage it was written
// to buy is bought once, on the day it was added, by whoever ran it by
// hand.
var fuzzFunc = regexp.MustCompile(`(?m)^func (Fuzz[A-Za-z0-9_]*)\(`)

// TestEveryFuzzTargetIsRunByTheNightly.
func TestEveryFuzzTargetIsRunByTheNightly(t *testing.T) {
	root := repoRootFromInvariants(t)

	nightly, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "nightly.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(nightly)

	var targets []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// dist/ holds built artifacts and .git holds packed
			// objects; neither is source, and both are large enough
			// that walking them is the slow part.
			if name := d.Name(); name == ".git" || name == "dist" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range fuzzFunc.FindAllStringSubmatch(string(body), -1) {
			targets = append(targets, m[1])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(targets) < 2 {
		t.Fatalf("found %d fuzz targets in the tree; this repository has more than "+
			"that, so the pattern has stopped matching how they are declared and this "+
			"test is comparing nothing", len(targets))
	}

	for _, name := range targets {
		// Anchored exactly as the workflow must write it. The anchor is
		// not decoration: an unanchored FuzzParseClientHello also
		// matches FuzzParseClientHelloFromRecords, and `go test`
		// refuses to fuzz two targets at once rather than choosing.
		if !strings.Contains(workflow, "'"+name+"$'") {
			t.Errorf("%s is a fuzz target and the nightly workflow does not run it.\n"+
				"Add a step:\n"+
				"  go test -run XXX -fuzz '%s$' -fuzztime 5m ./<its package>/\n"+
				"Without it the target runs its seed corpus in the gate - a few inputs, "+
				"a fraction of a second - and never fuzzes at all", name, name)
		}
	}

	// The other direction: a target named in the workflow that no longer
	// exists fails the whole nightly job, and a nightly that is red for
	// a stale name is one people stop reading.
	named := regexp.MustCompile(`-fuzz '(Fuzz[A-Za-z0-9_]*)\$'`)
	have := map[string]bool{}
	for _, name := range targets {
		have[name] = true
	}
	for _, m := range named.FindAllStringSubmatch(workflow, -1) {
		if !have[m[1]] {
			t.Errorf("the nightly runs %s and no such fuzz target exists any more; "+
				"that job will fail every night until the line goes", m[1])
		}
	}
}

// repoRootFromInvariants is repoRoot under a name that says where it
// starts from, kept separate because this test walks the whole tree and
// a wrong root would silently find nothing.
func repoRootFromInvariants(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("%s does not look like the repository root: %v", root, err)
	}
	return root
}
