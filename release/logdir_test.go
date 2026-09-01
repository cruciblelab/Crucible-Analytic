package release

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// logPath matches a /var/log path this repository names for itself.
var logPath = regexp.MustCompile(`/var/log/crucible[a-z-]*`)

// TestOneLogDirectoryFamily.
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
// # Why a test rather than care
//
// Nothing could have reported this. Every file was internally
// consistent; what was inconsistent was the set of them, and no
// compiler, linter or schema check reads across a shell script, five
// unit files, five TOML examples and a Markdown guide.
//
// So the rule is the only one that holds without a list: whatever this
// repository calls its log directory, it calls it that everywhere.
func TestOneLogDirectoryFamily(t *testing.T) {
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

	spellings := map[string][]string{}
	for _, path := range files {
		body, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		rel, _ := filepath.Rel(root, path)
		// A record of a mistake has to be allowed to contain the mistake.
		// PLAN.md, NOTES.md and CHANGELOG.md quote the failure as it was
		// reported; this file's own comment names both spellings because
		// naming them is what it is for. Four files, listed rather than
		// derived, because "documents the bug" is not a property anything
		// can read off a file - and a list this short is safe only
		// because a wrong entry here fails loudly: the check it exempts
		// is the one that just caught a real defect.
		switch rel {
		case "PLAN.md", "NOTES.md", "CHANGELOG.md", filepath.Join("release", "logdir_test.go"):
			continue
		}
		for _, m := range logPath.FindAllString(string(body), -1) {
			spellings[m] = append(spellings[m], rel)
		}
	}

	if len(spellings) == 0 {
		t.Fatal("no /var/log path found anywhere, so this test is comparing nothing - " +
			"the pattern above has probably stopped matching how they are written")
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
	t.Errorf("this repository spells its log directory %d different ways:%s\n\n"+
		"Every one of these files is consistent with itself; the set of them is not, "+
		"and that is a shape no compiler or linter reads across. It cost the panel "+
		"its ability to start on every installation - see this test's comment",
		len(spellings), b.String())
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
