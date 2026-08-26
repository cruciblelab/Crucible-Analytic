package docs

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// parentHeadings are phases whose children are counted instead of them.
//
// A5 and C7 were each split once the work turned out to be two or three
// separate decisions (A5.1/A5.2, C7.1/C7.2/C7.3). The parent heading
// stays because commit messages and risk-table rows refer to it by the
// old name, and this document's rule about numbering is to explain
// rather than to renumber.
var parentHeadings = map[string]bool{"A5": true, "C7": true}

var (
	phaseHeading = regexp.MustCompile(`^#### ([A-G]I?[0-9]+(?:\.[0-9]+)?) — (.*)$`)
	groupRow     = regexp.MustCompile(`^\| \*\*([A-G]I?)\*\* [^|]*\| [^*]*\*\*([0-9]+)/([0-9]+)\*\*`)
	groupOf      = regexp.MustCompile(`^([A-G]I?)`)
)

// TestPlan_TheGroupTableMatchesThePhaseHeadings.
//
// PLAN.md answers "where are we" in two places that have to agree: a
// summary table near the top, and the phase headings themselves, which
// carry a checkmark when the phase is done. The table is written by
// hand at the end of a phase and the headings are edited in the middle
// of one, so they drift - and the drift is silent, because nobody reads
// three thousand lines to audit a table.
//
// They had drifted in three places when this was written, in both
// directions. C6 and D2 were finished, and shipped, and their headings
// never said so; the table meanwhile credited group B with one finished
// phase, and group B has never had one. Group A's total was one short of
// the phases it contains.
//
// None of that broke anything. It is worse than that: the table is what
// the customer is shown when they ask what state the project is in, so
// the one number nobody could check was the one number everybody read.
func TestPlan_TheGroupTableMatchesThePhaseHeadings(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "PLAN.md"))
	if err != nil {
		t.Fatal(err)
	}

	type tally struct{ done, total int }
	fromHeadings := map[string]*tally{}
	var seen int

	for _, line := range strings.Split(string(body), "\n") {
		if m := phaseHeading.FindStringSubmatch(line); m != nil {
			id, title := m[1], m[2]
			if parentHeadings[id] {
				continue
			}
			seen++
			g := groupOf.FindString(id)
			if fromHeadings[g] == nil {
				fromHeadings[g] = &tally{}
			}
			fromHeadings[g].total++
			if strings.Contains(title, "✅") {
				fromHeadings[g].done++
			}
		}
	}
	// A parser that matched nothing would agree with an empty table.
	if seen < 20 {
		t.Fatalf("only %d phase headings found; the parser is not reading PLAN.md", seen)
	}

	fromTable := map[string]tally{}
	for _, line := range strings.Split(string(body), "\n") {
		if m := groupRow.FindStringSubmatch(line); m != nil {
			var done, total int
			fmt.Sscanf(m[2], "%d", &done)
			fmt.Sscanf(m[3], "%d", &total)
			fromTable[m[1]] = tally{done, total}
		}
	}
	if len(fromTable) == 0 {
		t.Fatal("the group table was not found; this test would pass by comparing nothing")
	}

	for g, want := range fromHeadings {
		got, ok := fromTable[g]
		if !ok {
			t.Errorf("group %s has %d phases and no row in the group table", g, want.total)
			continue
		}
		if got.done != want.done || got.total != want.total {
			t.Errorf("group %s: the table says %d/%d, the headings say %d/%d",
				g, got.done, got.total, want.done, want.total)
		}
	}
	for g := range fromTable {
		if fromHeadings[g] != nil {
			continue
		}
		// AI is the one group this parser deliberately does not read.
		// Its phases are a different shape - "###" rather than "####",
		// two numbering families (AI.2/AI.3 and AI-1/AI-2, which the
		// plan itself explains), and two of them carry their completion
		// on the section heading above rather than their own.
		//
		// Teaching the parser all of that would be a lot of code for a
		// group that is finished and will not gain another phase. So it
		// is excluded - but the exclusion is not a free pass: if the
		// group ever reopens, the check below fails and whoever reopened
		// it has to decide what this test should do about it.
		if g == "AI" {
			if row := fromTable[g]; row.done != row.total {
				t.Errorf("group AI is no longer complete (%d/%d), so it can no longer be "+
					"skipped here; teach this test its heading shape", row.done, row.total)
			}
			continue
		}
		t.Errorf("the group table has a row for %s, which has no phases", g)
	}
}
