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

// wrapped matches one invocation in the workflow: the wrapper, then the
// target in quotes.
var wrapped = regexp.MustCompile(`release/fuzz\.sh '(Fuzz[A-Za-z0-9_]*)'`)

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

	// Every invocation in the workflow, read once. The wrapper anchors
	// the target itself (`-fuzz "${target}$"`), which is why the name is
	// written bare here and why this test reads the wrapper's argument
	// rather than go test's flag: an unanchored FuzzParseClientHello
	// also matches FuzzParseClientHelloFromRecords, and `go test`
	// refuses to fuzz two targets at once rather than choosing.
	run := map[string]bool{}
	for _, m := range wrapped.FindAllStringSubmatch(workflow, -1) {
		run[m[1]] = true
	}
	if len(run) < 2 {
		t.Fatalf("only %d fuzz invocations found in the workflow; the pattern has "+
			"stopped matching how they are written", len(run))
	}

	for _, name := range targets {
		if !run[name] {
			t.Errorf("%s is a fuzz target and the nightly workflow does not run it.\n"+
				"Add a step:\n"+
				"  release/fuzz.sh '%s' ./<its package>/ 5m\n"+
				"Without it the target runs its seed corpus in the gate - a few inputs, "+
				"a fraction of a second - and never fuzzes at all", name, name)
		}
	}

	// The other direction: a target named in the workflow that no longer
	// exists fails the whole nightly job, and a nightly that is red for
	// a stale name is one people stop reading.
	have := map[string]bool{}
	for _, name := range targets {
		have[name] = true
	}
	for name := range run {
		if !have[name] {
			t.Errorf("the nightly runs %s and no such fuzz target exists any more; "+
				"that job will fail every night until the line goes", name)
		}
	}
}

// TestEveryFuzzRunGoesThroughTheWrapper.
//
// # Why the wrapper is not optional
//
// `go test -fuzz` reports its own deadline as a failure sometimes: the
// coordinator suppresses the cancellation only when the error is
// identical to its workers' context error, and at the moment the
// deadline fires there is a window where it is not. Nightly #18 spent
// sixty-five minutes to print
//
//	--- FAIL: FuzzAFileThisBuildWroteIsAFileThisBuildCanRead (301.01s)
//	    context deadline exceeded
//
// with no failing input and nothing written to any corpus. release/fuzz.sh
// tells that apart from a finding - by the corpus directory, by the
// "Failing input written to" line, and by whether the failure arrived
// when the budget ran out - and passes the first while failing the
// second.
//
// A step that called `go test -fuzz` directly would go red on the same
// race, and a nightly that is red for no defect is one people learn to
// re-run without reading. That is the failure this whole invariants
// package exists against, so the rule is checked rather than remembered.
func TestEveryFuzzRunGoesThroughTheWrapper(t *testing.T) {
	root := repoRootFromInvariants(t)

	body, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "nightly.yml"))
	if err != nil {
		t.Fatal(err)
	}

	for i, line := range strings.Split(string(body), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue // the comments explain the flag; they do not run it
		}
		if !strings.Contains(trimmed, "-fuzz ") && !strings.Contains(trimmed, "-fuzz=") {
			continue
		}
		t.Errorf(".github/workflows/nightly.yml:%d runs the fuzzer directly:\n  %s\n"+
			"Use release/fuzz.sh, which fails on a finding and passes on the "+
			"toolchain's own deadline race. A direct call goes red on a run that "+
			"found nothing", i+1, trimmed)
	}

	// And the wrapper is there, and runnable.
	info, err := os.Stat(filepath.Join(root, "release", "fuzz.sh"))
	if err != nil {
		t.Fatalf("release/fuzz.sh: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("release/fuzz.sh is not executable (%v); the workflow calls it by "+
			"path", info.Mode().Perm())
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
