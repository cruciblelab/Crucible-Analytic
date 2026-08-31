package docs

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The contributor licence agreement is only worth having if everybody
// who contributed actually signed it.
//
// The failure it guards is quiet in the way this project keeps finding:
// a pull request is merged, the author never signed, nothing complains,
// and the fact surfaces years later at exactly the moment it is most
// expensive - when somebody asks whether the project can be relicensed
// and the honest answer turns out to be "not without finding a person
// who contributed once in 2027".
//
// So the two sides are compared: who git says wrote the commits, and who
// CLA-SIGNATURES.md says agreed.

// exemptAuthors are commit authors who are not contributors in the sense
// the agreement means, with the reason for each.
//
// The list is the half that can be wrong on purpose, so every entry
// carries why - the same rule the CSRF exemptions and the deadcode
// allowlist follow.
//
// # Why the assistant is listed by exact name, three times
//
// The commit trailers carry the model that wrote each session - "Claude
// Opus 5", "Claude Sonnet 5" - so a new model means a new name and this
// test going red until somebody adds it.
//
// A prefix rule ("anything starting with Claude") would spare that
// minute and cost the thing the test is for: a human contributor who
// happens to be called Claude would be silently exempted, and silent is
// the whole failure mode being guarded. One line per model, rarely, is
// the cheaper side of that trade.
//
// # Why bare "Claude" is not here
//
// It was, and it is a small demonstration that the stale half of this
// test is not decoration. 124 commits were authored as "Claude
// <noreply@anthropic.com>" before the history was rewritten to attribute
// them to the owner. The moment they were, the entry stopped describing
// anybody - and this test said so on the next run rather than leaving a
// name exempted for a reason that had expired.
var exemptAuthors = map[string]string{
	// The names the Co-Authored-By trailers carry, which vary by the
	// model a session ran on.
	"Claude Opus 5":   aiAssistantReason,
	"Claude Sonnet 5": aiAssistantReason,
}

// aiAssistantReason is why an AI assistant is not a party to the
// agreement.
//
// Worth having written down rather than assumed: if this project is ever
// offered under commercial terms, somebody's lawyer will read the commit
// history and ask. The answer being already recorded, with reasoning, is
// better than it being reconstructed under time pressure.
const aiAssistantReason = "An AI assistant, working at the owner's direction and " +
	"under the owner's account. Not a legal person and holds no copyright to " +
	"assign, so there is nobody for an agreement to be with. The work it produced " +
	"is the owner's, on the same footing as any other tool used to write code."

// TestEveryCommitAuthorHasSignedTheCLA.
//
// Reads git rather than a list, because a list of authors kept by hand
// would be a third thing to forget to update - and the whole point is
// that forgetting is what this catches.
func TestEveryCommitAuthorHasSignedTheCLA(t *testing.T) {
	root := repoRoot(t)

	if _, err := exec.LookPath("git"); err != nil {
		// A machine without git can still run every other test here. A
		// test that cannot run is not a test that failed - the same rule
		// internal/testdb's Admin follows.
		t.Skip("git is not on PATH; this test reads the commit history")
	}

	// A shallow clone cannot answer this, and saying so is not the same
	// as passing.
	//
	// actions/checkout@v4 fetches depth 1 by default, so CI saw a single
	// commit: one author, one body, and two of the exempted assistant
	// names therefore "had no commits". The stale-exemption half fired
	// and the gate was red on every push for weeks - reporting a finding
	// about the repository that was actually a fact about the checkout.
	//
	// This project has met that shape before and wrote it down: a test
	// that assumes its environment tests the environment it assumed. The
	// workflow now fetches full history so the check really runs; this
	// is what stops a contributor with a shallow clone meeting a
	// mystery.
	if out, err := exec.Command("git", "-C", root, "rev-parse",
		"--is-shallow-repository").Output(); err == nil &&
		strings.TrimSpace(string(out)) == "true" {
		t.Skip("this is a shallow clone, so the commit history is incomplete and " +
			"any answer here would be about the checkout rather than the repository. " +
			"Fetch full history (git fetch --unshallow) to run this check")
	}

	// Two queries rather than one. The first version asked for
	// "%an%n%b" and then guessed which lines were names, which meant
	// every prose line of every commit body was read as an author - the
	// test's own first run reported a Turkish sentence as an unsigned
	// contributor. Names and bodies are different things and are read
	// separately.
	names, err := exec.Command("git", "-C", root, "log", "--format=%an").Output()
	if err != nil {
		t.Skipf("git log failed (a shallow or absent checkout?): %v", err)
	}
	bodies, err := exec.Command("git", "-C", root, "log", "--format=%b").Output()
	if err != nil {
		t.Skipf("git log failed: %v", err)
	}

	signed := signatories(t, root)
	if len(signed) == 0 {
		t.Fatal("CLA-SIGNATURES.md lists nobody; this test would fail on everything " +
			"or pass on nothing depending on the parser, and neither is a check")
	}

	// Co-Authored-By is how this repository records paired work, so an
	// author who only ever appears in a trailer would otherwise be
	// invisible here.
	coAuthor := regexp.MustCompile(`(?i)^Co-Authored-By:\s*(.+?)\s*<`)

	authors := map[string]bool{}
	for _, line := range strings.Split(string(names), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			authors[line] = true
		}
	}
	for _, line := range strings.Split(string(bodies), "\n") {
		if m := coAuthor.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			authors[m[1]] = true
		}
	}

	var unsigned []string
	for name := range authors {
		if signed[name] || exemptAuthors[name] != "" {
			continue
		}
		unsigned = append(unsigned, name)
	}
	sort.Strings(unsigned)

	for _, name := range unsigned {
		t.Errorf("%q has commits in this repository and is not in CLA-SIGNATURES.md.\n"+
			"Either they signed and the file was not updated, or work was merged "+
			"without the agreement - and the second one is only discoverable now, "+
			"while the person is still reachable.\n"+
			"If they are not a contributor in the sense CLA.md means, add them to "+
			"exemptAuthors with the reason.", name)
	}

	// And the exemption list does not outlive the people in it.
	for name := range exemptAuthors {
		if !authors[name] {
			t.Errorf("%q is exempted from the CLA but has no commits; a stale "+
				"exemption is how a future contributor of the same name inherits "+
				"a decision nobody made about them", name)
		}
	}
}

// signatories reads the names out of the signature table.
func signatories(t *testing.T, root string) map[string]bool {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(root, "CLA-SIGNATURES.md"))
	if err != nil {
		t.Fatalf("reading CLA-SIGNATURES.md: %v", err)
	}

	out := map[string]bool{}
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 2 {
			continue
		}
		name := strings.TrimSpace(cells[0])
		switch name {
		case "", "Name", "Full name":
			continue
		}
		if strings.HasPrefix(name, "-") { // the header separator row
			continue
		}
		out[name] = true
	}
	return out
}
