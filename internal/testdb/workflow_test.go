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

// installCreates matches install.sh's role-creation loop.
var installCreates = regexp.MustCompile(`(?m)^  for role in ([a-z_ ]+); do$`)

// installCredential matches one row of install.sh's ROLE_CREDENTIAL
// table: role, file, DSN key.
var installCredential = regexp.MustCompile(`(?m)^  "([a-z_]+) +([a-z0-9.-]+) +([a-z_]+)"$`)

// TestEveryRoleTheInstallerCreatesCanBeReported.
//
// A password this script generates has exactly two honest destinations:
// the configuration file it belongs in, or the operator's screen. If it
// reaches neither, it exists only as a hash inside the database and the
// service that needs it can never connect - with nothing in the output
// saying so.
//
// That happened. install.sh creates five roles; the table saying where
// each password lives had four, because L3 added the fifth to three
// lists and not the fourth. Measured on a clean cluster, with an
// upgrader.toml the script had not written:
//
//	the role was created with a generated password
//	the password was not written into the file  (correct - not ours to overwrite)
//	the password was not printed                (the defect)
//	the run reported success
//
// So the two halves of install.sh are compared: the roles it creates,
// and the roles it can account for. Reading the script rather than
// restating it, because a list here would be a fifth place to forget.
func TestEveryRoleTheInstallerCreatesCanBeReported(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRootForTest(t), "release", "install.sh"))
	if err != nil {
		t.Skipf("cannot read install.sh: %v", err)
	}

	m := installCreates.FindSubmatch(body)
	if m == nil {
		t.Fatal("install.sh has no role-creation loop this test recognises.\n" +
			"Either it moved and this check now guards nothing, or roles are no " +
			"longer created there")
	}
	created := strings.Fields(string(m[1]))
	if len(created) == 0 {
		t.Fatal("install.sh creates no roles; this test would pass by checking nothing")
	}

	accounted := map[string]string{}
	for _, row := range installCredential.FindAllSubmatch(body, -1) {
		accounted[string(row[1])] = string(row[2])
	}
	if len(accounted) == 0 {
		t.Fatal("ROLE_CREDENTIAL has no rows this test recognises, so no password " +
			"could be written, checked or reported")
	}

	for _, role := range created {
		if accounted[role] == "" {
			t.Errorf("install.sh creates role %q and ROLE_CREDENTIAL does not say where "+
				"its password lives.\n"+
				"A generated password that reaches neither a file nor the screen is lost: "+
				"the database keeps only a hash, and the service that needs it can never "+
				"connect", role)
		}
	}

	makes := map[string]bool{}
	for _, role := range created {
		makes[role] = true
	}
	for role := range accounted {
		if !makes[role] {
			t.Errorf("ROLE_CREDENTIAL names %q, which install.sh never creates. Either "+
				"the role was dropped and this row outlived it, or the creation loop "+
				"forgot it - and then nothing has a password at all", role)
		}
	}

	// And the third list, in Go, which is what the suites connect as.
	for _, role := range AllRoles {
		if !makes[role] {
			t.Errorf("the suites connect as %q and install.sh does not create it", role)
		}
	}
}
