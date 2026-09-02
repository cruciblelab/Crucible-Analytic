//go:build release

package release

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestTheGateScriptRunsWhatCIRuns.
//
// # What went wrong
//
// The gate lived in CONTRIBUTING.md as eight commands, and eight
// commands is a list somebody runs the familiar two of. That is not
// hypothetical: internal/memlimit was pushed after `go test ./...` came
// back clean, and CI went red on gosec - a step that is not part of
// `go test`, needs a tool installed, and so never runs by accident.
//
// release/gate.sh is the fix, and it introduces the failure this test
// exists for. A script that runs seven of the eight steps is worse than
// no script: the eight-command list at least looked long enough to
// read, while a script that exits green is a claim.
//
// # What is checked, and what is deliberately not
//
// The two analysers, and their pinned versions. Those are the steps that
// need an install, that a person will not run by hand, and whose pins
// the workflow's own comments call load-bearing - an unpinned analyser
// moves the baseline under a gate without anybody choosing to.
//
// Not every `run:` line in the workflow, which would fail on things the
// script has no business doing: installing Go, checking out the
// repository, starting a database service. The rule that holds is
// narrower and it is the one that broke: whatever tool CI installs to
// judge this repository, the script installs too, at the same version.
func TestTheGateScriptRunsWhatCIRuns(t *testing.T) {
	root := repoRootFromWD(t)

	workflow := readFile(t, root, filepath.Join(".github", "workflows", "ci.yml"))
	script := readFile(t, root, filepath.Join("release", "gate.sh"))

	// Every `go install <module>@<version>` the gate job runs.
	installs := regexp.MustCompile(`go install ([^\s@]+)@(v[0-9][^\s"']*)`)
	found := installs.FindAllStringSubmatch(workflow, -1)
	if len(found) == 0 {
		t.Fatal("ci.yml installs no analyser any more, so this test is comparing " +
			"nothing - either the gate changed shape or the pattern above has " +
			"stopped matching how it is written")
	}

	seen := map[string]bool{}
	for _, m := range found {
		module, version := m[1], m[2]
		if seen[module] {
			continue
		}
		seen[module] = true

		if !strings.Contains(script, module) {
			t.Errorf("ci.yml installs %s and release/gate.sh does not mention it.\n"+
				"Somebody running the script would be told the gate is green while a "+
				"whole step never ran - which is exactly the failure the script was "+
				"written to remove", module)
			continue
		}
		if !strings.Contains(script, version) {
			t.Errorf("ci.yml pins %s at %s and release/gate.sh does not carry that "+
				"version.\nThe pin is load-bearing: an analyser that moves between the "+
				"two would make the script disagree with the gate about what counts "+
				"as a finding", module, version)
		}
	}

	// And the two diff tools that turn a raw report into a verdict. A
	// script that ran the analysers and skipped these would print
	// findings and exit zero.
	for _, tool := range []string{"sastdiff", "deadcodediff"} {
		if !strings.Contains(script, tool) {
			t.Errorf("release/gate.sh does not run %s, so it produces a report and "+
				"never judges it", tool)
		}
	}
}

// TestTheGateScriptIsExecutable.
//
// A script somebody has to remember to prefix with `bash` is one they
// run differently from how the documentation says to, which is how a
// flag gets dropped.
func TestTheGateScriptIsExecutable(t *testing.T) {
	path := filepath.Join(repoRootFromWD(t), "release", "gate.sh")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("%s is mode %v and cannot be executed", path, info.Mode().Perm())
	}
}
