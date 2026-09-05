//go:build integration

// The copy taken before the schema is touched.
//
// A schema upgrade is the one operation in this product a customer
// cannot undo from the page. It runs DDL as the only role allowed to,
// against a database that is theirs, and the honest advice after one
// goes wrong is "restore a backup" - which is only advice if there is
// one.
//
// So the applier takes it, at the last point where stopping is free: the
// request is claimed, the fingerprint is checked, and not one statement
// has run.
package applier

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
	"github.com/cruciblelab/crucible-analytic/internal/upgrade"
)

// heldSchemaVersionLock fails unless this session really holds it.
//
// # Why the assertion rather than a comment
//
// Two tests here move schema_version to a version that is not this
// build's, so "before" and "after" are distinguishable at all. That row
// is one row for the whole database and three suites write it; the two
// times they overlapped, the failure appeared in a different package
// with a message that named neither.
//
// applierAndPanel takes testdb.SchemaVersionLock, so these tests are
// safe - but only because of a line in another file. An invariant reads
// each test file for the lock's name and this one had none, which was a
// fair complaint: the file wrote the row and said nothing about what
// made that allowed.
//
// A comment would have satisfied the reader and the scan and proved
// nothing. This asks the database, so if the fixture ever stops taking
// the lock these tests say so here, rather than some other package
// saying something else somewhere else.
func heldSchemaVersionLock(t *testing.T, a *Applier) {
	t.Helper()
	var held bool
	err := a.Pool.QueryRow(context.Background(), `
		SELECT EXISTS (
		    SELECT 1 FROM pg_locks
		    WHERE locktype = 'advisory'
		      AND granted
		      AND (classid::bigint << 32 | objid::bigint) = $1
		)`, int64(testdb.SchemaVersionLock)).Scan(&held)
	if err != nil {
		t.Fatalf("asking whether the schema_version lock is held: %v", err)
	}
	if !held {
		t.Fatal("this test moves schema_version and nothing holds " +
			"testdb.SchemaVersionLock. That row is one row for the whole " +
			"database and three suites write it; without the lock this does not " +
			"fail here, it makes another package fail somewhere else with a " +
			"message that names neither")
	}
}

// TestTheBackupIsTakenBeforeTheFirstStatement.
//
// # What goes wrong without the ordering
//
// A backup taken after the migration records the state somebody would be
// going back *from*. It is a copy of the problem.
//
// The order is asserted by having the hook read the schema version and
// keep it: at the moment it runs, the database must still be on the old
// one.
func TestTheBackupIsTakenBeforeTheFirstStatement(t *testing.T) {
	a, panelPool := applierAndPanel(t)
	ctx := context.Background()

	var sawFingerprint string
	var sawErr error
	a.BackupFirst = func(ctx context.Context) (*int64, error) {
		state, err := schemaver.Read(ctx, a.Pool)
		sawErr = err
		sawFingerprint = state.Fingerprint
		id := int64(4242)
		return &id, nil
	}

	// Put the database on a version that is not this build's, so "before"
	// and "after" are distinguishable at all.
	heldSchemaVersionLock(t, a)
	if _, err := a.Pool.Exec(ctx,
		`UPDATE schema_version SET version = 1, fingerprint = 'onceki-parmak-izi'`); err != nil {
		t.Fatal(err)
	}

	askFor(t, panelPool, schemaver.Fingerprint)
	applyOnce(t, a)

	if sawErr != nil {
		t.Fatalf("the hook could not read the schema version: %v", sawErr)
	}
	if sawFingerprint != "onceki-parmak-izi" {
		t.Errorf("when the backup ran the database was already on %q.\n"+
			"A copy taken after the migration is a copy of the state somebody "+
			"would be going back from", sawFingerprint)
	}

	// And the upgrade still happened, because a backup that stopped it
	// would be a cure worse than the disease.
	if state, err := schemaver.Read(ctx, a.Pool); err != nil {
		t.Fatal(err)
	} else if !state.Matches() {
		t.Error("the upgrade did not finish after the backup")
	}
}

// TestAFailedBackupStopsTheUpgradeWithTheDatabaseUntouched.
//
// # Why this is a failure and not a warning
//
// A deployment that configured a backup directory has said it wants a
// copy before risky things happen. A backup that fails there is a real
// signal - a full disk, a directory nobody can write - arriving at
// exactly the moment it is most expensive to ignore.
//
// And stopping is free. The claim is taken, the fingerprint is checked,
// and no statement has run, so the database is exactly as it was.
func TestAFailedBackupStopsTheUpgradeWithTheDatabaseUntouched(t *testing.T) {
	a, panelPool := applierAndPanel(t)
	ctx := context.Background()

	before, err := schemaver.Read(ctx, a.Pool)
	if err != nil {
		t.Fatal(err)
	}
	// Put it behind, so an upgrade that ran would be visible.
	heldSchemaVersionLock(t, a)
	if _, err := a.Pool.Exec(ctx,
		`UPDATE schema_version SET version = 1, fingerprint = 'onceki-parmak-izi'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = a.Pool.Exec(context.Background(),
			`UPDATE schema_version SET version = $1, fingerprint = $2`,
			before.Version, before.Fingerprint)
	})

	a.BackupFirst = func(context.Context) (*int64, error) {
		return nil, errors.New("disk dolu")
	}
	a.BackupOptional = func(error) bool { return false }

	askFor(t, panelPool, schemaver.Fingerprint)
	_, runErr := a.RunOnce(ctx)
	if !errors.Is(runErr, ErrBackupFailed) {
		t.Fatalf("RunOnce returned %v, want ErrBackupFailed", runErr)
	}

	// The database is untouched.
	after, err := schemaver.Read(ctx, a.Pool)
	if err != nil {
		t.Fatal(err)
	}
	if after.Fingerprint != "onceki-parmak-izi" {
		t.Errorf("the schema moved to %q after the backup failed. The whole reason "+
			"the copy is taken here is that stopping costs nothing yet",
			after.Fingerprint)
	}

	// And the row says why, because the page is where somebody is
	// waiting and "Sırada" for ever is the answer they must not get.
	latest, err := upgrade.Latest(ctx, panelPool)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != upgrade.StateFailed {
		t.Errorf("the request is in state %q, want failed", latest.State)
	}
	if !strings.Contains(latest.ErrorChain, "disk dolu") {
		t.Errorf("the row does not say what went wrong: %q", latest.ErrorChain)
	}
}

// TestADeploymentThatNeverConfiguredBackupsStillUpgrades.
//
// The other side of the line, and the one that decides whether this
// feature is a safety net or a wall. A deployment with no backup
// directory never had a backup and has not just lost one; refusing to
// upgrade it would be a regression delivered as a precaution.
//
// L2 stops the services while the schema is behind, so "will not
// upgrade" is not a deployment waiting - it is a deployment down.
func TestADeploymentThatNeverConfiguredBackupsStillUpgrades(t *testing.T) {
	a, panelPool := applierAndPanel(t)
	ctx := context.Background()

	notConfigured := errors.New("no directory is configured")
	a.BackupFirst = func(context.Context) (*int64, error) { return nil, notConfigured }
	a.BackupOptional = func(err error) bool { return errors.Is(err, notConfigured) }

	askFor(t, panelPool, schemaver.Fingerprint)
	done := applyOnce(t, a)
	if done == nil {
		t.Fatal("nothing was applied")
	}

	latest, err := upgrade.Latest(ctx, panelPool)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != upgrade.StateSucceeded {
		t.Errorf("state = %q, want succeeded (error_chain: %q).\n"+
			"A deployment that never configured backups must upgrade exactly as "+
			"it did before this existed", latest.State, latest.ErrorChain)
	}
}

// TestAnApplierWithNoHookBehavesAsItAlwaysDid.
//
// Nil is the state every applier was in before F1d and the state a test
// about something else still wants. A hook that was required would make
// this package's other tests depend on a backup they do not care about.
func TestAnApplierWithNoHookBehavesAsItAlwaysDid(t *testing.T) {
	a, panelPool := applierAndPanel(t)

	if a.BackupFirst != nil {
		t.Fatal("the fixture sets a hook; this test is about not having one")
	}
	askFor(t, panelPool, schemaver.Fingerprint)
	if done := applyOnce(t, a); done == nil {
		t.Fatal("nothing was applied")
	}
}

// TestTheRefusedSchemaCostsNoBackup.
//
// An applier that does not carry the requested schema refuses before it
// does anything. Copying the database first would spend minutes and a
// disk on a request that was never going to run - and on the deployment
// most likely to be mid-upgrade, where the disk is already the thing
// under pressure.
func TestTheRefusedSchemaCostsNoBackup(t *testing.T) {
	a, panelPool := applierAndPanel(t)

	called := false
	a.BackupFirst = func(context.Context) (*int64, error) {
		called = true
		return nil, nil
	}

	askFor(t, panelPool, strings.Repeat("f", 64))
	if _, err := a.RunOnce(context.Background()); !errors.Is(err, ErrNotThisBinary) {
		t.Fatalf("RunOnce returned %v, want ErrNotThisBinary", err)
	}
	if called {
		t.Error("a request this binary refuses cost a backup first")
	}
}
