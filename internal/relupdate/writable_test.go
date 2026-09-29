package relupdate

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// readOnlyDir makes dir refuse new files to this process, and checks
// that it did.
//
// chmod is enough for anybody but root, which ignores the mode. For
// root the immutable attribute does the same job - and root is who runs
// this suite in a development container. Checked afterwards rather than
// assumed: a test of "cannot write" that could write would pass by
// testing the other branch.
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() != 0 {
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	} else {
		if out, err := exec.Command("chattr", "+i", dir).CombinedOutput(); err != nil {
			t.Fatalf("making %s immutable, which is how root is refused a write: %v\n%s", dir, err, out)
		}
		t.Cleanup(func() { _ = exec.Command("chattr", "-i", dir).Run() })
	}
	if f, err := os.CreateTemp(dir, "premise-"); err == nil {
		name := f.Name()
		f.Close()
		_ = os.Remove(name)
		t.Fatalf("%s still takes new files; this test's premise does not hold", dir)
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestTheInstallerAsksWhetherItMayWriteByWriting.
//
// Three answers, because a systemd install can give each of them and
// they need different sentences: yes; no, because the operator has not
// opened the directory (the ordinary state, measured on 2026-09-29 as
// "read-only file system" on a real systemd); and no, because there is
// no such directory - which means the prefix is wrong.
func TestTheInstallerAsksWhetherItMayWriteByWriting(t *testing.T) {
	t.Run("a directory it may write", func(t *testing.T) {
		prefix := anInstallation(t, "eski", "panel", "collector")
		before := dirEntries(t, filepath.Join(prefix, "bin"))
		if err := (Installer{Prefix: prefix}).Writable(); err != nil {
			t.Fatalf("a writable binary directory was refused: %v", err)
		}
		// The probe removes itself. Left behind, one would accumulate per
		// request - in the directory every service runs from.
		after := dirEntries(t, filepath.Join(prefix, "bin"))
		if strings.Join(before, ",") != strings.Join(after, ",") {
			t.Errorf("the probe left something behind: before %v, after %v", before, after)
		}
	})

	t.Run("a directory it may not write", func(t *testing.T) {
		prefix := anInstallation(t, "eski", "panel")
		bin := filepath.Join(prefix, "bin")
		readOnlyDir(t, bin)
		err := (Installer{Prefix: prefix}).Writable()
		if !errors.Is(err, ErrNotEnabledHere) {
			t.Fatalf("a binary directory this process cannot write was reported as %v; "+
				"want ErrNotEnabledHere", err)
		}
		if errors.Is(err, ErrNoBinaryDirectory) {
			t.Error("a directory that exists was reported as missing")
		}
		// The sentence has to send somebody to the step, because the page
		// shows it verbatim to whoever pressed the button.
		for _, want := range []string{bin, "KURULUM.md", "13.5"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not say %q: %v", want, err)
			}
		}
	})

	t.Run("no directory at all", func(t *testing.T) {
		prefix := t.TempDir()
		err := (Installer{Prefix: prefix}).Writable()
		if !errors.Is(err, ErrNoBinaryDirectory) {
			t.Fatalf("a prefix with no bin directory was reported as %v; want ErrNoBinaryDirectory", err)
		}
		if errors.Is(err, ErrNotEnabledHere) {
			t.Error("a missing directory was reported as a step the operator has not taken; " +
				"opening a directory that is not there fixes nothing")
		}
		if !strings.Contains(err.Error(), "prefix") {
			t.Errorf("the refusal does not point at the prefix: %v", err)
		}
		// And it created nothing: Install makes the directory when it is
		// absent, and that is how a wrong prefix used to succeed.
		if _, statErr := os.Stat(filepath.Join(prefix, "bin")); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("asking created the directory (%v)", statErr)
		}
	})

	t.Run("a file where the directory should be", func(t *testing.T) {
		prefix := t.TempDir()
		if err := os.WriteFile(filepath.Join(prefix, "bin"), []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := (Installer{Prefix: prefix}).Writable(); !errors.Is(err, ErrNoBinaryDirectory) {
			t.Fatalf("a file named bin was reported as %v; want ErrNoBinaryDirectory", err)
		}
	})
}

// TestARingThatCannotBeWrittenSaysWhereTheUnitHasToAllowIt.
//
// The one way the doorbell has been measured to fail is the upgrader's
// own unit mounting /run read-only (NOTES, "V systemd altında", S4). A
// machine that took the new binaries through the panel and still has the
// old unit file fails exactly here, and "read-only file system" alone
// does not say which file to change.
func TestARingThatCannotBeWrittenSaysWhereTheUnitHasToAllowIt(t *testing.T) {
	dir := t.TempDir()
	// A directory where the doorbell file goes: opening it for writing
	// fails for any user, root included.
	if err := os.Mkdir(filepath.Join(dir, DoorbellName), 0o755); err != nil {
		t.Fatal(err)
	}
	err := Doorbell{Dir: dir}.Ring()
	if err == nil {
		t.Fatal("a doorbell that could not be written was reported as rung")
	}
	for _, want := range []string{"ReadWritePaths", dir, "install.sh"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not say %q: %v", want, err)
		}
	}
}
