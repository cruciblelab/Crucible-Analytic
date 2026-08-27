// Command sastdiff compares a gosec report against the committed
// baseline and reports only what is new.
//
// It lives under internal/ rather than in the repository's cmd/ directory
// on purpose: cmd/ holds the five binaries that make up the product, and
// "how many binaries does this product have" is a question with an exact
// answer that a development tool should not change.
//
//	gosec -fmt=json -out=gosec.json -severity=medium ./...
//	go run ./internal/sast/cmd/sastdiff -report gosec.json
//
// Exits non-zero when the report contains a finding the baseline does not
// know about, or when the baseline describes a finding that no longer
// exists.
//
// When a new finding is triaged and turns out not to be a defect, -accept
// adds it to the baseline and leaves every reason already in the file
// alone:
//
//	go run ./internal/sast/cmd/sastdiff -report gosec.json -accept
//
// That still exits non-zero, because the entry it just wrote has no
// reason in it yet. -init is the other thing entirely: it throws the
// baseline away and starts over, which empties every reason in it.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/cruciblelab/crucible-analytic/internal/sast"
)

func main() {
	var (
		reportPath   = flag.String("report", "gosec.json", "gosec JSON report to read")
		baselinePath = flag.String("baseline", ".sast-baseline.json", "committed baseline of triaged findings")
		root         = flag.String("root", "", "repository root to make report paths relative to (default: working directory)")
		initialise   = flag.Bool("init", false, "write a new baseline from the report, with empty reasons for a person to fill in")
		accept       = flag.Bool("accept", false, "add the report's new findings to the existing baseline, keeping the reasons already written")
	)
	flag.Parse()

	if *initialise && *accept {
		fmt.Fprintln(os.Stderr, "sastdiff: -init rewrites the baseline and -accept adds to it; pick one")
		os.Exit(1)
	}

	if err := run(*reportPath, *baselinePath, *root, *initialise, *accept); err != nil {
		fmt.Fprintln(os.Stderr, "sastdiff:", err)
		os.Exit(1)
	}
}

func run(reportPath, baselinePath, root string, initialise, accept bool) error {
	if root == "" {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		root = wd
	}

	rf, err := os.Open(reportPath)
	if err != nil {
		return err
	}
	defer rf.Close()

	rep, err := sast.ParseReport(rf)
	if err != nil {
		return err
	}
	rep.Relativize(root)

	if initialise {
		b := sast.BaselineFrom(rep)
		if err := writeBaseline(baselinePath, b); err != nil {
			return err
		}
		fmt.Printf("wrote %s with %d entries; every reason is empty and the test suite will refuse it until they are filled in\n",
			baselinePath, len(b.Entries))
		return nil
	}

	bf, err := os.Open(baselinePath)
	if err != nil {
		return err
	}
	defer bf.Close()

	base, err := sast.ParseBaseline(bf)
	if err != nil {
		return err
	}

	if accept {
		merged, added := sast.Merge(base, rep)
		if len(added) == 0 {
			fmt.Println("nothing to add; the baseline already covers every finding in the report")
			return nil
		}
		if err := writeBaseline(baselinePath, merged); err != nil {
			return err
		}
		for _, e := range added {
			fmt.Printf("added   %s %s\n        %s\n", e.Rule, e.File, e.Snippet)
		}
		// Non-zero on purpose. The entries just written have empty
		// reasons, and the point of the file is the reasons - so this
		// cannot be chained into a commit until a person has been back
		// to it. The test suite refuses an empty reason as well; this is
		// the earlier of the two, at the moment somebody is still
		// looking at the finding.
		return fmt.Errorf("%d entry/entries added to %s with no reason yet; write one for each before committing",
			len(added), baselinePath)
	}

	res := sast.Compare(rep, base)

	for _, e := range res.Stale {
		fmt.Printf("STALE   %s %s\n        %s\n", e.Rule, e.File, e.Snippet)
		fmt.Printf("        was triaged as: %s\n", e.Reason)
	}
	for _, i := range res.New {
		fmt.Printf("NEW     %s %s:%s (%s/%s)\n        %s\n",
			i.RuleID, i.File, i.Line, i.Severity, i.Confidence, i.Details)
	}

	switch {
	case len(res.New) > 0 && len(res.Stale) > 0:
		return fmt.Errorf("%d new finding(s), and %d baseline entry/entries describing code that is gone", len(res.New), len(res.Stale))
	case len(res.New) > 0:
		return fmt.Errorf("%d finding(s) the baseline does not know about", len(res.New))
	case len(res.Stale) > 0:
		// Failing rather than warning. A stale entry is a suppression
		// with nothing under it, and the next finding that lands on the
		// same rule and file is one a reader will assume was already
		// triaged. Deleting the line is a smaller job than working that
		// out later.
		return fmt.Errorf("%d baseline entry/entries no longer match anything; delete them", len(res.Stale))
	}

	fmt.Printf("no new findings (%d in the report, all in the baseline)\n", len(rep.Issues))
	return nil
}

func writeBaseline(path string, b *sast.Baseline) error {
	b.Note = "Triaged gosec findings. Every entry needs a reason - see internal/sast. " +
		"Regenerate with: go run ./internal/sast/cmd/sastdiff -init"

	body, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(path, body, 0o644)
}
