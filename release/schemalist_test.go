// Every schema in the source tree reaches every list that applies it.
//
// No build tag: this reads four files and needs neither a database nor a
// compiler, so it belongs in the gate rather than in the nightly run
// that builds the package.
//
// It exists because the lists had already drifted. internal/heartbeat/
// schema.sql was added, wired into build.sh and install.sh, and left out
// of release_test.go's contents list and out of the CI job that applies
// the schemas - so the package shipped a file no test asserted was
// there, and CI ran its integration suites against a database missing a
// table. Nothing failed; the heartbeat tests skip when the table is
// absent, which is how a missing schema stays quiet.
//
// A list of file names copied into three places is not a design anybody
// would choose, but it is the honest one here: build.sh cannot import Go
// and neither can a test read bash. What can be shared is the
// requirement that they agree, and that is this test. CI used to be a
// fourth copy and is now not a copy at all - it runs the installer.
package release

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// schemaFiles finds every schema.sql under internal/.
//
// From the filesystem rather than from a list in this file, which is the
// whole point: a hand-written list here would be a fifth copy to forget.
func schemaFiles(t *testing.T) []string {
	t.Helper()

	root := repoRootFromWD(t)
	matches, err := filepath.Glob(filepath.Join(root, "internal", "*", "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) < 5 {
		t.Fatalf("found %d schema files under internal/, which is fewer than this project has ever had - is the glob right?", len(matches))
	}

	out := make([]string, 0, len(matches))
	for _, m := range matches {
		rel, err := filepath.Rel(root, m)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, rel)
	}
	return out
}

func repoRootFromWD(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(wd)
}

func readFile(t *testing.T, root, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// TestEverySchemaIsShippedAppliedAndTested.
//
// Three consumers, three different failures when one is missed:
//
//   - build.sh: the package does not carry the file, and the install
//     fails part-way through on somebody else's machine.
//   - install.sh: a source-tree install silently skips it.
//   - release_test.go: the package's contents are asserted without it,
//     so the first failure above has no test.
//
// CI is checked differently: it runs release/install.sh rather than
// keeping a fourth copy of the list, so what this asserts about it is
// that it still does.
func TestEverySchemaIsShippedAppliedAndTested(t *testing.T) {
	root := repoRootFromWD(t)
	schemas := schemaFiles(t)

	consumers := []struct {
		file string
		why  string
	}{
		{"release/build.sh", "the package will not carry it"},
		{"release/install.sh", "a source-tree install will skip it"},
	}

	for _, c := range consumers {
		body := readFile(t, root, c.file)
		for _, schema := range schemas {
			if !strings.Contains(body, schema) {
				t.Errorf("%s does not mention %s: %s", c.file, schema, c.why)
			}
		}
	}

	// CI is not on that list, and used not to be exempt: it carried its
	// own copy of the schema names and had already drifted from the
	// other three. It now runs release/install.sh instead, which applies
	// the list above and the grants and the hardening - so CI's database
	// is a deployment's rather than an approximation of one, and there
	// is no fourth list to forget.
	//
	// Asserted, because "CI installs it properly" is exactly the kind of
	// arrangement that gets replaced by a quicker psql loop one afternoon.
	ci := readFile(t, root, ".github/workflows/ci.yml")
	if !strings.Contains(ci, "release/install.sh") {
		t.Error(".github/workflows/ci.yml no longer runs release/install.sh; " +
			"its database is then not the one a customer gets, and a missing GRANT cannot fail a test")
	}

	// The package's contents list names schemas by their numbered
	// destination rather than their source path, so it is checked by the
	// directory each one comes from.
	contents := readFile(t, root, "release/release_test.go")
	build := readFile(t, root, "release/build.sh")
	for _, schema := range schemas {
		dir := filepath.Base(filepath.Dir(schema))
		numbered := numberedName(build, schema)
		if numbered == "" {
			// Already reported above as a missing mention.
			continue
		}
		if !strings.Contains(contents, "schema/"+numbered) {
			t.Errorf("release_test.go does not assert schema/%s (from internal/%s), so the package could ship without it and pass", numbered, dir)
		}
	}
}

// numberedName reads the destination build.sh copies a schema to.
//
// Parsed out of the script rather than derived from a convention: the
// numbering is what decides the order the files are applied in, and a
// test that reconstructed it from the directory name would agree with
// itself while disagreeing with the script.
func numberedName(build, schema string) string {
	for _, line := range strings.Split(build, "\n") {
		if !strings.Contains(line, schema) {
			continue
		}
		i := strings.Index(line, "schema/")
		if i < 0 {
			continue
		}
		rest := line[i+len("schema/"):]
		if j := strings.Index(rest, ".sql"); j >= 0 {
			return rest[:j+len(".sql")]
		}
	}
	return ""
}
