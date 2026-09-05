package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The allowlist reader, tested where it is easiest to get wrong.
//
// This tool had no tests until the "unseen" category arrived, which is
// its own small lesson: it was one function in main, and one function in
// main is where a check quietly stops checking. The failure mode is
// silent by construction - a parser that dropped every entry would make
// the gate pass on everything, and nothing else in CI would notice.

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "allowlist.txt")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestTheTwoCategoriesAreKeptApart.
//
// They are checked in opposite directions - an ordinary entry must stay
// in the report, an unseen one must stay out of it - so an entry read
// into the wrong map is not a cosmetic mistake. It inverts the check
// that entry gets.
func TestTheTwoCategoriesAreKeptApart(t *testing.T) {
	path := write(t, `
# A comment, and a blank line above it.

pkg.Ordinary            # built ahead of its page
unseen: pkg.Invisible   # RTA lost sight of it when the type went behind an interface
   unseen: pkg.Indented # leading space must not change the category
`)

	allowed, unseen, err := readAllowlist(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(allowed) != 1 || allowed["pkg.Ordinary"] == "" {
		t.Errorf("ordinary entries: %v", allowed)
	}
	if len(unseen) != 2 || unseen["pkg.Invisible"] == "" || unseen["pkg.Indented"] == "" {
		t.Errorf("unseen entries: %v", unseen)
	}
	if _, ok := allowed["unseen: pkg.Invisible"]; ok {
		t.Error("the marker was kept as part of the name, so the entry can never match a report")
	}
}

// TestAnEntryWithoutAReasonIsRefused, in both categories.
//
// The reason is the only thing that stops this file from becoming a
// place to make findings go away, and an unseen entry needs one more
// than an ordinary one: nothing will ever go red to make somebody look
// at it again.
func TestAnEntryWithoutAReasonIsRefused(t *testing.T) {
	for _, line := range []string{
		"pkg.Ordinary",
		"pkg.Ordinary #",
		"unseen: pkg.Invisible",
		"unseen: pkg.Invisible #   ",
	} {
		if _, _, err := readAllowlist(write(t, line+"\n")); err == nil {
			t.Errorf("%q was accepted with no reason", line)
		}
	}
}

// TestTheCommittedAllowlistParses.
//
// The tests above use fixtures, and a parser that is right about
// fixtures and wrong about the real file is the worst of both. This
// reads the one CI actually gates on.
func TestTheCommittedAllowlistParses(t *testing.T) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the repository root")
	}
	path := filepath.Join(filepath.Dir(self), "..", "..", "deadcode_allowlist.txt")

	allowed, unseen, err := readAllowlist(path)
	if err != nil {
		t.Fatalf("the committed allowlist does not parse: %v", err)
	}
	if len(allowed) == 0 {
		t.Error("no ordinary entries were read, which would make the gate pass on anything")
	}
	for name, reason := range unseen {
		if !strings.Contains(reason, "MOD1") {
			t.Errorf("the unseen entry %s does not say when it went unseen. "+
				"Without that, nobody can tell whether the blindness is still the "+
				"reason or the entry has simply been forgotten", name)
		}
	}
}
