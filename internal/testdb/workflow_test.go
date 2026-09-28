package testdb

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// passwordReset matches a shell loop that sets each role's password to
// its own name: the `for` line, and the ALTER ROLE at most two lines
// below it. The places write it differently - `role` in the workflows,
// `r` in the instructions for people, which docker-compose.yml also
// comments out - so both variables are captured and the test checks
// that the ALTER ROLE uses the loop's.
//
// Anchored on the loop rather than on role names, so a loop that
// stopped listing a role is a loop this still finds and can report as
// incomplete. Looking for the names instead would find nothing and pass.
var passwordReset = regexp.MustCompile(
	`for (\w+) in ([a-z_ ]+); do\n(?:[^\n]*\n){0,2}?[^\n]*ALTER ROLE \$\{?(\w+)\}? PASSWORD`)

// alterRolePassword is any password change at all, so that one this test
// does not recognise as a loop is reported rather than skipped.
var alterRolePassword = regexp.MustCompile(`ALTER ROLE \S+ PASSWORD`)

// passwordResetFiles are the places that tell a machine or a person to
// set the suites' passwords.
var passwordResetFiles = []string{
	".github/workflows/ci.yml",
	".github/workflows/nightly.yml",
	"README.md",
	"docker-compose.yml",
}

// TestEveryPasswordResetKnowsEveryRole.
//
// The suites connect as five roles with the convention "the password is
// the role name". Installing the database the way a customer does
// generates real passwords, so every setup resets each role's password
// to its own name afterwards: CI, the nightly run, and the development
// instructions in README.md and docker-compose.yml.
//
// Each reset is a hand-written list in a shell loop, and a hand list in
// a file no Go tool reads is a list nothing keeps honest. CI's stopped
// being complete when the fifth role arrived, and the symptom was not
// "schema_admin is missing" but
//
//	failed SASL auth: FATAL: password authentication failed for user "schema_admin"
//
// which reads like a database problem rather than like a line in a
// workflow. Every applier and upgrade test failed on it, on each of the
// seventeen pushes of the day L3 landed, and nobody looked: the local
// gate was green. The test written then read ci.yml only, and the two
// lists written for people stayed at four: following README.md on a
// fresh install, measured, fails the upgrade suite with exactly that
// line.
//
// So every list is compared, both ways. The lists are the half that can
// fall behind; AllRoles is the half the code actually uses.
func TestEveryPasswordResetKnowsEveryRole(t *testing.T) {
	root := repoRootForTest(t)
	known := map[string]bool{}
	for _, role := range AllRoles {
		known[role] = true
	}

	for _, name := range passwordResetFiles {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil {
			// A checkout without the file is not a failure of this
			// project's code, and a test that cannot read its subject
			// has not found a defect.
			t.Logf("cannot read %s: %v", name, err)
			continue
		}

		loops := passwordReset.FindAllSubmatch(body, -1)
		if len(loops) == 0 {
			t.Errorf("%s has no password reset this test recognises.\n"+
				"Either it moved - in which case this test now checks nothing there "+
				"and must follow it - or it was removed, and whoever follows the file "+
				"will fail to authenticate", name)
			continue
		}
		if all := len(alterRolePassword.FindAll(body, -1)); all != len(loops) {
			t.Errorf("%s changes a role's password %d times and this test recognises %d "+
				"of them as a loop over the roles; the rest go unchecked", name, all, len(loops))
		}

		for _, m := range loops {
			if loopVar, usedVar := string(m[1]), string(m[3]); loopVar != usedVar {
				t.Errorf("%s loops over roles as $%s and sets the password of $%s", name, loopVar, usedVar)
			}
			listed := map[string]bool{}
			for _, role := range strings.Fields(string(m[2])) {
				listed[role] = true
				if !known[role] {
					t.Errorf("%s sets a password for %q, which is not a role any suite "+
						"connects as. Either it was renamed here and not there, or the "+
						"line outlived the role", name, role)
				}
			}
			for _, role := range AllRoles {
				if !listed[role] {
					t.Errorf("%s does not set a password for %q.\n"+
						"install.sh generates one, the suites connect with the role name, "+
						"and the failure surfaces as a SASL error that names the database "+
						"rather than this line", name, role)
				}
			}
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
