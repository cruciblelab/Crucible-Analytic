package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// The route count README states is the number of routes this API has.
//
// # Why the number is in a test and not only in prose
//
// Because it drifted. README said "every one of the 28 routes is
// asserted to reject a missing token"; the assertions are fine - they
// derive the list from the router, which is the whole point of
// perSiteRoutes - but the *number* beside them was written once and
// never moved, and by the time O2's summary and R2's fingerprint
// endpoints existed there were 34.
//
// A stale count in that sentence is worse than no count, because the
// sentence's job is to say the coverage is exhaustive. A reader who
// counts 34 routes and reads "28" learns that six of them are not
// covered - which is false, and is exactly the doubt the sentence was
// written to remove.
//
// # Why here rather than in internal/docs
//
// perSiteRoutes is this package's helper and unexported, and it is the
// authority: it reads the registrations out of server.go and
// server_beacon.go. internal/docs cannot reach it, and a copy of the
// number there would be a third place to keep in step.
func TestTheRouteCountInTheReadmeIsTheCountThisAPIRegisters(t *testing.T) {
	routes := perSiteRoutes(t)

	readme := readReadmeFromRoot(t)
	claim := regexp.MustCompile(`every one of the ([0-9]+) routes is asserted`)
	m := claim.FindStringSubmatch(readme)
	if m == nil {
		t.Fatalf("README no longer says how many routes are covered.\n"+
			"Without that sentence this test checks nothing. This API registers %d "+
			"per-site routes today.", len(routes))
	}
	stated, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("README's route count %q is not a number: %v", m[1], err)
	}
	if stated != len(routes) {
		t.Errorf("README says %d routes; the API registers %d per-site routes.\n"+
			"The assertions cover all of them - they read the list from the router - "+
			"so the defect is the number, and a number that undercounts tells a "+
			"reader the coverage has holes it does not have.", stated, len(routes))
	}
}

// readReadmeFromRoot walks up to the repository root and reads README.md.
//
// Walks rather than assuming a depth: a hard-coded "../.." reads nothing
// the day the tree moves, and reading nothing is how this kind of test
// passes without checking.
func readReadmeFromRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if body, err := os.ReadFile(filepath.Join(dir, "README.md")); err == nil {
			return string(body)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no README.md above the working directory")
		}
		dir = parent
	}
}
