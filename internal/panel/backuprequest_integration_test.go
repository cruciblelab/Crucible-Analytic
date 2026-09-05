//go:build integration

// The backup request, against a real database.
//
// The rule this file follows, which this project has had to learn
// twice: when testing a refusal, remove every reason for it except the
// one under test, then check the error's identity. "Something refused"
// is a wish, not a test.

package panel

import (
	"context"
	"errors"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/backup"
	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// backupStore opens a store and takes the queue's lock.
//
// BackupQueueLock, because panel_backup_requests has a partial unique
// index allowing one in-flight row for the whole database: two suites
// asking at once make one of them fail with ErrBackupInFlight, in
// another package, on a different runner.
func backupStore(t *testing.T) (*Store, *devgate.Gate, context.Context) {
	t.Helper()
	store := &Store{pool: testdb.Pool(t, testdb.Panel)}
	testdb.Lock(t, testdb.Pool(t, testdb.SchemaAdmin), testdb.BackupQueueLock)

	// Swept through the panel's own role, which is the one the DELETE
	// policy names - schema_admin answers and does not sweep. And the
	// error is checked: a cleanup that deletes no rows and reports
	// nothing leaves the next test failing with ErrBackupInFlight
	// somewhere unrelated.
	sweep := func() {
		if _, err := store.pool.Exec(context.Background(),
			`DELETE FROM panel_backup_requests`); err != nil {
			t.Errorf("sweeping the backup queue: %v", err)
		}
	}
	sweep()
	t.Cleanup(sweep)
	return store, testGate(t, store), context.Background()
}

// backupOwner is a site owner: entitled to manage settings, and stopped
// by nothing except what is being tested.
func backupOwner() Access {
	return Access{
		Principal: Principal{Kind: PrincipalUser, UserID: 1, Label: "yedek-sahibi"},
		Role:      RoleOwner,
		Member:    true,
	}
}

// The data backup takes no password. That is the decision at the top of
// backuprequest.go, and it is worth a test because the change that
// added a gate for the configuration could have put one in front of
// everything - which would take a feature the customer owns and hand it
// to us.
func TestADataBackupNeedsNoDeveloperPassword(t *testing.T) {
	store, _, ctx := backupStore(t)

	req, err := store.RequestBackup(ctx, backupOwner(), devgate.Authorization{}, "",
		[]string{backup.SetPanel})
	if err != nil {
		t.Fatalf("a data backup was refused without a password: %v", err)
	}
	if req == nil {
		t.Fatal("no row was written")
	}
}

// And the configuration does.
func TestASecretsBackupIsRefusedWithoutTheDeveloperPassword(t *testing.T) {
	store, _, ctx := backupStore(t)

	// A zero Authorization is what any other package can construct, and
	// it authorizes nothing. Nothing else about this call is wrong: the
	// principal is an owner, the set is one this build knows, and the
	// queue is empty.
	_, err := store.RequestBackup(ctx, backupOwner(), devgate.Authorization{}, "",
		[]string{backup.SetSirlar})
	if !errors.Is(err, ErrSecretsPasswordRequired) {
		t.Fatalf("got %v, want ErrSecretsPasswordRequired", err)
	}

	// And nothing was queued, or the refusal would still have consumed
	// the one in-flight slot.
	latest, err := backup.Latest(ctx, store.pool)
	if err != nil {
		t.Fatal(err)
	}
	if latest != nil {
		t.Errorf("a refused request left a row in state %q", latest.State)
	}
}

func TestAValidPasswordQueuesASecretsBackup(t *testing.T) {
	store, gate, ctx := backupStore(t)

	req, err := store.RequestBackup(ctx, backupOwner(),
		authorizeAction(t, gate, SecretsGateAction), "", []string{backup.SetSirlar})
	if err != nil {
		t.Fatalf("an authorised secrets backup was refused: %v", err)
	}
	if req == nil {
		t.Fatal("no row was written")
	}
	if len(req.Sets) != 1 || req.Sets[0] != backup.SetSirlar {
		t.Errorf("the row names %v", req.Sets)
	}
}

// An authorization minted for something else must not open this.
//
// Worth asserting because the failure is silent: an authorization that
// happened to satisfy every action would look exactly like the system
// working, and scoping one to an action is the whole reason it carries
// one.
func TestAnAuthorizationForSomethingElseDoesNotTakeTheConfiguration(t *testing.T) {
	store, gate, ctx := backupStore(t)

	_, err := store.RequestBackup(ctx, backupOwner(),
		authorizeAction(t, gate, ReleaseGateAction), "", []string{backup.SetSirlar})
	if !errors.Is(err, ErrSecretsPasswordRequired) {
		t.Fatalf("an authorization minted for %q was accepted for %q (err=%v)",
			ReleaseGateAction, SecretsGateAction, err)
	}
}

// A viewer holding a correct password must still be refused, and
// refused for lack of entitlement rather than for the password. The
// reverse order would make the developer password a way for anybody
// holding it to act as any account.
func TestAViewerCannotTakeTheConfigurationWithACorrectPassword(t *testing.T) {
	store, gate, ctx := backupStore(t)

	viewer := Access{Principal: Principal{Kind: PrincipalUser, UserID: 1, Label: "izleyici"},
		Role: RoleViewer, Member: true}

	_, err := store.RequestBackup(ctx, viewer,
		authorizeAction(t, gate, SecretsGateAction), "", []string{backup.SetSirlar})
	if !errors.Is(err, ErrSettingNotWritable) {
		t.Fatalf("a viewer holding a valid authorization was refused with %v; want "+
			"ErrSettingNotWritable. Being refused for the password instead would mean "+
			"the password can stand in for a role", err)
	}
}

// The two artifacts cannot be asked for together, and the panel is the
// first of the two places that says so.
func TestThePanelRefusesTheConfigurationAndTheDataTogether(t *testing.T) {
	store, gate, ctx := backupStore(t)

	_, err := store.RequestBackup(ctx, backupOwner(),
		authorizeAction(t, gate, SecretsGateAction), "",
		[]string{backup.SetPanel, backup.SetSirlar})
	if !errors.Is(err, backup.ErrMixedRequest) {
		t.Fatalf("got %v, want ErrMixedRequest. A correct password must not be a way "+
			"to put ip_hash_key in the same file as the traffic", err)
	}
}
