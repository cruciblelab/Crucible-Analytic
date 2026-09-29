package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The KURULUM.md headings e2e/systemd_test.go runs are in the document,
// each with a shell block under it.
//
// The systemd suite reads the opt-in commands out of the packaged
// KURULUM.md and runs them as written, on a real systemd, so the document
// and the suite cannot say different things. But the suite runs at night,
// behind a tag, on a runner: a heading renamed in a pull request would go
// green there and red the next morning, in a job nobody had touched.
// Asked here, the rename fails on the push that made it.
//
// The headings are read out of the suite's source rather than listed
// again: a second list would be a third place for the same sentence.
var suiteHeading = regexp.MustCompile(`(?m)^\s*\w+Heading\s*=\s*"(### [^"]+)"`)

func TestTheHeadingsTheSystemdSuiteRunsAreInTheGuide(t *testing.T) {
	root := repoRoot(t)
	suite, err := os.ReadFile(filepath.Join(root, "e2e", "systemd_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	guide, err := os.ReadFile(filepath.Join(root, "KURULUM.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(guide)

	matches := suiteHeading.FindAllStringSubmatch(string(suite), -1)
	if len(matches) < 2 {
		t.Fatalf("found %d heading constants in e2e/systemd_test.go, want at least the two "+
			"opt-ins; the suite changed shape and this check now reads nothing", len(matches))
	}
	for _, m := range matches {
		heading := m[1]
		at := strings.Index(text, "\n"+heading+"\n")
		if at < 0 {
			t.Errorf("e2e/systemd_test.go runs the commands under %q and KURULUM.md has no "+
				"such heading; the nightly suite would stop at it", heading)
			continue
		}
		section := text[at+len(heading)+2:]
		if next := strings.Index(section, "\n### "); next >= 0 {
			section = section[:next]
		}
		// Exactly one, not "at least one".
		//
		// The suite runs the first shell block under the heading. This
		// asked only that one existed, and a mutation that turned the
		// opt-in block into plain text survived: the opt-OUT block, then
		// in the same section, became the first - so the nightly would
		// have run the commands that turn the feature off and called the
		// result a measurement of turning it on. The two now have headings
		// of their own, and a second block here is refused whatever order
		// it is in.
		if n := strings.Count(section, "```bash\n"); n != 1 {
			t.Errorf("KURULUM.md's %q has %d ```bash blocks; the systemd suite runs the "+
				"first one as written, so there must be exactly the one that turns the "+
				"feature on", heading, n)
		}
	}
}
