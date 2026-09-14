package docs

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel/preflight"
)

// checkCountSentence is KURULUM.md's claim about how many checks the
// wizard's last step runs.
//
// The number is spelled in Turkish words rather than digits, which is
// how the document writes counts - so the pattern reads the word and the
// map below turns it into a number. Anchored on the sentence rather than
// on a bare number: a document this long has many numbers and only one
// of them is this claim.
var checkCountSentence = regexp.MustCompile(
	`7\. adım \*\*(on altı|on beş|on yedi|on dört|on sekiz|yirmi) kontrol\*\* çalıştırır`)

var turkishNumbers = map[string]int{
	"on dört": 14, "on beş": 15, "on altı": 16, "on yedi": 17, "on sekiz": 18,
	"yirmi": 20,
}

// The count in KURULUM.md is the count the code produces.
//
// It said fourteen for months while the code produced sixteen, and
// nothing noticed - a number in prose is the kind of fact that is true
// when written and only accidentally true afterwards. An installer
// counting rows on the screen and finding two more than the manual
// promised has been given a reason to distrust the rest of it.
//
// Measured with no addresses configured and the one warning that
// replaces them subtracted, because that line is described separately
// in the same paragraph: the base count is what this asserts.
func TestTheSetupCheckCountInTheManualIsTheCountTheCodeRuns(t *testing.T) {
	root := repoRoot(t)
	body, err := os.ReadFile(filepath.Join(root, "KURULUM.md"))
	if err != nil {
		t.Fatalf("reading KURULUM.md: %v", err)
	}

	m := checkCountSentence.FindSubmatch(body)
	if m == nil {
		t.Fatal("KURULUM.md no longer contains the sentence naming how many checks " +
			"step 7 runs, or it names a number this test does not know how to read.\n" +
			"Either the paragraph moved - in which case this check now verifies " +
			"nothing and must follow it - or the claim was dropped.")
	}
	claimed, ok := turkishNumbers[string(m[1])]
	if !ok {
		t.Fatalf("KURULUM.md says %q checks and this test cannot turn that into a "+
			"number; add it to turkishNumbers", m[1])
	}

	// A nil Checker with an empty Config: every database check reports
	// a skip and every unconfigured one too, which is exactly right
	// here - the question is how many lines there are, not what they
	// say.
	results := preflight.New(nil, false).Run(context.Background(), preflight.Config{})

	base := 0
	for _, r := range results {
		if strings.HasPrefix(r.ID, "service.") {
			continue
		}
		base++
	}
	if base == 0 {
		t.Fatal("the preflight run produced no non-service checks at all; either " +
			"Run changed shape or this count is reading the wrong thing")
	}

	if base != claimed {
		t.Errorf("KURULUM.md §9.3 says step 7 runs %d checks and preflight.Run "+
			"produces %d (not counting the service lines, which the same paragraph "+
			"describes separately).\n"+
			"Whichever moved, the document is the half a reader trusts: fix the "+
			"sentence, or the check list.", claimed, base)
	}
}
