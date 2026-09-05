//go:build release

package release

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The install script's directory modes, and the call that had no work to
// do and ended the install anyway.
//
// # What happened
//
// install.sh ran `chmod 0755 ${PREFIX} ${PREFIX}/bin` before copying the
// binaries. chmod fails with EPERM for anybody who does not own the
// directory - even when the mode is already exactly what was asked for -
// and `set -e` then ends the install.
//
// That is the ordinary case in a container. The image bakes
// /opt/crucible-analytic in, root-owned and already 0755, and the init
// container runs as somebody else. The nightly container run failed two
// nights in a row on a call that would have changed nothing:
//
//	init-1 | == binaries
//	init-1 | chmod: /opt/crucible-analytic: Operation not permitted
//	init-1 | chmod: /opt/crucible-analytic/bin: Operation not permitted
//
// # Why this test does not need a second user
//
// Reproducing "somebody else owns it" needs two accounts, and CI has one
// it can use. What the defect is actually about is narrower: a chmod
// that is called when there is nothing to change. So chmod is replaced
// on PATH by a stub that always fails, which makes any call to it
// visible - and turns the question into one bash can answer alone.

// helperFrom lifts one shell function out of install.sh.
//
// By name and by shape, which is the point: rename the function or
// change how it is written and this stops finding it, which is a
// failure that names the thing that moved rather than a silent pass.
func helperFrom(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(repoRoot(t), "release", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?ms)^` + name + `\(\) \{.*?^\}`)
	found := re.FindString(string(body))
	if found == "" {
		t.Fatalf("release/install.sh has no %s() function.\n"+
			"It is what stops the install dying on a chmod that had nothing to "+
			"change; if it was renamed, rename it here too", name)
	}
	return found
}

// runEnsureMode calls ensure_mode with chmod guaranteed to fail.
func runEnsureMode(t *testing.T, want string, dir string) (string, error) {
	t.Helper()

	work := t.TempDir()
	// A chmod that always refuses, first on PATH. Anything that reaches
	// it fails the way the container did.
	stub := filepath.Join(work, "chmod")
	script := "#!/bin/sh\necho \"chmod: $2: Operation not permitted\" >&2\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	probe := filepath.Join(work, "probe.sh")
	body := "die() { printf 'install: %s\\n' \"$*\" >&2; exit 1; }\n" +
		helperFrom(t, "ensure_mode") + "\nensure_mode " + want + " \"$1\"\n"
	if err := os.WriteFile(probe, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash", probe, dir)
	cmd.Env = append(os.Environ(), "PATH="+work+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestADirectoryAlreadyAtTheRightModeIsNotChmodded.
func TestADirectoryAlreadyAtTheRightModeIsNotChmodded(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	out, err := runEnsureMode(t, "0755", dir)
	if err != nil {
		t.Fatalf("a directory that is already 0755 ended the install: %v\n%s\n"+
			"chmod was called when there was nothing to change, which is the "+
			"failure that stopped every container install", err, out)
	}
	if strings.Contains(out, "Operation not permitted") {
		t.Errorf("chmod was called anyway:\n%s", out)
	}
}

// TestADirectoryAtTheWrongModeThatCannotBeFixedStopsTheInstall.
//
// The other half, and the reason the first half is not simply "never
// chmod". A prefix the services cannot traverse produces a deployment
// where nothing starts, and the message for that has to arrive here
// rather than later as a unit failing with status=203/EXEC.
func TestADirectoryAtTheWrongModeThatCannotBeFixedStopsTheInstall(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	out, err := runEnsureMode(t, "0755", dir)
	if err == nil {
		t.Fatalf("a prefix at 0700 that could not be changed was accepted:\n%s", out)
	}
	for _, want := range []string{dir, "700", "0755"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not mention %q, so nobody reading it knows "+
				"what to do:\n%s", want, out)
		}
	}
}
