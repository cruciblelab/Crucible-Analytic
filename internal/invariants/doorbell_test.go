package invariants

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/relupdate"
)

// One path, named in four languages.
//
// # What this holds together
//
// The restart channel is a file at /run/crucible-analytic/restart-please.
// Four files have to agree on it, and no two of them are in the same
// language:
//
//	internal/relupdate/restart.go        Go constants, the upgrader's end
//	release/systemd/crucible-restart.path  PathModified=, what systemd watches
//	release/restart.sh                   the rm that clears it
//	release/tmpfiles/crucible-analytic.conf  what creates the directory
//
// Nothing connects them. A typo in any one of them produces a system
// where every part is individually correct and the feature does not
// exist: the upgrader writes a file nobody watches, or systemd watches
// a file nobody writes, or the directory is created somewhere else and
// Configured() reports that no restarter is installed.
//
// None of those fail. The upgrader logs "no restarter is configured",
// which is a true sentence about a deployment that opted in, and the
// operator has no reason to disbelieve it.
//
// # Why it greps rather than listing the four files
//
// A list of four files is correct until the fifth is written - a
// document, a container file, a second script. The rule is about the
// string, so the check is about the string: every mention of a
// /run path belonging to this product, anywhere in the tree, has to be
// this one.
//
// *İki listenin birbirini tutması gerekiyorsa, biri fazladır.*

// runPath matches a /run path that looks like it belongs to this
// product. Deliberately loose on the tail: catching
// /run/crucible-analytics (plural, the obvious typo) is the point.
var runPath = regexp.MustCompile(`/run/crucible[A-Za-z0-9_.-]*`)

func TestEveryFileThatNamesTheDoorbellNamesTheSameOne(t *testing.T) {
	root := repoRootFromInvariants(t)

	// Where the directory is checked, matched against the constant the
	// Go code actually uses rather than a copy written here.
	want := relupdate.DefaultDoorbellDir

	found := map[string][]string{} // path spelled -> files that spell it
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "dist", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		// This file names the wrong spellings on purpose.
		if filepath.Base(path) == "doorbell_test.go" {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		for _, m := range runPath.FindAllString(string(body), -1) {
			// Most of these mentions are in prose, and a sentence ends
			// with a full stop. Without this the check reports
			// "/run/crucible-analytic." as a fifth spelling of the path,
			// which is a true statement about the regexp and a useless
			// one about the system. No path here ends in a dot.
			m = strings.TrimRight(m, ".")
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				rel = path
			}
			rel = filepath.ToSlash(rel)
			if !contains(found[m], rel) {
				found[m] = append(found[m], rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(found) == 0 {
		t.Fatalf("no file in the tree mentions a %s path, so this check examined "+
			"nothing. The restart channel is defined by that path being written in "+
			"several languages at once; if it has moved, this check has to move with it",
			"/run/crucible...")
	}

	for spelled, files := range found {
		// The directory itself, or the request file inside it. Anything
		// else under this prefix is a path nobody decided on.
		if spelled == want || spelled == want+"/"+relupdate.DoorbellName {
			continue
		}
		t.Errorf("%s is spelled in %s, and the upgrader uses %s.\n"+
			"Every part of the restart channel would still be individually correct: "+
			"the unit watches a file, the script deletes a file, the upgrader creates "+
			"a file - just not the same one. Nothing fails. The upgrader reports that "+
			"no restarter is configured, on a machine where somebody installed one",
			spelled, strings.Join(files, ", "), want)
	}
}
