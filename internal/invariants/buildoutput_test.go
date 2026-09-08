package invariants

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// What this repository ships is source, and this is the check that keeps
// it source.
//
// # How it was found
//
// While widening the Turkish corruption check from the root documents to
// every tracked file, two files came back "not valid UTF-8":
//
//	analytics-api   15.7 MB, ELF executable, with debug_info, not stripped
//	devpass          5.2 MB, ELF executable, with debug_info, not stripped
//
// Two compiled commands, committed months apart in two unrelated
// commits, 21 MB of a 28 MB repository. Nobody put them there on
// purpose: `go build ./cmd/...` leaves one binary per command in the
// working directory, .gitignore excluded exactly one of the seven
// (`/collector`), and `git add -A` did the rest twice.
//
// # Why it matters more than the megabytes
//
// Every clone pays for them, and every clone gets a *stale* product: a
// build from a commit nobody remembers, sitting at the top of the tree
// under a name that looks like the thing you would run. Neither carried
// a secret - both were checked - but that was luck rather than a
// property: a binary built on a machine with a config beside it is one
// `go:embed` away from carrying it, and a committed binary cannot be
// unshipped from the clones that already have it.
//
// .gitignore's own first sentence is "What this repository distributes is
// source code. Nothing else." That sentence was true of the intent and
// false of the tree.
//
// # Why the rule is UTF-8 rather than a list of names
//
// A list of forbidden filenames catches the two that are already here.
// The rule below catches the fifth one, in a directory nobody has
// created yet, under a name nobody has chosen - a database dump, a
// screenshot, a tarball, a build from a command that does not exist
// today. Everything this project distributes is text; anything that is
// not is either build output or somebody's data, and both are answered
// the same way.
//
// A genuine binary fixture would fail this. That is the intended
// outcome: it goes in the map below with a sentence saying what it is
// and why the repository carries it.

// binaryFilesWithAReason is every tracked file allowed not to be text.
//
// Empty, and that is a statement rather than a placeholder: this
// repository ships no binary at all today. An entry here is a decision,
// and the reason beside it is what makes it reviewable.
var binaryFilesWithAReason = map[string]string{}

// TestNoBuildOutputIsCommitted.
func TestNoBuildOutputIsCommitted(t *testing.T) {
	root := repoRoot(t)

	// The command names, derived. `go build ./cmd/...` writes one file
	// per directory here into whatever directory it is run from, which
	// for anybody working at the top of the tree is the top of the tree.
	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		t.Fatal(err)
	}
	commands := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() {
			commands[e.Name()] = true
		}
	}
	if len(commands) < 5 {
		t.Fatalf("only %d commands found under cmd/; the message below would stop "+
			"naming the cause", len(commands))
	}

	for _, rel := range trackedFiles(t, root) {
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Errorf("could not read %s: %v", rel, err)
			continue
		}
		if utf8.Valid(body) {
			continue
		}
		if why, ok := binaryFilesWithAReason[rel]; ok {
			t.Logf("binary on purpose: %s - %s", rel, why)
			continue
		}
		if commands[rel] {
			t.Errorf("%s is a compiled copy of cmd/%s, committed to the repository "+
				"(%d bytes).\n"+
				"`go build ./cmd/...` writes it here; .gitignore has to exclude it, "+
				"and `git rm --cached %s` takes it out of the tree.\n"+
				"A committed binary is a stale product under a name that looks like "+
				"the real one, and it cannot be taken back out of the clones that "+
				"already have it", rel, rel, len(body), rel)
			continue
		}
		t.Errorf("%s is not text (%d bytes), and this repository distributes source.\n"+
			"If it is build output or somebody's data, it does not belong in the "+
			"tree. If it is a fixture something genuinely needs, put it in "+
			"binaryFilesWithAReason with the sentence that says why", rel, len(body))
	}
}

// TestEveryCommandsBuildOutputIsIgnored.
//
// The other half, and the half that stops this happening again. Removing
// the two that are here leaves the working directory exactly as
// dangerous as it was: `git add -A` after a build offers the other five,
// and the next person accepts them for the same reason the last two were
// accepted - because `git status` was long and the names looked like
// part of the project.
//
// Checked against git's own answer rather than by reading .gitignore, so
// a rule written in a form that does not match is caught as well as a
// rule nobody wrote.
func TestEveryCommandsBuildOutputIsIgnored(t *testing.T) {
	root := repoRoot(t)

	entries, err := os.ReadDir(filepath.Join(root, "cmd"))
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	checked := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		checked++
		if !ignoredByGit(t, root, e.Name()) {
			missing = append(missing, e.Name())
		}
	}
	if checked < 5 {
		t.Fatalf("only %d commands found under cmd/; this test checked almost nothing", checked)
	}
	if len(missing) > 0 {
		t.Errorf("`go build ./cmd/...` leaves these in the working directory and "+
			"git offers to commit them: %s.\n"+
			"Two such binaries were committed this way before anybody noticed. "+
			"Add them to .gitignore beside /collector", strings.Join(missing, ", "))
	}
}

// ignoredByGit asks git, at the repository root, where a build would
// leave the file.
func ignoredByGit(t *testing.T, root, name string) bool {
	t.Helper()
	// check-ignore exits 0 when the path is ignored, 1 when it is not,
	// and something else on a real failure - which must not be read as
	// "ignored".
	out, err := runGit(root, "check-ignore", "-q", "--no-index", name)
	if err == nil {
		return true
	}
	if code, ok := exitCode(err); ok && code == 1 {
		return false
	}
	t.Fatalf("git check-ignore %s: %v\n%s", name, err, out)
	return false
}

// runGit runs one git command at the repository root.
//
// Here rather than in each caller so that "ask git" is one thing this
// package does one way, and so a git that is missing produces one
// recognisable failure instead of three different ones.
func runGit(root string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// exitCode pulls the process's exit status out of an error, for the
// commands that answer with it rather than with output.
func exitCode(err error) (int, bool) {
	var e *exec.ExitError
	if errors.As(err, &e) {
		return e.ExitCode(), true
	}
	return 0, false
}
