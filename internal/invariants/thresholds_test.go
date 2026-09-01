package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The timing verdicts this repository is allowed to make, and the
// measured gap that makes each one safe.
//
// This list is the "against a list, not against memory" half, the same
// shape as listeningPackages above. The other half is read out of the
// source; a disagreement in either direction fails, naming the side
// that moved.
//
// # Why timing verdicts get a list of their own
//
// A test that fails on a magnitude fails differently from one that
// fails on a value. `len(rows) != 3` is wrong everywhere or nowhere.
// `elapsed > 250ms` is wrong only on machines slower than the one it
// was written on - so it passes review, passes locally, and then goes
// red on a build nobody changed.
//
// It cost this project three red builds in one day, all the same shape:
//
//	the stall floor  "sensitive half's floor is 250ms"   the upgrade took 461ms
//	the race test    "no more than three may give way"   CI gave way seven times
//	the race test    "nobody may wait on a table"        one of twenty-four did
//
// Each number was true of the machine that measured it and of nothing
// else.
//
// # The rule these entries have to satisfy
//
// A timing verdict is safe when the behaviour it permits and the failure
// it catches are **an order of magnitude apart, measured**. Then a
// machine would have to be ten times slower to produce a false red, and
// the threshold can sit anywhere in the gap.
//
// A verdict whose threshold falls between two plausible *correct*
// values is not a threshold - it is a guess about hardware. The three
// above all had margins below 1: the number they forbade was a number
// the correct behaviour actually produced.
//
// Three shapes satisfy the rule without any margin at all, and most
// entries here are one of them:
//
//   - **relative** - compared against another measurement from the same
//     run (`elapsed >= 2*delay`), so both sides scale together.
//   - **self-referential** - compared against a bound the test itself
//     passed in (`elapsed < the deadline given to the call`).
//   - **derived from the system under test** - a fact about PostgreSQL
//     or a configured timeout rather than about this machine.
var timingVerdicts = map[string]string{
	"internal/applier/downtime_integration_test.go": "derived: deadlockDetection is " +
		"PostgreSQL's deadlock_timeout, a property of the database rather than of " +
		"the machine; and judgeStall's floor is max(250ms, how long the upgrade " +
		"actually took), which is the fix for the first of the three red builds above",
	"internal/botdata/live_test.go": "an hour, against data freshly fetched over the " +
		"network in the same test - about the age of a file, not the speed of anything",
	"internal/devgate/devgate_test.go": "measured: fifteen empty submissions must not " +
		"reach argon2. Correct is an early return costing microseconds; the failure " +
		"is fifteen real hashes, the better part of a second. The 200ms ceiling sits " +
		"in a gap of three orders of magnitude",
	"internal/fullproxy/slowloris_test.go": "a 5s ceiling on a connection the server " +
		"is supposed to drop on its own deadline - the failure it catches is a " +
		"connection held open forever, so the gap is unbounded",
	"internal/heartbeat/heartbeat_integration_test.go": "against an uptime the test " +
		"seeded itself ninety minutes ago; the comparison is with its own fixture",
	"internal/limiter/limiter_test.go": "a 1s ceiling on giving up after the context " +
		"expired, which correct code does immediately. The failure it catches is " +
		"waiting for a slot that will never free - again unbounded",
	"internal/logsink/logsink_integration_test.go": "measured both ways: dropping " +
		"takes ~3ms and blocking took 1.88s. The 500ms ceiling is 150x the correct " +
		"cost. It was 2s once, which sat *above* the failure - so a faster database " +
		"would have slipped a blocking sink under it",
	"internal/panel/analytics/client_test.go": "relative: one against 2*delay from " +
		"the same run, one against the client's own configured RequestTimeout",
	"internal/panel/rangefetches_integration_test.go": "twenty-nine days, against a " +
		"row the test inserted with a chosen timestamp - the age of a fixture",
	"internal/proxy/sniff_test.go": "self-referential: the floor is the deadline the " +
		"test passed into sniffClientHello, and the ceiling is ten times it",
}

// timingWords are the ways a duration shows up in a comparison here.
var timingWords = []string{
	"time.", "elapsed", "Elapsed", "took", "Took", "duration", "Duration",
	"waited", "Since", "latency", "Latency", "deadline", "Deadline",
}

// TestEveryTimingVerdictIsAccountedFor.
//
// Reads every `if <comparison> { ... t.Error/t.Fatal ... }` out of the
// test files, keeps the ones whose comparison mentions a duration, and
// requires the file to appear in timingVerdicts above with the reason it
// is safe.
//
// Files rather than lines, deliberately. A line number is the one thing
// about a test that changes for no reason at all, and a mirror that has
// to be edited every time somebody adds a paragraph is a mirror people
// learn to update without reading. What matters is that somebody looked
// at this file's timing verdicts and wrote down why they survive a
// slower machine.
func TestEveryTimingVerdictIsAccountedFor(t *testing.T) {
	root := repoRoot(t)
	found := map[string]bool{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "dist", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			// A file that does not parse is somebody else's failure; this
			// test must not be the one that reports it.
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			ifs, ok := n.(*ast.IfStmt)
			if !ok || ifs.Cond == nil {
				return true
			}
			cmp, ok := ifs.Cond.(*ast.BinaryExpr)
			if !ok {
				return true
			}
			switch cmp.Op {
			case token.GTR, token.LSS, token.GEQ, token.LEQ:
			default:
				return true
			}
			if !mentionsDuration(cmp) || !failsTheTest(ifs.Body) {
				return true
			}
			found[rel] = true
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}

	if len(found) == 0 {
		t.Fatal("no timing verdict found anywhere, so this test is comparing nothing " +
			"against nothing - the scan above has probably stopped recognising them")
	}

	for file := range found {
		if _, ok := timingVerdicts[file]; !ok {
			t.Errorf("%s decides a test's outcome on how long something took, and no "+
				"entry says why that survives a slower machine.\n"+
				"Add one to timingVerdicts with the measured gap between the behaviour "+
				"it permits and the failure it catches - or make the comparison "+
				"relative to another measurement from the same run, which needs no "+
				"gap at all", file)
		}
	}
	for file := range timingVerdicts {
		if !found[file] {
			t.Errorf("timingVerdicts still explains %s, which no longer makes a timing "+
				"verdict.\nRemove the entry: an explanation for a check that is gone "+
				"is an explanation nobody can check", file)
		}
	}
}

func mentionsDuration(e ast.Expr) bool {
	text := exprText(e)
	for _, w := range timingWords {
		if strings.Contains(text, w) {
			return true
		}
	}
	return false
}

func failsTheTest(b *ast.BlockStmt) bool {
	failed := false
	ast.Inspect(b, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch sel.Sel.Name {
		case "Error", "Errorf", "Fatal", "Fatalf":
			failed = true
		}
		return true
	})
	return failed
}

// exprText renders enough of an expression to look for a duration in.
func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.BasicLit:
		return v.Value
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	case *ast.CallExpr:
		parts := []string{exprText(v.Fun)}
		for _, a := range v.Args {
			parts = append(parts, exprText(a))
		}
		return strings.Join(parts, " ")
	case *ast.BinaryExpr:
		return exprText(v.X) + " " + exprText(v.Y)
	case *ast.ParenExpr:
		return exprText(v.X)
	case *ast.UnaryExpr:
		return exprText(v.X)
	case *ast.IndexExpr:
		return exprText(v.X)
	}
	return ""
}
