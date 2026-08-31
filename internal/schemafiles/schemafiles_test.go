package schemafiles

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
)

// This package is a list, and a list is dangerous the way every other
// hand-written list in this repository has been: not when it is wrong,
// but when it is short. A schema.sql added to a new package and not
// registered here would leave the applier migrating a database to a
// shape that is missing a table, and every existing test would pass.
//
// So all three of the things this list has to agree with are checked
// against it: the files on disk, the order release/build.sh stages them
// in, and the fingerprint constant.

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(filepath.Dir(wd)) // internal/schemafiles -> repo root
}

// TestEverySchemaOnDiskIsRegistered.
//
// The half that catches a new package. Globbed rather than listed,
// because a list checked against a list is one list.
func TestEverySchemaOnDiskIsRegistered(t *testing.T) {
	root := repoRoot(t)
	matches, err := filepath.Glob(filepath.Join(root, "internal", "*", "schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no schema.sql found on disk; this test would pass by comparing nothing")
	}

	registered := Map()
	var onDisk []string
	for _, m := range matches {
		rel, err := filepath.Rel(root, m)
		if err != nil {
			t.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		onDisk = append(onDisk, rel)

		body, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		sql, ok := registered[rel]
		if !ok {
			t.Errorf("%s exists on disk and is not in InOrder.\n"+
				"The applier migrates a database using exactly this list, so an "+
				"unregistered schema is a table that never gets created - and nothing "+
				"else will notice, because every other test runs against a database "+
				"somebody installed by hand.", rel)
			continue
		}
		if sql != string(body) {
			t.Errorf("%s is registered but the embedded copy differs from the file", rel)
		}
	}

	// And the other direction: an entry whose file is gone.
	for path := range registered {
		found := false
		for _, d := range onDisk {
			if d == path {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("InOrder names %s, which is not on disk", path)
		}
	}
}

// TestTheOrderMatchesTheReleasePackage.
//
// build.sh stages the same files under numbered names, and that
// numbering is the order install.sh applies them in. Two orderings
// exist because the two consumers are different - a shell script
// copying files and a Go slice - but they describe one fact, and a
// second ordering nobody checks is the shape this project has found
// broken more than once.
//
// retention after the hypertables, schemaver last: both are asserted by
// this agreeing with the script that already gets them right.
func TestTheOrderMatchesTheReleasePackage(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "release", "build.sh"))
	if err != nil {
		t.Fatal(err)
	}

	// cp internal/panel/schema.sql "${STAGE}/schema/01-panel.sql"
	line := regexp.MustCompile(`cp\s+(internal/\w+/schema\.sql)\s+"\$\{STAGE\}/schema/(\d+)-`)
	matches := line.FindAllStringSubmatch(string(body), -1)
	if len(matches) == 0 {
		t.Fatal("no schema copies found in build.sh; the extraction is broken and " +
			"this test would pass by comparing nothing")
	}

	type staged struct{ path, number string }
	var fromScript []staged
	for _, m := range matches {
		fromScript = append(fromScript, staged{m[1], m[2]})
	}
	sort.Slice(fromScript, func(i, j int) bool {
		return fromScript[i].number < fromScript[j].number
	})

	if len(fromScript) != len(InOrder) {
		var names []string
		for _, s := range fromScript {
			names = append(names, s.path)
		}
		t.Fatalf("build.sh stages %d schemas and InOrder has %d.\n"+
			"build.sh: %s\nA schema in one and not the other is a table that exists "+
			"after a tarball install and not after a panel upgrade, or the reverse.",
			len(fromScript), len(InOrder), strings.Join(names, ", "))
	}
	for i, s := range fromScript {
		if s.path != InOrder[i].Path {
			t.Errorf("position %d: build.sh stages %s, InOrder has %s",
				i, s.path, InOrder[i].Path)
		}
	}
}

// TestTheEmbeddedSchemaIsTheOneTheConstantNames.
//
// schemaver.Fingerprint is written by hand and this is computed from
// the bytes actually compiled into the binary. They are two sides of a
// mirror: the applier refuses a request whose fingerprint does not match
// the constant, so if the constant and the embedded files ever disagree
// the applier would be verifying one schema and applying another.
func TestTheEmbeddedSchemaIsTheOneTheConstantNames(t *testing.T) {
	if got := Fingerprint(); got != schemaver.Fingerprint {
		t.Errorf("the embedded schema hashes to\n  %s\nand schemaver.Fingerprint is\n  %s\n\n"+
			"The applier checks a request against the constant and then applies these "+
			"bytes. While the two disagree it is verifying one schema and running "+
			"another, which is worse than not checking at all.", got, schemaver.Fingerprint)
	}
}
