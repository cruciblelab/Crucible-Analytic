package botdata

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWritableAgreesWithSave.
//
// # What this is for
//
// Writable exists to answer one question - "would the update be able to
// write here?" - and it answers it by doing what Save does short of
// writing. That is only worth anything while the two stay the same
// mechanism.
//
// A wrong answer here is worse than no answer. Writable is what decides
// which of two messages the collector logs at startup, and both are
// confident: one tells the operator to run a command, the other tells
// them the command will not help. An optimistic Writable restores the
// misdiagnosis this was written to remove; a pessimistic one tells a
// perfectly good deployment that it is broken.
//
// So the check is agreement rather than a list of cases Writable ought
// to get right: for each arrangement, both are run, and either both
// succeed or both fail.
func TestWritableAgreesWithSave(t *testing.T) {
	set := Set{Labels: map[string]string{"t13d1516h2_8daaf6152771_02713d6af862": "test"}, Source: "test"}

	cases := []struct {
		name string
		// path returns the bot data path to try, and is given a fresh
		// temporary directory to build it in.
		path func(t *testing.T, root string) string
		// rootCanDoAnything marks the arrangements that a process
		// running as root walks straight through. They are the
		// permission ones, and they are exactly the arrangements this
		// function exists for - so they are skipped rather than
		// quietly inverted.
		rootCanDoAnything bool
	}{
		{
			name: "an ordinary directory",
			path: func(_ *testing.T, root string) string {
				return filepath.Join(root, "known_bots.json")
			},
		},
		{
			name: "a directory that does not exist yet",
			path: func(_ *testing.T, root string) string {
				return filepath.Join(root, "state", "bots", "known_bots.json")
			},
		},
		{
			name: "the parent is a file, not a directory",
			path: func(t *testing.T, root string) string {
				blocker := filepath.Join(root, "blocker")
				if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
				return filepath.Join(blocker, "known_bots.json")
			},
		},
		{
			name: "a directory this process may not write to",
			path: func(t *testing.T, root string) string {
				locked := filepath.Join(root, "locked")
				if err := os.Mkdir(locked, 0o555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
				return filepath.Join(locked, "known_bots.json")
			},
			rootCanDoAnything: true,
		},
		{
			name: "an unwritable directory above the one that would be created",
			path: func(t *testing.T, root string) string {
				locked := filepath.Join(root, "locked")
				if err := os.Mkdir(locked, 0o555); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
				return filepath.Join(locked, "state", "known_bots.json")
			},
			rootCanDoAnything: true,
		},
		{
			name: "no path at all",
			path: func(_ *testing.T, _ string) string { return "" },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.rootCanDoAnything && os.Geteuid() == 0 {
				t.Skip("running as root, which is permitted everything; this arrangement " +
					"says nothing here. Run the suite as an unprivileged user to see it")
			}

			probeErr := Writable(tc.path(t, t.TempDir()))
			saveErr := Save(tc.path(t, t.TempDir()), set)

			switch {
			case probeErr == nil && saveErr != nil:
				t.Errorf("Writable said yes and Save then failed: %v.\n"+
					"The collector will tell an operator to run the update, the update "+
					"will fail, and the next restart will say the data has never been "+
					"fetched - which is the misdiagnosis Writable exists to remove", saveErr)
			case probeErr != nil && saveErr == nil:
				t.Errorf("Writable said no (%v) and Save succeeded.\n"+
					"The collector will tell a working deployment that its path cannot "+
					"be written and that the update will not help, which is false", probeErr)
			}
		})
	}
}

// TestWritableLeavesNothingBehind.
//
// It runs on every start of a deployment that has not fetched yet, so a
// probe file left in the state directory would be there for good - and
// it would be there under a name nothing else recognises, in the one
// directory an operator looks at to see whether the data arrived.
func TestWritableLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	if err := Writable(filepath.Join(dir, "known_bots.json")); err != nil {
		t.Fatalf("Writable on a temporary directory: %v", err)
	}
	left, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		names := make([]string, 0, len(left))
		for _, e := range left {
			names = append(names, e.Name())
		}
		t.Errorf("Writable left %v behind", names)
	}
}
