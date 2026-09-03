package invariants

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/schemafiles"
)

// dockerSchemaCopy matches the Dockerfile lines that put a schema file
// into the image, and pulls out both the source path and the numbered
// name it lands under.
var dockerSchemaCopy = regexp.MustCompile(
	`(?m)^COPY\s+(internal/\S+/schema\.sql)\s+/opt/crucible-analytic/schema/(\d+)-\S+\.sql\s*$`)

// TestTheImageCarriesEverySchemaFile.
//
// # What this is for
//
// The Dockerfile names its schema files one COPY at a time, so the image
// has a hand-written copy of a list that already exists in
// internal/schemafiles. It went short, and nothing said so: the list
// stopped at six while the schema grew to ten.
//
// The four it was missing were panel_logs, panel_upgrade_requests,
// ip_range_refresh_requests and schema_version - the log sink, the
// upgrade queue, the refresh queue, and the row that records what shape
// the database is in. Every container install has been without them
// since the day each landed.
//
// It surfaced as the init container exiting 3 on
//
//	ERROR: relation "schema_version" does not exist
//	STATEMENT: INSERT INTO schema_version ...
//
// which is install.sh doing its last step against a table nobody had
// created.
//
// # Why it went unseen
//
// Two guards were broken at once and the outer one hid the inner. These
// files are exercised only by the `docker` build tag, which runs only in
// the nightly - and the nightly could not get past its own first job,
// because e2e called install.sh without --no-systemd and the script
// rightly refused. So the pipeline that would have reported this had
// been failing for a different reason since before the gap opened.
//
// A guard that never runs is not a weaker guard. It is the absence of
// one, and it looks identical from the outside.
//
// # The rule
//
// Same as the other mirrors here: one side is derived, the other is the
// file people edit, and either moving alone fails. Order is checked too,
// because these are applied in numeric filename order and the schema's
// order is load bearing - internal/rangerefresh's comments point at a
// table internal/asnlookup creates.
func TestTheImageCarriesEverySchemaFile(t *testing.T) {
	root := repoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatalf("reading the Dockerfile: %v", err)
	}

	matches := dockerSchemaCopy.FindAllStringSubmatch(string(body), -1)
	if len(matches) == 0 {
		t.Fatal("the Dockerfile copies no schema files, so this test is comparing " +
			"nothing against nothing - the pattern above has probably stopped " +
			"matching how they are written")
	}

	inImage := make([]string, 0, len(matches))
	numbers := make([]string, 0, len(matches))
	for _, m := range matches {
		inImage = append(inImage, filepath.ToSlash(m[1]))
		numbers = append(numbers, m[2])
	}

	want := make([]string, 0, len(schemafiles.InOrder))
	for _, f := range schemafiles.InOrder {
		want = append(want, f.Path)
	}

	inImageSet := map[string]bool{}
	for _, p := range inImage {
		inImageSet[p] = true
	}
	for _, p := range want {
		if !inImageSet[p] {
			t.Errorf("%s is part of the schema and the image does not carry it.\n"+
				"A container install applies what is in /opt/crucible-analytic/schema and "+
				"nothing else, so the tables this file creates simply will not exist "+
				"there - silently, until something asks for one", p)
		}
	}
	wantSet := map[string]bool{}
	for _, p := range want {
		wantSet[p] = true
	}
	for _, p := range inImage {
		if !wantSet[p] {
			t.Errorf("the Dockerfile copies %s, which is not in schemafiles.InOrder.\n"+
				"Either it was removed from the schema and the COPY outlived it, or "+
				"the image is applying something the rest of the system does not", p)
		}
	}

	// And the order, which numeric filenames decide.
	if len(inImage) == len(want) {
		for i := range inImage {
			if inImage[i] != want[i] {
				t.Errorf("the image applies these in a different order from "+
					"schemafiles.InOrder: position %d is %s in the Dockerfile and %s "+
					"in the schema.\nThe order is load bearing - a file that expects a "+
					"table an earlier one creates fails on a fresh database",
					i+1, inImage[i], want[i])
				break
			}
		}
	}
	if !strings.HasPrefix(numbers[0], "0") && numbers[0] != "10" {
		t.Errorf("the first schema file is numbered %q; they are applied in filename "+
			"order, so the numbers have to sort the way the schema does", numbers[0])
	}
}
