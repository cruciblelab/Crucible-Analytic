package invariants

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Every periodic loop in this product, and why a cancel landing in the
// middle of its work is safe.
//
// # The defect this list exists for
//
// Two services buffered rows in memory and wrote them on a ticker, and
// both handed the loop's own context to the write. The Run goroutine is
// either in select or inside a COPY, so a cancel that landed during the
// COPY killed it - and those rows had already left the buffer, so
// nothing would send them again. `systemctl stop` lost up to a batch,
// every time, if it landed in the wrong moment.
//
// It was found by a CI flake (run 357 red, 358 green, same commit) in
// internal/beacon, and the neighbour turned out to be identical line for
// line. Two instances of one class, and the second one was worse: the
// collector sees every request, not only the ones that ran JavaScript.
//
// # The property that separates safe from unsafe
//
// Whether the data has another copy.
//
//   - A read (a settings refresh) loses nothing: the cache keeps its
//     last values and the next tick asks again.
//   - A purge or an import inside a transaction loses nothing: it rolls
//     back whole and the next tick redoes it.
//   - A buffered batch has no other copy. Discarding it is the only
//     irreversible one, and those two are the ones that had to change.
//
// So this is a list rather than a scan: "is this the only copy" is not a
// question a syntax tree can answer. What the tree gives is the other
// half - every function that ticks and watches a context - and a new one
// arriving without an entry here fails, which is the point. The rule is
// PLAN.md §3.4's: checked against a list, not against memory.
var periodicLoops = map[string]string{
	"internal/beacon/writer.go:Run": "buffered rows, the only copy - the class this " +
		"list exists for. flush detaches from cancellation and bounds itself; see " +
		"TestAWriteAlreadyGoingOutIsNotAbandonedOnShutdown and the wedged-database " +
		"test beside it",
	"internal/storage/flusher.go:Run": "the same, and worse: Run advances lastFlush " +
		"whether the write succeeded or not and Snapshot selects by lastSeen rather " +
		"than draining, so a killed write is a window nothing retries. flushOnce " +
		"detaches; see TestAFlushAlreadyGoingOutIsNotAbandonedOnShutdown",

	"internal/settings/live.go:Run": "a read. A cancelled refresh keeps the last known " +
		"values - the decided failure mode, see TestSource_KeepsLastKnownValuesWhen" +
		"TheDatabaseGoesAway - and the next tick asks again",
	"internal/heartbeat/heartbeat.go:Run": "liveness, not data: the ctx.Done() branch " +
		"writes nothing at all, and a beat cut short is replaced by the next one. A " +
		"missing heartbeat while the service is stopping is the correct signal",
	"internal/asnlookup/asnlookup.go:Run": "an import inside a transaction. A cancel " +
		"rolls it back whole, leaving the previous ranges intact, and the source is a " +
		"URL that can be fetched again",
	"internal/limiter/limiter.go:throttleWait": "a wait, where cancelling is the " +
		"behaviour being asked for rather than a loss",

	"cmd/collector/main.go:main": "two loops, both safe for reasons above: the " +
		"retention pass is a bounded delete that rolls back and repeats, and the " +
		"settings refresh is a read. The collector's buffered writes are the " +
		"flusher's, listed separately",
	"cmd/beacon/main.go:main": "the settings refresh and the retention pass, same as " +
		"the collector's; the buffered writes are internal/beacon's Run",
	"cmd/upgrader/main.go:main": "polls for a requested upgrade and applies it. The " +
		"apply is one transaction per statement group with its own accounting, and a " +
		"cancel leaves the request row unclaimed for the next pass",
	"cmd/panel/main.go:runHousekeeping": "a purge pass with its own two-minute " +
		"timeout. Idempotent by construction - it deletes rows past a retention " +
		"boundary - so a cancelled pass is redone an hour later",
}

// TestEveryPeriodicWriteLoopIsAccountedFor.
func TestEveryPeriodicWriteLoopIsAccountedFor(t *testing.T) {
	found := periodicLoopsInTree(t)

	if len(found) < 5 {
		t.Fatalf("only %d periodic loops found; this scan is not reading the tree, "+
			"and a scan that reads nothing agrees with any list", len(found))
	}

	for _, key := range found {
		if _, ok := periodicLoops[key]; !ok {
			t.Errorf("%s ticks and watches a context, and nothing says why a cancel "+
				"landing in the middle of its work is safe.\n"+
				"Add an entry to periodicLoops. The question to answer is whether the "+
				"data it is working on has another copy: a read or a transaction loses "+
				"nothing, a buffered batch loses everything and has to detach from "+
				"cancellation the way internal/beacon and internal/storage do.", key)
		}
	}

	live := map[string]bool{}
	for _, key := range found {
		live[key] = true
	}
	for key := range periodicLoops {
		if !live[key] {
			t.Errorf("periodicLoops names %s, which no longer ticks on a context.\n"+
				"Remove the entry - a list with stale rows is a list nobody trusts "+
				"enough to read.", key)
		}
	}
}

// periodicLoopsInTree returns "path:func" for every function that
// creates a ticker and waits on a context's Done channel.
//
// Both conditions, because either alone is common and neither alone is
// this shape: plenty of functions tick without a context (a test
// helper), and plenty select on Done without a ticker (a server). The
// pair is what makes a loop that does work again and again until
// somebody stops it.
func periodicLoopsInTree(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)

	var found []string
	for _, dir := range sourceRoots {
		err := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				if name := info.Name(); name == "testdata" || name == "scratchpad" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				if ticks(fn.Body) && waitsOnDone(fn.Body) {
					found = append(found, filepath.ToSlash(rel)+":"+fn.Name.Name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(found)
	return found
}

// ticks reports whether the body creates a ticker.
func ticks(body *ast.BlockStmt) bool {
	return calls(body, "NewTicker")
}

// waitsOnDone reports whether the body reads a Done channel.
func waitsOnDone(body *ast.BlockStmt) bool {
	return calls(body, "Done")
}

// calls reports whether the body calls a selector with this name, at any
// depth: x.NewTicker(...), ctx.Done().
func calls(body *ast.BlockStmt, name string) bool {
	seen := false
	ast.Inspect(body, func(n ast.Node) bool {
		if seen {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if ok && sel.Sel.Name == name {
			seen = true
			return false
		}
		return true
	})
	return seen
}
