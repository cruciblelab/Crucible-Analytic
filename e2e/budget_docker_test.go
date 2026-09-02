//go:build docker

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheCollectorRefusesAProfileItsContainerCannotHold.
//
// # Why this test needs a container and cannot be faked
//
// internal/profile's floors were measured by running each profile's
// refresh inside a container five times at each size and keeping the
// smallest size where all five lived - 160 MB for Dengeli, 320 MB for
// Tam. cmd/collector's budget check turns those numbers into a refusal,
// and its unit tests cover the arithmetic and the refuse-or-warn split
// with hand-built ceilings.
//
// What they cannot cover is the join: that a real `--memory=256m`
// produces a file at a path internal/memlimit actually reads, that the
// value parses, that the check runs before anything that needs a
// database, and that the process exits non-zero with a sentence naming
// the fix. Every one of those is a fact about the world rather than
// about the code, and this project's rule is that a claim about the
// world is measured against the world.
//
// The failure being prevented is the collector being killed by the
// kernel mid-refresh, hours after starting, on a machine that looked
// fine. The collector stands in front of the customer's site, so that is
// the site going down at the refresh interval.
//
// # No database anywhere in this test
//
// The check runs before storage.NewWriter, deliberately - it needs
// nothing, so it answers first. That is what lets this test be a plain
// `docker run` rather than a stack: the container that fits gets past
// the check and then fails on the DSN, which is the expected outcome and
// what the assertions read.
func TestTheCollectorRefusesAProfileItsContainerCannotHold(t *testing.T) {
	dockerAvailable(t)
	root := repoRootForBudget(t)
	image := buildImage(t, root)

	cases := []struct {
		name string
		// countryOnly false means both datasets: the Tam profile, floor
		// 320 MB.
		countryOnly bool
		memoryMB    int
		wantRefusal bool
		// wantInOutput is checked either way, so the passing cases
		// prove the check ran rather than merely that nothing broke.
		wantInOutput string
	}{
		{
			name:        "the largest profile in a container that cannot hold it",
			countryOnly: false,
			memoryMB:    256,
			wantRefusal: true,
			// The suggestion, which is the half that makes the refusal
			// actionable. 256 MB holds Dengeli's 160 floor and the 48 MB
			// allowance; it does not hold Tam's 320.
			wantInOutput: `"dengeli"`,
		},
		{
			name:         "the same profile with room for it",
			countryOnly:  false,
			memoryMB:     512,
			wantRefusal:  false,
			wantInOutput: `"profile":"tam"`,
		},
		{
			name:         "the smaller profile in the container that refused the larger",
			countryOnly:  true,
			memoryMB:     256,
			wantRefusal:  false,
			wantInOutput: `"profile":"dengeli"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeBudgetConfig(t, dir, tc.countryOnly)

			cmd := exec.Command("docker", "run", "--rm",
				fmt.Sprintf("--memory=%dm", tc.memoryMB),
				"-v", dir+":/cfg:ro",
				image, "collector", "-config", "/cfg/collector.toml")
			out, err := cmd.CombinedOutput()
			body := string(out)

			// Both outcomes exit non-zero: the one that fits gets past
			// this check and then cannot reach a database that does not
			// exist. So the exit code is not the signal - the text is,
			// and these two strings are the two different things that
			// happened.
			refused := strings.Contains(body, "refusing to start")
			if refused != tc.wantRefusal {
				t.Errorf("refused = %v, want %v (exit: %v)\n%s",
					refused, tc.wantRefusal, err, lastLines(body, 20))
			}
			if !strings.Contains(body, tc.wantInOutput) {
				t.Errorf("the output does not contain %q, so this run did not do what "+
					"the case is about\n%s", tc.wantInOutput, lastLines(body, 20))
			}

			if tc.wantRefusal {
				if err == nil {
					t.Error("the collector refused and still exited zero; a supervisor " +
						"would treat that as a clean shutdown and not restart or alert")
				}
				// It must not have got as far as anything that needs
				// the world to be working. A refusal that happened
				// after the database connection would be a refusal
				// nobody ever sees on a machine whose database is down.
				if strings.Contains(body, "failed to connect to TimescaleDB") {
					t.Error("the memory check ran after the database connection; it " +
						"needs nothing and must answer before anything that does")
				}
				return
			}

			// The fitting cases must reach the database attempt, which
			// is how this test knows they got past the check rather
			// than dying earlier for some unrelated reason.
			if !strings.Contains(body, "failed to connect to TimescaleDB") {
				t.Errorf("this profile fits and the run never reached the database "+
					"attempt, so something else stopped it\n%s", lastLines(body, 20))
			}
		})
	}
}

// writeBudgetConfig writes the smallest configuration collector.Load
// accepts, with the one axis this test varies.
//
// The DSN points at a port nothing listens on, on purpose: this test is
// about a check that runs before any connection, and giving it a real
// database would hide a regression that moved the check after one.
func writeBudgetConfig(t *testing.T, dir string, countryOnly bool) {
	t.Helper()
	body := fmt.Sprintf(`site_id = "budget-test"

[network]
backend_addr = "127.0.0.1:8080"

[storage]
timescale_dsn = "postgres://nobody@127.0.0.1:1/none"

[logging]
dir = ""

[asn_lookup]
enabled = true
country_only = %v
`, countryOnly)

	if err := os.WriteFile(filepath.Join(dir, "collector.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// repoRootForBudget finds the repository root from this package.
func repoRootForBudget(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(wd) // e2e -> repository root
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("%s does not look like the repository root: %v", root, err)
	}
	return root
}
