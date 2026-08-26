package sast

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// baselinePath finds the committed baseline from this package's own
// location, so the test works from any working directory.
func baselinePath(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the repository root")
	}
	return filepath.Join(filepath.Dir(self), "..", "..", ".sast-baseline.json")
}

func loadBaseline(t *testing.T) *Baseline {
	t.Helper()
	f, err := os.Open(baselinePath(t))
	if err != nil {
		t.Fatalf("opening the committed baseline: %v", err)
	}
	defer f.Close()

	b, err := ParseBaseline(f)
	if err != nil {
		t.Fatalf("parsing the committed baseline: %v", err)
	}
	return b
}

// TestBaseline_EveryEntryHasAReason.
//
// This is the difference between a baseline and a mute button. An entry
// without a reason is indistinguishable from a finding somebody silenced
// because it was noisy, and it stays indistinguishable forever - the
// person who reads it in a year has no way to tell which it was, so they
// either re-triage everything or trust all of it.
//
// The bootstrap path writes every reason empty on purpose (see
// BaselineFrom), so this test is what stands between "generated" and
// "triaged".
func TestBaseline_EveryEntryHasAReason(t *testing.T) {
	b := loadBaseline(t)
	if len(b.Entries) == 0 {
		t.Fatal("the baseline is empty; this test would pass by checking nothing")
	}

	for _, e := range b.Entries {
		if strings.TrimSpace(e.Reason) == "" {
			t.Errorf("%s in %s has no reason: a baseline entry without one is just a suppression", e.Rule, e.File)
		}
		// A reason is meant to be an argument, not an acknowledgement.
		// "false positive" and "not an issue" restate the decision
		// rather than support it.
		if len(strings.Fields(e.Reason)) < 6 {
			t.Errorf("%s in %s has a reason too short to be one: %q", e.Rule, e.File, e.Reason)
		}
	}
}

// TestBaseline_NoDuplicateFingerprints. Two entries with one fingerprint
// means one of them can never go stale, so it would outlive the code it
// describes without anything saying so.
func TestBaseline_NoDuplicateFingerprints(t *testing.T) {
	b := loadBaseline(t)

	seen := map[string]Entry{}
	for _, e := range b.Entries {
		if prev, ok := seen[e.Fingerprint]; ok {
			t.Errorf("fingerprint %s appears twice: %s in %s, and %s in %s",
				e.Fingerprint, prev.Rule, prev.File, e.Rule, e.File)
		}
		seen[e.Fingerprint] = e
	}
}

// TestBaseline_PathsAreRelative.
//
// An absolute path in the baseline means it was generated without
// -root, or on a machine whose checkout lives somewhere else. Either way
// it matches nothing on a CI runner, so every finding reads as new and
// the scan reports twenty-one things every night until somebody turns it
// off.
func TestBaseline_PathsAreRelative(t *testing.T) {
	b := loadBaseline(t)
	for _, e := range b.Entries {
		if strings.HasPrefix(e.File, "/") {
			t.Errorf("absolute path in the baseline: %s. Regenerate with -root, or it will match nothing outside this machine", e.File)
		}
	}
}

// TestBaseline_CoversTheToolItself.
//
// The scanner scanning its own code is not a curiosity, it is the
// property that keeps the exemption list from starting. gosec found
// three findings in sastdiff the moment it existed, and they are
// triaged in the baseline like everything else.
func TestBaseline_CoversTheToolItself(t *testing.T) {
	b := loadBaseline(t)
	for _, e := range b.Entries {
		if strings.Contains(e.File, "internal/sast/") {
			return
		}
	}
	t.Error("no baseline entry from internal/sast: either the tool stopped being scanned, or it was excluded - the first thing a scanner should not do is skip itself")
}
