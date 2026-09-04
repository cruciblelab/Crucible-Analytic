package invariants

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every queue has something that empties it.
//
// # The defect this exists because of
//
// internal/relupdate had a request queue with Ask, Claim, Finish and
// ExpireStale, all tested. It had a fetcher, an installer, a rollback
// and a panel button. Nothing called Claim.
//
// So pressing the button wrote a row that no process ever read. The
// page said "Sırada" and went on saying it. Not a crash, not a refusal,
// not a log line - waiting, which is the one outcome a customer cannot
// tell apart from "this is slow".
//
// Every test in that package tested one link, and a chain of tested
// links is not a tested chain. What was missing was nobody having
// written the sentence "and then the upgrader does it".
//
// # Why this is derived rather than listed
//
// A list of queues is a list that is correct the day it is written. The
// queues are found by looking for the function that takes work off one,
// so a queue added later is in scope the moment it exists - which is
// the property a list cannot have, and the reason the first one was
// missed.

// claimFunc matches the function that takes a row off a queue.
//
// The name is the convention this project already follows in both
// queues it has. A third queue calling it something else would escape
// this check, which is a real limit and is why the failure message says
// what the convention is rather than only that it was broken.
var claimFunc = regexp.MustCompile(`(?m)^func Claim\(`)

// TestEveryRequestQueueHasSomethingThatEmptiesIt.
func TestEveryRequestQueueHasSomethingThatEmptiesIt(t *testing.T) {
	root := repoRootFromInvariants(t)

	queues := map[string]string{} // package name -> file that defines Claim
	internal := filepath.Join(root, "internal")
	err := filepath.WalkDir(internal, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if claimFunc.Match(body) {
			queues[filepath.Base(filepath.Dir(path))] = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(queues) == 0 {
		t.Fatal("no queue packages found; this test would pass by examining nothing. " +
			"A queue is a package defining `func Claim(` - if that convention has " +
			"changed, this check has to change with it")
	}

	for pkg, definedIn := range queues {
		t.Run(pkg, func(t *testing.T) {
			callers := callersOf(t, root, pkg)
			if len(callers) == 0 {
				t.Errorf("%s defines a queue (%s) and nothing outside its own package "+
					"ever takes work off it.\n"+
					"A queue with no consumer does not fail: it accepts requests and "+
					"leaves them pending, and the page that queued them goes on saying "+
					"\"Sırada\" forever. That is what internal/relupdate did through five "+
					"phases of work, every one of which had passing tests.\n"+
					"Either wire a runner to it, or the queue is dead code and should go",
					pkg, mustRel(root, definedIn))
			}
		})
	}
}

// callersOf finds non-test code outside pkg that calls pkg.Claim.
//
// Outside the package, because a Claim called only from its own tests is
// exactly the state this catches: the function works, has a test, and
// nothing in a running program reaches it.
func callersOf(t *testing.T, root, pkg string) []string {
	t.Helper()
	call := regexp.MustCompile(regexp.QuoteMeta(pkg) + `\.Claim\(`)
	// A runner living inside the queue's own package is legitimate - it
	// is what internal/relupdate now has - but only if something outside
	// calls *it*. So an unqualified Claim( inside the package counts
	// only when the package's runner is itself reached from a command.
	runner := regexp.MustCompile(`\b` + regexp.QuoteMeta(pkg) + `\.(Runner|Applier)\b`)

	var found []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "dist", "node_modules", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if filepath.Base(filepath.Dir(path)) == pkg {
			return nil // the package itself; see runner below
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if call.Match(body) || runner.Match(body) {
			found = append(found, mustRel(root, path))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func mustRel(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}
