// Every role the installer creates can connect, and the installation's
// own verification says so.
//
// No build tag: this reads two files and needs no database, so it runs
// in the gate's first half.
//
// harden.sql takes CONNECT away from PUBLIC, which leaves the explicit
// grant in install.sh as the only way in. Three lists have to agree for
// that to be safe - the roles install.sh creates, the roles it grants
// CONNECT to, and the roles verify.sql checks can still connect - and
// they did not: verify.sql asked about the four services and not about
// schema_admin, the upgrader's role. Measured with install.sh made to
// skip schema_admin's grant: the install exited zero, the verification
// passed, and schema_admin could not connect ("permission denied for
// database"). Nothing connects as it until the first upgrade.
//
// The same shape L3 left in install.sh's credential table, and the same
// answer: read the lists rather than restate them, because a list here
// would be one more place to forget the fifth role.
package release

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	// installCreatesRoles is install.sh's role-creation loop.
	installCreatesRoles = regexp.MustCompile(`(?m)^  for role in ([a-z_ ]+); do$`)
	// installGrantsConnect is the grant that harden.sql's REVOKE leaves
	// as the only way in.
	installGrantsConnect = regexp.MustCompile(`GRANT CONNECT ON DATABASE \$\{DB_NAME\}\s+TO ([a-z_, ]+)"`)
	// verifyChecksConnect is one role in verify.sql's CONNECT checks.
	verifyChecksConnect = regexp.MustCompile(`has_database_privilege\('([a-z_]+)', current_database\(\), 'CONNECT'\)`)
)

func TestEveryRoleTheInstallerCreatesCanConnect(t *testing.T) {
	root := repoRootFromWD(t)
	install, err := os.ReadFile(filepath.Join(root, "release", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	verify, err := os.ReadFile(filepath.Join(root, "release", "sql", "verify.sql"))
	if err != nil {
		t.Fatal(err)
	}

	m := installCreatesRoles.FindSubmatch(install)
	if m == nil {
		t.Fatal("install.sh has no role-creation loop this test recognises.\n" +
			"Either it moved and this check now guards nothing, or roles are no " +
			"longer created there")
	}
	created := strings.Fields(string(m[1]))

	grants := installGrantsConnect.FindAllSubmatch(install, -1)
	if len(grants) != 1 {
		t.Fatalf("install.sh has %d CONNECT grants this test recognises, want exactly one", len(grants))
	}
	var granted []string
	for _, role := range strings.Split(string(grants[0][1]), ",") {
		granted = append(granted, strings.TrimSpace(role))
	}

	var checked []string
	public := 0
	for _, c := range verifyChecksConnect.FindAllSubmatch(verify, -1) {
		if string(c[1]) == "public" {
			public++
			continue
		}
		checked = append(checked, string(c[1]))
	}
	// The other half of the same pair: without it, CONNECT was never
	// revoked as far as the verification knows, and the roles above
	// could connect through PUBLIC whatever install.sh granted.
	if public != 1 {
		t.Errorf("verify.sql checks PUBLIC's CONNECT %d times, want exactly once", public)
	}

	sameRoles(t, created, granted,
		"install.sh creates %q and does not grant it CONNECT. harden.sql revokes PUBLIC's, "+
			"so the role exists and can never connect",
		"install.sh grants CONNECT to %q, which it never creates")
	sameRoles(t, created, checked,
		"install.sh creates %q and verify.sql never checks that it can connect, so an "+
			"install that failed to grant it would still report success",
		"verify.sql checks that %q can connect, and install.sh never creates it")
}

// sameRoles reports every role in one list and not the other, both ways.
func sameRoles(t *testing.T, want, got []string, missing, extra string) {
	t.Helper()
	in := func(list []string) map[string]bool {
		set := map[string]bool{}
		for _, role := range list {
			set[role] = true
		}
		return set
	}
	wantSet, gotSet := in(want), in(got)
	for _, role := range want {
		if !gotSet[role] {
			t.Errorf(missing, role)
		}
	}
	for _, role := range got {
		if !wantSet[role] {
			t.Errorf(extra, role)
		}
	}
}
