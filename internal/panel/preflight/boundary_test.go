package preflight

import (
	"context"
	"go/build"
	"strings"
	"testing"
)

// TestPreflightDoesNotImportThePanel is the test the split exists for.
//
// Before it, these checks were methods on the panel's Store, and every
// one of them was reachable only by constructing the whole panel: users,
// sessions, audit log, settings, the developer gate. That is a lot of
// machinery to ask "can the beacon's role write to this table", and it
// meant the tests for these checks built a Store too.
//
// Splitting the files out only helps if the dependency goes with them. A
// package sitting in its own directory while importing everything it
// used to be part of has been moved, not separated - and nothing in the
// compiler notices the difference. So the rule is asserted here rather
// than described in a comment somebody will contradict later.
//
// If a check genuinely needs something from the panel, pass it in
// through Config, the way GuardedKeys is passed in. That keeps the
// knowledge at the wiring point, where the binary already imports both.
func TestPreflightDoesNotImportThePanel(t *testing.T) {
	const self = "github.com/cruciblelab/crucible-analytic/internal/panel"

	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("reading this package: %v", err)
	}

	// Imports only, not TestGoFiles: the demo test may reasonably reach
	// for the panel's real key list to show real output, and a test
	// dependency does not travel into anyone's binary.
	for _, imported := range pkg.Imports {
		if imported == self || strings.HasPrefix(imported, self+"/") {
			t.Errorf("this package imports %q; preflight inspects a deployment and must not "+
				"drag the panel's store, sessions and auth into every binary that runs a check. "+
				"Pass what the check needs through Config instead.", imported)
		}
	}
}

// TestCheckerNeedsOnlyAPool records the other half of the same idea: the
// constructor's signature is the boundary, and a Checker that grew a
// *panel.Store parameter would pass the import test above while undoing
// what it protects.
func TestCheckerNeedsOnlyAPool(t *testing.T) {
	// A nil pool is enough to build one, which is the point - every
	// non-database check runs without touching Postgres at all.
	c := New(nil, false)
	if c == nil {
		t.Fatal("New returned nil")
	}
	if c.ipTokenKeyConfigured {
		t.Error("ipTokenKeyConfigured defaulted to true; the safe default is 'we were not told'")
	}
	if got := c.checkIPTokenKey(); got.Status != CheckSkip {
		t.Errorf("status = %s, want skip: a deployment that never leaves masked mode needs no "+
			"key and is not misconfigured for lacking one", got.Status)
	}
}

// TestRunSurvivesWithoutADatabase covers the wiring mistake this split
// made newly possible.
//
// The checks used to be methods on the panel's Store, and a Store always
// had a pool - there was no way to hold one without a database. A
// standalone Checker can be built with nothing, and the place that would
// discover it is the last step of the setup wizard, where a panic is the
// worst possible outcome: the installer is one button from handover and
// gets a blank page instead.
//
// So the answer is a page of honest skips. Handover still cannot
// complete, because skipped required checks block it - which is right.
// Nothing was verified.
func TestRunSurvivesWithoutADatabase(t *testing.T) {
	for _, tc := range []struct {
		name    string
		checker *Checker
	}{
		{"nil checker", nil},
		{"checker with no pool", New(nil, false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			results := tc.checker.Run(context.Background(), Config{})
			if len(results) == 0 {
				t.Fatal("no results at all")
			}

			// Every database check reports skip, and says why.
			var databaseChecks int
			for _, r := range results {
				if !strings.HasPrefix(r.ID, "schema.") && !strings.HasPrefix(r.ID, "grants.") {
					continue
				}
				databaseChecks++
				if r.Status != CheckSkip {
					t.Errorf("%s = %s, want skip: %s", r.ID, r.Status, r.Detail)
				}
				if r.Label == "" {
					t.Errorf("%s came back without a label, so the page would show a blank row", r.ID)
				}
				if !strings.Contains(r.Detail, "incelenmedi") {
					t.Errorf("%s does not say nothing was examined: %q", r.ID, r.Detail)
				}
			}
			if databaseChecks == 0 {
				t.Fatal("no database checks in the results; this test is not checking anything")
			}

			// And handover is blocked, because nothing was verified.
			if ok, _ := Complete(results); ok {
				t.Error("handover would be allowed after a run that never reached a database")
			}
		})
	}
}
