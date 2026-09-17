package preflight

import (
	"context"
	"go/build"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
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
//
// # Why the parameter count is asserted rather than left to the compiler
//
// It used to take a second argument, ipTokenKeyConfigured, and that
// argument is the reason this test changed. A boolean the caller was
// supposed to know is the shape that failed: cmd/panel read it from a
// store field no production code set, so the answer was false on every
// installation and the check below skipped on all of them. What replaced
// it asks the database.
//
// A compile error would catch a third parameter, but it would catch it as
// "too few arguments in call to New" at every call site and say nothing
// about why one argument is the rule. The count is asserted so that the
// next person tempted to pass a fact in gets told where facts come from.
func TestCheckerNeedsOnlyAPool(t *testing.T) {
	signature := reflect.TypeOf(New)
	if got := signature.NumIn(); got != 1 {
		t.Errorf("New takes %d arguments, want 1 (the pool)", got)
	}
	if got := signature.In(0); got != reflect.TypeOf((*pgxpool.Pool)(nil)) {
		t.Errorf("New's argument is %s, want *pgxpool.Pool: a check learns about a "+
			"deployment by asking it, not by being told", got)
	}

	// A nil pool is enough to build one, which is the point - every
	// non-database check runs without touching Postgres at all.
	c := New(nil)
	if c == nil {
		t.Fatal("New returned nil")
	}

	// And a question that could not be asked is never an answer. The old
	// version asserted a *default* here, which is what a passed-in fact
	// has; this one has no default, so what there is to assert is the
	// direction: no database, no pass.
	got := c.checkIPTokenKey(context.Background())
	if got.Status != CheckSkip {
		t.Errorf("status = %s, want skip: with no database nothing was examined, and "+
			"%q is a different fact from a key being absent", got.Status, got.Detail)
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
		{"checker with no pool", New(nil)},
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

// TestCompleteAgreesWithWhatTheStatusesMean.
//
// CheckWarn is defined as "something worth knowing that does not block
// handover", and for a while Complete blocked on it anyway. Nothing
// caught that, because nothing acted on Complete until the wizard grew
// a handover step - at which point a log directory at 0755 made an
// installation impossible to hand over, over a permission bit that the
// check itself calls a warning.
//
// The distinction that does survive is skip: "we looked and it is
// imperfect" and "we could not look" are different facts, and only the
// second is a reason to stop.
func TestCompleteAgreesWithWhatTheStatusesMean(t *testing.T) {
	cases := []struct {
		name     string
		result   CheckResult
		blocking bool
	}{
		{"required and passed", CheckResult{Severity: SeverityRequired, Status: CheckPass}, false},
		{"required and failed", CheckResult{Severity: SeverityRequired, Status: CheckFail}, true},
		{"required but could not run", CheckResult{Severity: SeverityRequired, Status: CheckSkip}, true},
		{"required with a warning", CheckResult{Severity: SeverityRequired, Status: CheckWarn}, false},
		{"recommended and failed", CheckResult{Severity: SeverityRecommended, Status: CheckFail}, false},
		{"recommended and skipped", CheckResult{Severity: SeverityRecommended, Status: CheckSkip}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.result.ID = "x"
			ok, blocking := Complete([]CheckResult{tc.result})
			if tc.blocking && ok {
				t.Errorf("%s did not block handover", tc.name)
			}
			if !tc.blocking && !ok {
				t.Errorf("%s blocked handover: %+v", tc.name, blocking)
			}
		})
	}
}
