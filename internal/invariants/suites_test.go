package invariants

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A test suite that never runs is worse than a missing one.
//
// Missing is visible: somebody asks "is this covered" and the answer is
// no. A suite behind a build tag that no workflow names looks covered
// from every angle - the files are there, they compile (the gate vets
// every tag), and `go test -tags loadtest ./internal/asnlookup/` passes
// on the machine of whoever wrote them. It just never runs again.
//
// This is not hypothetical. internal/asnlookup carried three
// loadtest-tagged tests, one of them the proof that local_csv_path never
// touches the network, and the nightly's load job named one directory:
// ./internal/loadtest/. They had never run in CI. Adding the network
// suite for M1 to a job whose command was a hand-written path is what
// prompted looking, which is the sixth time this season that a list was
// dangerous by being short rather than by being wrong.
//
// So both sides are read from files: the tags out of the test sources,
// the commands out of the workflows.

// gatedTags are the build tags whose suites do not run in the default
// gate, with what each one depends on.
//
// The reason each is here rather than in the ordinary suite is the
// dependency, and naming it is what stops somebody "simplifying" one
// into the gate: a gate that goes red because a third party's web server
// is down teaches people to ignore red.
var gatedTags = map[string]string{
	"network":  "a third party's web server, up when they decide it is",
	"loadtest": "real concurrency and timing, too slow and too shared-runner-dependent to gate",
	"e2e":      "the whole chain built and running, minutes per run",
	"docker":   "a built image and a container runtime",
	"release":  "a full reproducible build, tens of minutes",
}

// integration is deliberately absent: ci.yml runs it over ./... on every
// push, so it is gated, not scheduled.

// buildTag matches "//go:build network" and "//go:build e2e || docker".
var buildTag = regexp.MustCompile(`^//go:build (.+)$`)

// goTestLine matches a workflow's test command and captures the tag list
// and everything after it, which is where the package paths are.
//
// Digits belong in the class: without them "e2e" captured as "e", the
// e2e job looked like it ran a tag nothing carries, and the e2e suite
// looked unrun. Which is to say this regexp made exactly the mistake the
// test exists to catch, and the test caught it on its first run.
var goTestLine = regexp.MustCompile(`go test [^\n]*-tags[= ]"?([a-z0-9,]+)"?([^\n]*)`)

// TestEveryGatedSuiteIsRunSomewhere.
//
// The direction that catches a suite nobody executes.
func TestEveryGatedSuiteIsRunSomewhere(t *testing.T) {
	root := repoRoot(t)

	suites := taggedSuites(t, root)
	if len(suites) == 0 {
		t.Fatal("no build-tagged test files found; this test would pass by checking " +
			"nothing, which is exactly how it would look if the scan broke")
	}
	commands := workflowTestCommands(t, root)
	if len(commands) == 0 {
		t.Fatal("no tagged `go test` commands found in .github/workflows; either the " +
			"workflows changed shape or this scan did, and both mean every suite " +
			"below would be reported as unrun")
	}

	for _, s := range sortedSuiteKeys(suites) {
		tag, pkg := s.tag, s.pkg
		if _, gated := gatedTags[tag]; !gated {
			t.Errorf("%s carries //go:build %s and that tag is not in gatedTags.\n"+
				"Either it runs in the ordinary gate - in which case the tag is doing "+
				"nothing - or it is a new kind of dependency that needs a line here "+
				"saying what it needs and why it cannot gate a merge", pkg, tag)
			continue
		}
		if !someCommandRuns(commands, tag, pkg) {
			t.Errorf("%s has //go:build %s tests and no workflow runs them.\n"+
				"It compiles (the gate vets every tag) and it passes wherever somebody "+
				"runs it by hand, so nothing anywhere reports the day it stops.\n"+
				"Add the package to the job that runs -tags %s.", pkg, tag, tag)
		}
	}
}

// TestEveryGatedTagStillHasASuite.
//
// The stale half. A tag listed above with no tests left describes a
// dependency this project no longer has, and the workflow job that runs
// it is then a green step that executes nothing - which reads, on the
// summary page, exactly like a job that passed.
func TestEveryGatedTagStillHasASuite(t *testing.T) {
	suites := taggedSuites(t, repoRoot(t))

	present := map[string]bool{}
	for s := range suites {
		present[s.tag] = true
	}
	for tag := range gatedTags {
		if !present[tag] {
			t.Errorf("gatedTags lists %q and no test file carries that tag. The workflow "+
				"job running it passes by testing nothing, which on the summary page "+
				"looks the same as passing", tag)
		}
	}
}

type suite struct{ tag, pkg string }

// taggedSuites reads every _test.go file's build constraint.
//
// Constraints are OR'd terms in this repository ("e2e || docker"), so
// each term is recorded separately: a file that runs under either tag is
// a suite under both, and the one that goes unrun is the one nobody
// thought about.
func taggedSuites(t *testing.T, root string) map[suite]bool {
	t.Helper()
	out := map[suite]bool{}

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == "vendor" || name == "testdata" ||
				(strings.HasPrefix(name, ".") && path != root) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(entry.Name(), "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// The constraint is in the first few lines or it is not a
		// constraint: Go only honours it above the package clause.
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "package ") {
				break
			}
			m := buildTag.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			pkg := packagePath(root, filepath.Dir(path))
			for _, term := range strings.Split(m[1], "||") {
				term = strings.TrimSpace(term)
				if term == "" || term == "integration" {
					continue
				}
				out[suite{tag: term, pkg: pkg}] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scanning for build-tagged tests: %v", err)
	}
	return out
}

// command is one `go test -tags ...` invocation from a workflow.
type command struct {
	tags     []string
	packages []string
}

// workflowTestCommands reads every tagged test command out of the
// workflow files.
func workflowTestCommands(t *testing.T, root string) []command {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}

	var out []command
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			trimmed := strings.TrimSpace(line)
			// A commented-out command is not a command, and a comment
			// mentioning one is how this scan would be fooled into
			// reporting a suite as covered.
			if strings.HasPrefix(trimmed, "#") {
				continue
			}
			m := goTestLine.FindStringSubmatch(trimmed)
			if m == nil {
				continue
			}
			c := command{tags: strings.Split(m[1], ",")}
			for _, field := range strings.Fields(m[2]) {
				if strings.HasPrefix(field, "./") {
					c.packages = append(c.packages, strings.TrimSuffix(field, "/"))
				}
			}
			out = append(out, c)
		}
	}
	return out
}

// someCommandRuns reports whether any command covers this suite.
func someCommandRuns(commands []command, tag, pkg string) bool {
	for _, c := range commands {
		if !contains(c.tags, tag) {
			continue
		}
		for _, p := range c.packages {
			if p == "./..." || p == "."+string(filepath.Separator)+pkg || p == "./"+pkg {
				return true
			}
		}
	}
	return false
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// packagePath turns an absolute directory into the repository-relative
// form a workflow writes, using forward slashes on every platform
// because that is what the YAML contains.
func packagePath(root, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return dir
	}
	return filepath.ToSlash(rel)
}

// sortedSuiteKeys makes the failure output stable; an error list that
// reshuffles between runs is one people stop reading.
func sortedSuiteKeys(m map[suite]bool) []suite {
	out := make([]suite, 0, len(m))
	for s := range m {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].pkg != out[j].pkg {
			return out[i].pkg < out[j].pkg
		}
		return out[i].tag < out[j].tag
	})
	return out
}
