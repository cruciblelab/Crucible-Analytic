package main

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
)

// TestOnlyAnUnconfiguredDirectoryLetsAnUpgradeSkipItsBackup.
//
// # What goes wrong in each direction
//
// Too strict, and a deployment that never configured backups can no
// longer upgrade its schema. L2 stops the services while the schema is
// behind, so that deployment is not waiting - it is down, and the
// feature that put it there was added as a precaution.
//
// Too loose, and the copy is skipped at the one moment it was worth
// taking: a full disk, a directory nobody can write, a database that
// could not be read. Those are the failures a customer needs to hear
// about before the DDL runs, not after.
//
// # Why this test exists at all
//
// The decision was a closure inside main. A mutation flipping it to
// "nothing is optional" changed nothing anywhere in the suite, because
// the only caller is the applier and the applier's tests supply their
// own predicate. The wiring was the untested half of a decision whose
// two failure modes are both bad.
func TestOnlyAnUnconfiguredDirectoryLetsAnUpgradeSkipItsBackup(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want bool
	}{
		{"no directory configured", backup.ErrNotConfigured, true},
		{"and still recognised through a wrapping",
			fmt.Errorf("taking the pre-upgrade copy: %w", backup.ErrNotConfigured), true},
		{"the disk is full", errors.New("this backup needs about 181862 bytes and " +
			"the disk has 49152 available"), false},
		{"the directory cannot be written", errors.New("permission denied"), false},
		{"the database could not be read", errors.New("connection refused"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := backupOptional(c.err); got != c.want {
				t.Errorf("backupOptional = %v, want %v.\n"+
					"true means the schema upgrade goes ahead with no copy behind "+
					"it; false means it stops with the reason on the row",
					got, c.want)
			}
		})
	}
}
