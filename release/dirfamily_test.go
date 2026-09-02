package release

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// directoryFamilies are the machine directories this repository names
// for itself, each of which must have exactly one spelling.
//
// A table rather than a second copy of this test. The log directory came
// first; the state directory turned out to be the identical defect one
// directory over, and writing it out again would have meant two walks,
// two message templates and two places to fix the next time. A third
// family is now a line.
var directoryFamilies = []struct {
	what    string
	pattern *regexp.Regexp
	// cost is what the disagreement actually did, for the failure
	// message. A test that says "these differ" invites somebody to make
	// them differ deliberately; one that says what it broke does not.
	cost string
}{
	{
		what:    "log directory",
		pattern: regexp.MustCompile(`/var/log/crucible[a-z-]*`),
		cost: "It cost the panel its ability to start on every installation: " +
			"logging.Setup returns an error rather than falling back to stderr, so " +
			"the service that shipped an uncommented dir was the only one that tried " +
			"to open a log tree and the only one that could not",
	},
	{
		what:    "state directory",
		pattern: regexp.MustCompile(`/var/lib/crucible[a-z-]*`),
		cost: "It cost every systemd installation its known-bot signal, silently: " +
			"the collector unit runs under ProtectSystem=strict and names only one " +
			"spelling in ReadWritePaths, so bot data written to the other one could " +
			"never land - and the startup line said it had never been fetched, which " +
			"sent the operator to a command that could not work",
	},
}

// TestOneNamePerDirectoryFamily.
//
// # What went wrong
//
// Two spellings lived side by side for months. The installer's LOG_DIR
// default and all five systemd units said /var/log/crucible-analytic;
// panel.example.toml said /var/log/crucible, and it was the only example
// shipping an *uncommented* dir - so the panel was the only service that
// tried to open a log tree at startup, and the only one that could not.
//
// logging.Setup returns an error rather than falling back to stderr, so
// the effect was total: the nightly reported
//
//	panel: logging setup failed: mkdir /var/log/crucible: permission denied
//
// with collector, beacon and analytics-api all up and the panel absent.
// The systemd path fails the same way for a different reason -
// ProtectSystem=strict with ReadWritePaths naming only the other
// spelling - so this was never about --no-systemd. It was about a name.
//
// The instructions were wrong too, which is the part that would have
// outlived the code fix: KURULUM.md told an operator to create one
// directory and the panel's own preflight told them to create the same
// wrong one, in a command they were meant to paste.
//
// # And then it happened again
//
// The state directory was the identical split, one directory over, and
// it had been there the whole time this test was watching the log one:
// install.sh's STATE_DIR and the collector unit said
// /var/lib/crucible-analytic, everything else said the short form -
// including config.example.toml's *uncommented* bot_data path.
//
// It was quieter and therefore worse. The panel died and said why; bot
// data is a cache, so the collector logged a line and carried on, and
// the line said "has never been fetched" when the truth was "cannot be
// written here". Every systemd installation ran with half of the D3
// crossover silently off, and the advice on screen was to run a command
// that could not work.
//
// That is why this test is now a table. The first family was written as
// if it were the only one.
//
// # Why a test rather than care
//
// Nothing could have reported either of them. Every file was internally
// consistent; what was inconsistent was the set of them, and no
// compiler, linter or schema check reads across a shell script, five
// unit files, five TOML examples, a Dockerfile, a compose file and a
// Markdown guide.
//
// So the rule is the only one that holds without a list: whatever this
// repository calls one of its directories, it calls it that everywhere.
func TestOneNamePerDirectoryFamily(t *testing.T) {
	root := repoRootFromWD(t)

	// Derived by walking, not listed. A sixth service's unit file or a
	// new guide is covered the day it is written.
	var files []string
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
		switch filepath.Ext(path) {
		case ".toml", ".sh", ".service", ".md", ".go", ".yml", ".yaml":
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}

	// Read once, matched for every family. The walk is the expensive
	// half and it does not depend on which directory is being asked
	// about.
	bodies := map[string]string{}
	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(root, path)
		// A record of a mistake has to be allowed to contain the mistake.
		// PLAN.md, NOTES.md and CHANGELOG.md quote the failure as it was
		// reported; this file's own comment names the discarded spellings
		// because naming them is what it is for. Four files, listed
		// rather than derived, because "documents the bug" is not a
		// property anything can read off a file - and a list this short
		// is safe only because a wrong entry here fails loudly: the
		// check it exempts is the one that just caught a real defect.
		switch rel {
		case "PLAN.md", "NOTES.md", "CHANGELOG.md", filepath.Join("release", "dirfamily_test.go"):
			continue
		}
		bodies[rel] = string(body)
	}

	for _, family := range directoryFamilies {
		t.Run(family.what, func(t *testing.T) {
			spellings := map[string][]string{}
			for rel, body := range bodies {
				for _, m := range family.pattern.FindAllString(body, -1) {
					spellings[m] = append(spellings[m], rel)
				}
			}

			if len(spellings) == 0 {
				t.Fatalf("no %s path found anywhere, so this is comparing nothing - "+
					"%s has probably stopped matching how they are written",
					family.what, family.pattern)
			}
			if len(spellings) == 1 {
				return
			}

			var names []string
			for name := range spellings {
				names = append(names, name)
			}
			sort.Strings(names)

			var b strings.Builder
			for _, name := range names {
				where := spellings[name]
				sort.Strings(where)
				b.WriteString("\n\t" + name + "\n\t\t" + strings.Join(dedupe(where), "\n\t\t"))
			}
			t.Errorf("this repository spells its %s %d different ways:%s\n\n"+
				"Every one of these files is consistent with itself; the set of them is "+
				"not, and that is a shape no compiler or linter reads across. %s.",
				family.what, len(spellings), b.String(), family.cost)
		})
	}
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
