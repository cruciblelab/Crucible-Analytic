package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The destination check, which is the only part of -open that can
// destroy something.
//
// # Why this is not a convenience
//
// -open writes configuration files with live credentials in them. The
// day it is used is the day the machine is gone and somebody is typing
// paths from memory, and `-into /etc/crucible-analytic` on a machine
// that still has one would replace five working files with five from a
// backup - silently, because writing a file over another file is what
// writing a file does.
//
// So the destination has to be empty, and it is checked before the
// password is asked for rather than after: deriving the key costs a
// third of a second and 128 MiB, and being told the destination is
// unusable at the end of that is being told at the wrong end.
func TestPrepareIntoRefusesADirectoryWithSomethingInIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "panel.toml"), []byte("live\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := prepareInto(dir)
	if err == nil {
		t.Fatal("a directory with a file in it was accepted as a restore destination")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("the refusal does not name the directory: %v", err)
	}

	// And it left the file alone. A check that refused after truncating
	// something would pass the assertion above.
	body, err := os.ReadFile(filepath.Join(dir, "panel.toml"))
	if err != nil || string(body) != "live\n" {
		t.Errorf("the existing file was changed: %q %v", body, err)
	}
}

func TestPrepareIntoCreatesTheDirectoryItWasGiven(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "yeni", "derin")

	if err := prepareInto(dir); err != nil {
		t.Fatalf("a new directory was refused: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("nothing was created: %v", err)
	}
	// 0700, because what lands in here is every credential this
	// deployment has. A restore directory readable by other accounts on
	// the machine would undo the file modes the archive carries.
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("the destination is %04o, want 0700", perm)
	}

	// And an existing empty directory at the wrong mode is tightened
	// rather than accepted: MkdirAll's mode applies only to directories
	// it creates, which is the same trap internal/backup found on the
	// backup directory itself.
	loose := t.TempDir()
	if err := os.Chmod(loose, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prepareInto(loose); err != nil {
		t.Fatalf("an empty directory was refused: %v", err)
	}
	info, err = os.Stat(loose)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("an existing directory kept %04o; MkdirAll does not chmod what is "+
			"already there", perm)
	}
}
