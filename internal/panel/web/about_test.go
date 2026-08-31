package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The credits are a claim about who made this, shown to every customer.
// Two things can quietly stop being true about them, and each has a test.

// TestEveryContributorHasSignedTheCLA.
//
// The panel says three names made this software. CLA-SIGNATURES.md is
// the record of who agreed that their work could be in it. Those are the
// same question asked in two places, so they are compared.
//
// The failure this guards is not vandalism, it is drift: somebody joins,
// signs, and is added to one file; or the credits gain a name that never
// signed and the first person to notice is a lawyer reading the panel
// during due diligence.
//
// The AI assistant is the deliberate exception, and it is the same
// exception internal/docs/cla_test.go already makes with the same
// reasoning - it is not a legal person and holds no copyright to assign.
// Named here rather than pattern-matched, for the reason that file
// records: a prefix rule would silently exempt a human of that name.
func TestEveryContributorHasSignedTheCLA(t *testing.T) {
	signatures := readRepoFile(t, "CLA-SIGNATURES.md")

	for _, c := range contributors {
		if c.Name == "Claude" {
			continue // see aiAssistantReason in internal/docs/cla_test.go
		}
		if !strings.Contains(signatures, c.Name) {
			t.Errorf("the settings page credits %q and CLA-SIGNATURES.md does not list them.\n"+
				"Either they have not signed - in which case the panel is telling every "+
				"customer something the paperwork does not support - or the signature "+
				"file was not updated", c.Name)
		}
	}
}

// TestTheAboutBlockAgreesWithTheRepository.
//
// Three facts on that page are also written down elsewhere in the tree,
// and all three are the sort that rot silently: a repository that moves,
// a licence that changes, a contact that leaves.
//
// Checked against the files rather than restated here, because a test
// that carried its own copy of the answer would be a fourth place to
// forget.
func TestTheAboutBlockAgreesWithTheRepository(t *testing.T) {
	// The licence. LICENSE is the Apache text itself, so the version
	// line in it is what is compared - not the file name, which would
	// still say LICENSE after somebody replaced its contents.
	licence := readRepoFile(t, "LICENSE")
	if LicenceName == "Apache-2.0" && !strings.Contains(licence, "Apache License") {
		t.Errorf("the panel says the licence is %s and LICENSE is not the Apache text.\n"+
			"A panel that names the wrong licence is worse than one that names none: "+
			"somebody relies on it", LicenceName)
	}

	// The repository. NOTICE carries it as the attribution a
	// redistributor has to reproduce, so the two must be the same
	// place.
	notice := readRepoFile(t, "NOTICE")
	if !strings.Contains(strings.ToLower(notice), strings.ToLower(repoPath(RepositoryURL))) {
		t.Errorf("the panel sends people to %s and NOTICE does not mention that repository",
			RepositoryURL)
	}

	// And it is a link somebody can follow, not a placeholder.
	if !strings.HasPrefix(RepositoryURL, "https://") {
		t.Errorf("the repository URL is %q; a panel that shows a non-https link teaches "+
			"the reader that this panel's links are not to be trusted", RepositoryURL)
	}
}

// TestEveryContributorMarkIsEmbedded.
//
// A badge that does not resolve renders as nothing, which looks like a
// decision rather than a missing file - so nothing on the page would
// ever say the asset had been dropped.
//
// Asked of the real asset set, the one the binary serves.
func TestEveryContributorMarkIsEmbedded(t *testing.T) {
	assets, err := ui.LoadAssets()
	if err != nil {
		t.Fatalf("loading the embedded assets: %v", err)
	}
	for _, c := range contributors {
		if c.Mark == "" {
			continue
		}
		if assets.URL(c.Mark) == "" {
			t.Errorf("%s's mark %q is not among the embedded assets, so the row would "+
				"draw without one and nothing would say why", c.Name, c.Mark)
		}
	}
}

// TestNoContributorMarkClaimsAVendorLogo.
//
// The one rule that is easy to break later with the best intentions:
// somebody decides the credits look bare and drops in a real vendor
// logo. claudeMarkReason is why that is not a cosmetic choice.
//
// The check is deliberately literal - the mark files are ours, drawn
// here, and none of them may carry a vendor's name in a way that says
// "this is their logo".
func TestNoContributorMarkClaimsAVendorLogo(t *testing.T) {
	if claudeMarkReason == "" {
		t.Fatal("the reason the assistant has no vendor logo has been emptied; " +
			"it is the only thing recording that the omission was a decision")
	}

	root := repoRoot(t)
	for _, c := range contributors {
		if c.Mark == "" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(root,
			"internal", "panel", "ui", "static", c.Mark))
		if err != nil {
			t.Fatalf("reading %s: %v", c.Mark, err)
		}
		for _, vendor := range []string{"anthropic"} {
			if strings.Contains(strings.ToLower(string(body)), vendor) {
				// The comment in marka-claude.svg names Anthropic while
				// explaining why their logo is absent, which is the
				// opposite of the problem - so this looks for it in the
				// files that are not that one.
				if c.Name == "Claude" {
					continue
				}
				t.Errorf("%s mentions %q; a vendor's mark in this directory is a "+
					"trademark use nobody granted. See claudeMarkReason", c.Mark, vendor)
			}
		}
	}
}

// readRepoFile reads a file from the repository root.
func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(body)
}

// repoRoot walks up to the directory holding go.mod.
//
// Computed rather than "../../..", so moving this package does not turn
// these tests into checks of a file that is not there - which would fail
// loudly, but for the wrong reason, and the fix would be to correct a
// path rather than to look at what changed.
func repoRoot(t *testing.T) string {
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

// repoPath reduces a repository URL to "owner/name", which is the form
// NOTICE and the module path both carry regardless of scheme or host
// casing.
func repoPath(url string) string {
	trimmed := strings.TrimSuffix(url, "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 {
		return trimmed
	}
	return strings.Join(parts[len(parts)-2:], "/")
}

// TestNobodyIsBothAContributorAndOnTheTeamList.
//
// The two lists make different claims - one says "their work is in this,
// under a signed agreement", the other says "they are on the team and
// have not signed". A name in both draws twice and asserts both, and the
// likeliest way it happens is somebody signing and being added to the
// contributors without being removed from here.
func TestNobodyIsBothAContributorAndOnTheTeamList(t *testing.T) {
	credited := map[string]bool{}
	for _, c := range contributors {
		credited[c.Name] = true
	}
	for _, c := range team {
		if credited[c.Name] {
			t.Errorf("%q is in both contributors and team. Those are different "+
				"claims: one is covered by CLA-SIGNATURES.md and the other is not, "+
				"and the page would say both", c.Name)
		}
	}
}

// TestNobodyOnTheTeamListIsWithoutARole.
//
// A name with no role renders as a bare word in a list under a heading,
// which tells a reader nothing about why it is there - and a credits
// block that cannot say why somebody is in it invites the reading that
// names get added casually.
func TestNobodyOnTheTeamListIsWithoutARole(t *testing.T) {
	for _, c := range team {
		if strings.TrimSpace(c.Name) == "" {
			t.Error("a team entry has no name")
		}
		if c.RoleKey == "" {
			t.Errorf("%q is on the team list with no role key, so the page draws a bare name",
				c.Name)
		}
	}
}

// TestEveryCategoryHasALabel.
//
// The page walks panel.CategoryOrder and looks each one up here. A
// category with no entry draws a section headed by an empty string -
// visible, unexplained, and pointing at nothing a reader can act on.
//
// Both directions, because a stale entry is the other half: a label kept
// after its category went is text the catalogue still carries and no
// page can reach, which is exactly what internal/panel/ui's dead-catalog
// check exists to stop accumulating.
func TestEveryCategoryHasALabel(t *testing.T) {
	if len(panel.CategoryOrder) == 0 {
		t.Fatal("CategoryOrder is empty; this test would pass by checking nothing")
	}
	for _, cat := range panel.CategoryOrder {
		if categoryLabelKey[cat] == "" {
			t.Errorf("category %q has no label key, so its section would be headed by "+
				"an empty string", cat)
		}
	}
	known := map[panel.Category]bool{}
	for _, cat := range panel.CategoryOrder {
		known[cat] = true
	}
	for cat := range categoryLabelKey {
		if !known[cat] {
			t.Errorf("categoryLabelKey has %q, which is not in CategoryOrder; the page "+
				"can never reach it and its catalogue text is unreachable too", cat)
		}
	}
}
