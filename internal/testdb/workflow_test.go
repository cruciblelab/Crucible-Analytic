package testdb

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// roleLoop matches the workflow's `for role in a b c; do`.
//
// Anchored on `for role in` rather than searching for role names, so a
// line that stopped listing a role is a line this still finds and can
// report as incomplete. Looking for the names instead would find nothing
// and pass.
var roleLoop = regexp.MustCompile(`for role in ([a-z_ ]+); do`)

// TestTheWorkflowKnowsEveryRole.
//
// The suites connect as five roles with the convention "the password is
// the role name", and CI is what makes that true: after installing the
// database the way a customer does - which generates real passwords - it
// resets each role's password to its own name.
//
// That reset is driven by a hand-written list in a shell loop, and a
// hand list in a file no Go tool reads is a list nothing keeps honest.
// It stopped being complete when the fifth role arrived, and the symptom
// was not "schema_admin is missing" but
//
//	failed SASL auth: FATAL: password authentication failed for user "schema_admin"
//
// which reads like a database problem rather than like a line in a
// workflow. Every applier and upgrade test failed on it, on every push,
// for weeks.
//
// So the two are compared. The workflow is the half that can fall
// behind; AllRoles is the half the code actually uses.
func TestTheWorkflowKnowsEveryRole(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRootForTest(t), ".github", "workflows", "ci.yml"))
	if err != nil {
		// A checkout without the workflow is not a failure of this
		// project's code, and a test that cannot read its subject has
		// not found a defect.
		t.Skipf("cannot read the workflow: %v", err)
	}

	m := roleLoop.FindSubmatch(body)
	if m == nil {
		t.Fatal("the workflow has no `for role in ...; do` line.\n" +
			"Either the password reset moved - in which case this test now checks " +
			"nothing and must follow it - or it was removed, and the suites will " +
			"fail to authenticate")
	}

	inWorkflow := map[string]bool{}
	for _, name := range strings.Fields(string(m[1])) {
		inWorkflow[name] = true
	}

	for _, role := range AllRoles {
		if !inWorkflow[role] {
			t.Errorf("the CI workflow does not set a password for %q.\n"+
				"install.sh generates one, the suites connect with the role name, and "+
				"the failure surfaces as a SASL error that names the database rather "+
				"than this line", role)
		}
	}

	known := map[string]bool{}
	for _, role := range AllRoles {
		known[role] = true
	}
	for name := range inWorkflow {
		if !known[name] {
			t.Errorf("the CI workflow sets a password for %q, which is not a role any "+
				"suite connects as. Either it was renamed here and not there, or the "+
				"line outlived the role", name)
		}
	}
}

// repoRootForTest walks up to the directory holding go.mod.
func repoRootForTest(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above this package")
		}
		dir = parent
	}
}
