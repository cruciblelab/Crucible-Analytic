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

// The letter class is [A-Z], not the [A-G] it was written as.
//
// [A-G] was the alphabet the day this was written, and it silently
// stopped being true the moment group H was added: a group outside the
// range matches neither the heading pattern nor the table pattern, so
// both sides of the "mirror" saw nothing and agreed with each other
// perfectly. The test kept passing. That is the same failure this test
// exists to prevent, one level up - a check nobody could check.
//
// Hardcoding today's last letter is what caused it, so it is not
// repeated: [A-Z] needs no edit when group I arrives.
var (
	// The trailing [a-z]? is not decoration. This project names split
	// phases D4a, D4b, D4c, and without it the pattern matched "D4" and
	// then demanded the em dash immediately - so every lettered phase
	// was invisible to the check whose whole job is keeping the summary
	// table honest. D4c shipped with its own heading and the table went
	// on saying eight phases.
	//
	// Found by the table check itself, which refused a count that was
	// right about the document and wrong about this pattern. The same
	// expression is what version_test.go compares a release's phase code
	// against, so the blind spot also meant a build tagged +D4c would
	// have been rejected as naming a phase that does not exist.
	phaseHeading = regexp.MustCompile(`^#### ([A-Z]I?[0-9]+(?:\.[0-9]+)?[a-z]?) — (.*)$`)
	groupRow     = regexp.MustCompile(`^\| \*\*([A-Z]I?)\*\* [^|]*\| [^*]*\*\*([0-9]+)/([0-9]+)\*\*(?:[^|]*?\(\+([0-9]+) düştü\))?`)
	groupOf      = regexp.MustCompile(`^([A-Z]I?)`)

	// A phase can also stop being work without being finished.
	//
	// A9 was the first: a designed, argued, plausible phase whose
	// central claim turned out to be false when it was finally measured
	// against the code it rested on. Neither state the table knew fitted
	// it. Calling it done would be a lie; leaving it in the denominator
	// would say the project owes work it has decided not to do, forever.
	//
	// So there is a third state, and it is deliberately expensive to
	// enter: the heading carries ❌ and has to name where the phase
	// went. Without that requirement ❌ would be a way to delete
	// inconvenient work quietly - which is the failure this whole test
	// exists to prevent, wearing a different mark.
	droppedMark = "❌"
	droppedTo   = regexp.MustCompile(`yerine ([A-Z]I?[0-9]*(?:\.[0-9]+)?[a-z]?)`)
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

	type tally struct{ done, total, dropped int }
	fromHeadings := map[string]*tally{}
	replacements := map[string]string{}
	phaseIDs := map[string]bool{}
	var seen int

	for _, line := range strings.Split(string(body), "\n") {
		if m := phaseHeading.FindStringSubmatch(line); m != nil {
			id, title := m[1], m[2]
			if parentHeadings[id] {
				continue
			}
			seen++
			phaseIDs[id] = true
			g := groupOf.FindString(id)
			if fromHeadings[g] == nil {
				fromHeadings[g] = &tally{}
			}
			if strings.Contains(title, droppedMark) {
				// Out of the denominator, into its own column - and
				// only if it says where it went.
				fromHeadings[g].dropped++
				to := droppedTo.FindStringSubmatch(title)
				if to == nil {
					t.Errorf("phase %s is marked %s and does not say what replaced it.\n"+
						"A dropped phase has to name its successor, otherwise the mark is "+
						"a way to remove work from the plan without anybody deciding to",
						id, droppedMark)
					continue
				}
				replacements[id] = to[1]
				continue
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
			var done, total, dropped int
			fmt.Sscanf(m[2], "%d", &done)
			fmt.Sscanf(m[3], "%d", &total)
			if m[4] != "" {
				fmt.Sscanf(m[4], "%d", &dropped)
			}
			fromTable[m[1]] = tally{done, total, dropped}
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
		if got.dropped != want.dropped {
			t.Errorf("group %s: the table says %d dropped phase(s), the headings say %d.\n"+
				"Write it in the status cell as *(+%d düştü)* - a phase that left the "+
				"denominator silently is indistinguishable from one that was never planned",
				g, got.dropped, want.dropped, want.dropped)
		}
	}

	// A phase cannot be dropped in favour of somewhere that does not
	// exist. Without this, "yerine X geçti" would be a sentence rather
	// than a reference, and the work would be gone with a citation to
	// nothing.
	for id, to := range replacements {
		if _, ok := fromTable[to]; ok {
			continue
		}
		if phaseIDs[to] {
			continue
		}
		t.Errorf("phase %s says it was replaced by %s, and %s is neither a group in the "+
			"table nor a phase in this document", id, to, to)
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
