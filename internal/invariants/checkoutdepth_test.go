package invariants

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A suite that asks git a question only gets a real answer in a checkout
// that has one.
//
// # The defect
//
// actions/checkout fetches one commit and no tags unless told otherwise.
// Three suites in this tree read git, and both ways of meeting a thin
// checkout have now happened for real:
//
//	loudly    internal/upgradepath builds a database from the previous
//	          release's schema, so it has to find that release. With no
//	          tags it found none and exited 1, taking the whole
//	          integration job with it - on the commit that added the
//	          package, for a reason that had nothing to do with the code
//	          under test.
//	quietly   internal/docs compares the changelog against the tags and
//	          the commit authors against CLA-SIGNATURES.md. Those tests
//	          skip when the checkout cannot answer, which is right for
//	          the machine and wrong for the pipeline: green, and nothing
//	          checked. That skip is the whole reason the unit job carries
//	          fetch-depth: 0, and nothing kept it there.
//
// Both are one missing line in a YAML file, in a job whose name says
// nothing about git.
//
// # The rule
//
// Neither side is written down here. The suites that read git are found
// by scanning the test sources; the jobs that run them are found by
// reading which packages each workflow's `go test` commands cover, with
// build tags accounted for. What the test then requires is that every
// job reaching such a suite checks out with fetch-depth: 0.
//
// The alternative - "every job that runs tests needs full history" -
// would be simpler and less true: the nightly load and fuzz jobs run
// packages that ask git nothing, and a rule that demands something for
// no reason is a rule people edit out.
func TestEveryJobRunningAGitReadingSuiteChecksOutFully(t *testing.T) {
	root := repoRoot(t)

	suites := gitReadingSuites(t, root)
	if len(suites) == 0 {
		t.Fatal("no test file in this tree was found to invoke git.\n" +
			"Either every such suite is gone - in which case this test and the " +
			"fetch-depth lines it protects have nothing left to protect - or the " +
			"scan stopped recognising how git is called, and a scan that matches " +
			"nothing reports nothing")
	}

	jobs := workflowJobs(t, root)
	if len(jobs) == 0 {
		t.Fatal("no jobs found in .github/workflows; the workflows changed shape and " +
			"this check now reads nothing")
	}

	for _, s := range suites {
		covered := 0
		for _, j := range jobs {
			if !j.runs(s) {
				continue
			}
			covered++
			if j.fetchDepth != "0" {
				where := "no actions/checkout step"
				if j.checkout {
					where = fmt.Sprintf("fetch-depth: %q", j.fetchDepth)
				}
				t.Errorf("%s reads git (%s) and %s runs it with %s.\n"+
					"At the default depth that job has one commit and no tags, so this "+
					"suite either fails on a missing baseline or skips itself and "+
					"reports green. Add to that job's checkout step:\n"+
					"        with:\n          fetch-depth: 0",
					s.pkg, s.evidence, j.String(), where)
			}
		}
		if covered == 0 {
			t.Errorf("%s reads git and no workflow job runs it%s.\n"+
				"A suite that runs nowhere is not protecting anything; either a "+
				"workflow lost the command that covered it or its build tag has no "+
				"job.", s.pkg, s.tagNote())
		}
	}
}

// gitSuite is one package whose tests invoke git, with the build tags
// that would select them (empty when the file carries no constraint).
type gitSuite struct {
	pkg      string // repository-relative, forward slashes
	tags     []string
	evidence string // the call that gave it away, for the failure message
}

func (s gitSuite) tagNote() string {
	if len(s.tags) == 0 {
		return ""
	}
	return " (it is behind //go:build " + strings.Join(s.tags, " || ") +
		", so it needs -tags with one of those)"
}

// key identifies a suite for de-duplication and ordering.
func (s gitSuite) key() string { return s.pkg + "\x00" + strings.Join(s.tags, ",") }

// gitCall matches an exec call that runs or looks for git.
//
// Written against the call rather than against the word: a test whose
// failure message mentions git is not a test that runs git, and
// counting it would make the rule demand full history for a job that
// needs none.
var gitCall = regexp.MustCompile(`(?s)exec\.(Command|CommandContext|LookPath)\([^)]{0,200}?"git"`)

// suiteTags reads a file's build constraint the way go/build selects on
// it: the alternatives of a single `//go:build a || b` line, or nothing
// at all. buildTag, shared with the suite-coverage check, is the half
// that matches the line.
func suiteTags(body []byte) []string {
	for _, line := range strings.Split(string(body), "\n") {
		m := buildTag.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		var tags []string
		for _, alt := range strings.Split(m[1], "||") {
			if alt = strings.TrimSpace(alt); alt != "" {
				tags = append(tags, alt)
			}
		}
		return tags
	}
	return nil
}

// gitReadingSuites walks the tree for test files that invoke git.
func gitReadingSuites(t *testing.T, root string) []gitSuite {
	t.Helper()

	var out []gitSuite
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "scratchpad":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		m := gitCall.FindSubmatch(body)
		if m == nil {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		s := gitSuite{
			pkg:      filepath.ToSlash(rel),
			tags:     suiteTags(body),
			evidence: "exec." + string(m[1]) + ` with "git"`,
		}
		// One entry per package and constraint: two files in a package
		// behind different tags are two different questions about the
		// workflows.
		if seen[s.key()] {
			return nil
		}
		seen[s.key()] = true
		out = append(out, s)
		return nil
	})
	if err != nil {
		t.Fatalf("scanning for suites that invoke git: %v", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key() < out[j].key() })
	return out
}

// job is one workflow job: how deep it checks out, and which packages
// its test commands reach.
type job struct {
	workflow   string
	id         string
	name       string
	checkout   bool
	fetchDepth string
	commands   []command
}

func (j job) String() string {
	label := j.id
	if j.name != "" {
		label = fmt.Sprintf("%s (%q)", j.id, j.name)
	}
	return j.workflow + " job " + label
}

// runs reports whether this job's commands compile and run that suite.
//
// The tag half matters: an untagged file is built by every command, but
// a file behind //go:build integration is built only by a command
// passing that tag - so the same package can be covered by one job for
// one of its suites and by another for the other.
func (j job) runs(s gitSuite) bool {
	for _, c := range j.commands {
		if len(s.tags) > 0 && !anyOf(c.tags, s.tags) {
			continue
		}
		for _, p := range c.packages {
			if p == "./..." || p == "./"+s.pkg {
				return true
			}
		}
	}
	return false
}

// anyOf reports whether the command passes at least one of the tags that
// would select the suite.
func anyOf(passed, wanted []string) bool {
	for _, w := range wanted {
		if contains(passed, w) {
			return true
		}
	}
	return false
}

// goTestCommand matches a `go test` invocation inside a step's script.
//
// release/fuzz.sh lines are deliberately not matched. That script runs
// `go test -fuzz` with -run pinned to the one target, so it does not
// run the rest of the package's tests and cannot reach a suite that
// reads git.
var goTestCommand = regexp.MustCompile(`\bgo test\b([^\n]*)`)

var tagsFlag = regexp.MustCompile(`-tags[= ]([a-z,]+)`)

func workflowJobs(t *testing.T, root string) []job {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no workflow files found; this check reads .github/workflows/*.yml")
	}

	var out []job
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("reading %s: %v", file, err)
		}
		var wf struct {
			Jobs map[string]struct {
				Name  string `yaml:"name"`
				Steps []struct {
					Uses string         `yaml:"uses"`
					With map[string]any `yaml:"with"`
					Run  string         `yaml:"run"`
				} `yaml:"steps"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(body, &wf); err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		if len(wf.Jobs) == 0 {
			t.Fatalf("%s has no jobs; either it stopped being a workflow or this "+
				"check stopped reading one", filepath.Base(file))
		}

		for id, spec := range wf.Jobs {
			j := job{workflow: filepath.Base(file), id: id, name: spec.Name}
			for _, step := range spec.Steps {
				if strings.HasPrefix(step.Uses, "actions/checkout@") {
					j.checkout = true
					if v, ok := step.With["fetch-depth"]; ok {
						j.fetchDepth = fmt.Sprint(v)
					}
				}
				for _, m := range goTestCommand.FindAllStringSubmatch(step.Run, -1) {
					c := command{}
					if tags := tagsFlag.FindStringSubmatch(m[1]); tags != nil {
						c.tags = strings.Split(tags[1], ",")
					}
					for _, field := range strings.Fields(m[1]) {
						if strings.HasPrefix(field, "./") {
							c.packages = append(c.packages, strings.TrimSuffix(field, "/"))
						}
					}
					if len(c.packages) > 0 {
						j.commands = append(j.commands, c)
					}
				}
			}
			out = append(out, j)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].workflow != out[j].workflow {
			return out[i].workflow < out[j].workflow
		}
		return out[i].id < out[j].id
	})
	return out
}
