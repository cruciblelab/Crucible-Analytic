//go:build integration

// The upgrade button's rules, against a real database.
//
// The lock is the whole subject. Every test here is about who may press
// the button in which state, and each connects as the role it is talking
// about rather than as a superuser - a fixture more privileged than
// production would satisfy every branch and prove none of them.
package panel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/schemaver"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
	"github.com/cruciblelab/crucible-analytic/internal/upgrade"
)

// upgradeStore is a Store on the panel's own role, with the queue and
// the lock reset around each test.
func upgradeStore(t *testing.T) *Store {
	t.Helper()
	store := &Store{pool: testdb.Pool(t, testdb.Panel)}
	testdb.Lock(t, testdb.Pool(t, testdb.SchemaAdmin), testdb.UpgradeQueueLock)

	// And the schema_version row, because setInstalledVersion below
	// writes it - and that row is a single row for the whole database.
	//
	// # How this was missed, and how it surfaced
	//
	// Three suites write it: this one, internal/panel/web (which puts it
	// into four states to see what the health page says) and
	// internal/applier (which records what it applied). Two of them took
	// this lock and this one did not, so the pair could interleave.
	//
	// It stayed invisible while the overlap was narrow. It became a CI
	// failure when internal/panel/web grew: the V2 and V4 tests pushed
	// that package from about ninety seconds to a hundred and twelve,
	// which widened the window until the two packages were running at
	// the same time for the whole of this suite. The same commit passed
	// on one runner and failed on another, which is the signature.
	//
	// The failure named neither package:
	//
	//	--- FAIL: TestTheHealthPageReportsTheSchemaVersion/uyuşuyor
	//	    the page says "satırları kaybeder", which belongs to another state
	//
	// It did belong to another state - one this suite had set.
	//
	// # Ordering
	//
	// UpgradeQueueLock first, then this one, matching internal/applier.
	// Two suites taking the same pair in opposite orders deadlock, and a
	// deadlocked suite looks like a hung machine rather than like a bug.
	testdb.Lock(t, testdb.Pool(t, testdb.SchemaAdmin), testdb.SchemaVersionLock)

	clean := func() {
		admin := testdb.Admin(t)
		if _, err := admin.Exec(context.Background(),
			`DELETE FROM panel_upgrade_requests`); err != nil {
			t.Fatalf("clearing the queue: %v", err)
		}
		// The lock is deployment-wide, so a test that left it on would
		// change the answer for every test after it - and for three
		// packages away, which is how the settings pollution in D4a was
		// found.
		if _, err := admin.Exec(context.Background(),
			`DELETE FROM panel_settings WHERE key = $1`, string(KeyUpgradeLocked)); err != nil {
			t.Fatalf("clearing the lock: %v", err)
		}
	}
	clean()
	t.Cleanup(clean)
	return store
}

// setInstalledVersion decides whether this database is behind the build,
// and puts it back afterwards.
//
// Every test below needs one answer or the other, and the first version
// of this file read whichever the development database happened to
// carry and skipped when it did not suit. That is the failure this
// project keeps finding: on a database that was already up to date,
// six of these seven would have skipped and the suite would have gone
// green having proved nothing.
//
// So the precondition is set rather than sampled. Written through the
// owner because schema_version is the applier's table and panel_user
// holds only SELECT - which is itself the property L1 established.
func setInstalledVersion(t *testing.T, behind bool) {
	t.Helper()
	admin := testdb.Admin(t)
	ctx := context.Background()

	var before struct {
		version     int
		fingerprint string
		recorded    bool
	}
	err := admin.QueryRow(ctx,
		`SELECT version, fingerprint FROM schema_version WHERE id = 1`).
		Scan(&before.version, &before.fingerprint)
	before.recorded = err == nil

	version, fingerprint := schemaver.Version, schemaver.Fingerprint
	if behind {
		version, fingerprint = schemaver.Version-1, "eski-parmak-izi"
	}
	if _, err := admin.Exec(ctx, `
		INSERT INTO schema_version (id, version, fingerprint, applied_by)
		VALUES (1, $1, $2, 'test')
		ON CONFLICT (id) DO UPDATE
		SET version = EXCLUDED.version, fingerprint = EXCLUDED.fingerprint`,
		version, fingerprint); err != nil {
		t.Fatalf("setting the installed version: %v", err)
	}

	t.Cleanup(func() {
		if !before.recorded {
			_, _ = admin.Exec(context.Background(), `DELETE FROM schema_version WHERE id = 1`)
			return
		}
		if _, err := admin.Exec(context.Background(), `
			UPDATE schema_version SET version = $1, fingerprint = $2 WHERE id = 1`,
			before.version, before.fingerprint); err != nil {
			t.Logf("restoring the installed version: %v", err)
		}
	})
}

// setLock turns the lock on through the owner's own path, which is the
// only way it can be turned on in production.
func setLock(t *testing.T, store *Store, gate *devgate.Gate, on bool) {
	t.Helper()
	if err := store.ApplySetting(context.Background(), operatorAccess(),
		KeyUpgradeLocked, "", on, authorize(t, gate, KeyUpgradeLocked), nil); err != nil {
		t.Fatalf("setting the lock to %v: %v", on, err)
	}
}

// TestUnlockedTheCustomerCanPressItWithNoPassword.
//
// The requirement in the user's own words: *işi bilmeyen normal müşteri
// de yapabilmeli.* Default off, one click, nothing typed.
//
// The positive case first, because a suite of refusals proves a button
// refuses and never proves it works.
func TestUnlockedTheCustomerCanPressItWithNoPassword(t *testing.T) {
	store := upgradeStore(t)
	ctx := context.Background()
	setInstalledVersion(t, true)

	customer := Access{
		Principal: Principal{Kind: PrincipalUser, UserID: 1, Label: "musteri@example.invalid"},
		Role:      RoleOwner, Member: true,
	}

	status, err := store.UpgradeStatus(ctx, customer)
	if err != nil {
		t.Fatalf("reading the status: %v", err)
	}
	if status.Locked {
		t.Fatal("the lock is on by default; the customer cannot upgrade without the developer")
	}
	if !status.Allowed {
		t.Error("an owner is not allowed to press an unlocked button")
	}
	if !status.Needed {
		t.Fatal("the precondition was set to behind and the status disagrees")
	}

	req, err := store.RequestUpgrade(ctx, customer, devgate.Authorization{}, "op-test")
	if err != nil {
		t.Fatalf("an owner could not request an upgrade with the lock off: %v", err)
	}
	if req.State != upgrade.StatePending {
		t.Errorf("the request is %q, want pending", req.State)
	}
	if req.Actor.Label != "musteri@example.invalid" {
		t.Errorf("the request records %q as the actor", req.Actor.Label)
	}
}

// TestLockedNoCapabilityIsEnough.
//
// The reason the lock is a password and not a role. RoleOwner holds
// CapManageMembers, so a customer can make themselves anything they
// like; if the guard were a capability they would simply grant it.
//
// A superadmin is used deliberately - the most capable principal this
// system has - so the assertion is not "a viewer is refused" but "being
// as powerful as it is possible to be is still not enough".
func TestLockedNoCapabilityIsEnough(t *testing.T) {
	store := upgradeStore(t)
	gate := testGate(t, store)
	ctx := context.Background()

	setLock(t, store, gate, true)

	_, err := store.RequestUpgrade(ctx, operatorAccess(), devgate.Authorization{}, "op-test")
	if !errors.Is(err, ErrUpgradeLocked) {
		t.Fatalf("a superadmin started a locked upgrade with no password (err = %v).\n"+
			"The customer can grant themselves any capability, so a capability cannot "+
			"be what holds this door.", err)
	}

	// And the status says so, rather than making the page guess.
	status, err := store.UpgradeStatus(ctx, operatorAccess())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Locked || status.Allowed {
		t.Errorf("status = {Locked:%v Allowed:%v}; the page cannot tell the customer "+
			"why the button will not work", status.Locked, status.Allowed)
	}
}

// TestLockedTheDeveloperPasswordOpensIt.
//
// The other half, and the one that makes the lock a lock rather than a
// wall. Without this the honest description of the feature would be
// "the developer can permanently disable upgrades", which is not what
// was asked for.
func TestLockedTheDeveloperPasswordOpensIt(t *testing.T) {
	store := upgradeStore(t)
	gate := testGate(t, store)
	ctx := context.Background()
	setInstalledVersion(t, true)

	setLock(t, store, gate, true)

	status, err := store.UpgradeStatus(ctx, operatorAccess())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Needed {
		t.Fatal("the precondition was set to behind and the status disagrees")
	}

	auth := authorizeAction(t, gate, UpgradeGateAction)
	if _, err := store.RequestUpgrade(ctx, operatorAccess(), auth, "op-test"); err != nil {
		t.Fatalf("the developer password did not open a locked upgrade: %v", err)
	}
}

// TestAPasswordForSomethingElseDoesNotOpenIt.
//
// The developer password authorizes one named action at a time. A
// password typed to change the privacy mode must not also apply a schema
// migration - otherwise every guarded setting becomes a way to reach
// every other guarded thing, and the gate's whole point is lost.
func TestAPasswordForSomethingElseDoesNotOpenIt(t *testing.T) {
	store := upgradeStore(t)
	gate := testGate(t, store)
	ctx := context.Background()

	setLock(t, store, gate, true)

	// A real, valid, current authorization - for a different action.
	wrong := authorize(t, gate, KeyPrivacyIPStorage)

	_, err := store.RequestUpgrade(ctx, operatorAccess(), wrong, "op-test")
	if !errors.Is(err, ErrUpgradeLocked) {
		t.Fatalf("an authorization minted for %q started an upgrade (err = %v)",
			GateAction(KeyPrivacyIPStorage), err)
	}
}

// TestAViewerIsRefusedBeforeThePasswordIsEvenConsidered.
//
// Entitlement before authorization, and before anything that costs work:
// a viewer is refused on who they are, whatever they typed. So no
// password they could supply changes the answer, and no attempt of
// theirs consumes the developer's failure budget.
func TestAViewerIsRefusedBeforeThePasswordIsEvenConsidered(t *testing.T) {
	store := upgradeStore(t)
	ctx := context.Background()

	viewer := Access{
		Principal: Principal{Kind: PrincipalUser, UserID: 2, Label: "izleyici@example.invalid"},
		Role:      RoleViewer, Member: true,
	}

	_, err := store.RequestUpgrade(ctx, viewer, devgate.Authorization{}, "op-test")
	if !errors.Is(err, ErrSettingNotWritable) {
		t.Fatalf("a viewer's upgrade request failed with %v, want ErrSettingNotWritable", err)
	}
	if errors.Is(err, ErrUpgradeLocked) {
		t.Error("a viewer is told the lock is the problem, which invites them to go " +
			"and find the password for a door that is not theirs either way")
	}
}

// TestAnUpgradeThatWouldChangeNothingIsRefused.
//
// A request that queued, ran and changed nothing would still take the
// one in-flight slot, and afterwards would read exactly like a real
// upgrade. Somebody looking at the history a month later could not tell
// which rows meant anything.
func TestAnUpgradeThatWouldChangeNothingIsRefused(t *testing.T) {
	store := upgradeStore(t)
	ctx := context.Background()
	setInstalledVersion(t, false)

	status, err := store.UpgradeStatus(ctx, operatorAccess())
	if err != nil {
		t.Fatal(err)
	}
	if status.Needed {
		t.Fatal("the precondition was set to up-to-date and the status disagrees")
	}

	_, err = store.RequestUpgrade(ctx, operatorAccess(), devgate.Authorization{}, "op-test")
	if !errors.Is(err, ErrUpgradeNotNeeded) {
		t.Fatalf("an upgrade to the version already installed was accepted (err = %v)", err)
	}
}

// TestPressingItIsRecordedInTheAuditLog.
//
// The request row says what happened to the migration and is swept after
// thirty days. This says a person asked for one, and is the half that
// has to survive.
func TestPressingItIsRecordedInTheAuditLog(t *testing.T) {
	store := upgradeStore(t)
	ctx := context.Background()
	setInstalledVersion(t, true)

	status, err := store.UpgradeStatus(ctx, operatorAccess())
	if err != nil {
		t.Fatal(err)
	}
	if !status.Needed {
		t.Fatal("the precondition was set to behind and the status disagrees")
	}
	if _, err := store.RequestUpgrade(ctx, operatorAccess(), devgate.Authorization{}, "op-test"); err != nil {
		t.Fatalf("requesting: %v", err)
	}

	entries, _, err := store.Audit(ctx, AuditFilter{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Action == ActionUpgradeRequested {
			if !strings.Contains(e.Target, "->") {
				t.Errorf("the entry does not say which versions: %q", e.Target)
			}
			return
		}
	}
	t.Error("pressing the upgrade button left no audit entry; the request row is swept " +
		"after thirty days and then nothing records that anybody asked")
}

// authorizeAction mints an authorization for an action that is not a
// setting.
//
// authorize() next door builds one from a setting key; the upgrade gate
// has its own action name, deliberately, so that a password typed for a
// setting cannot also apply a migration.
func authorizeAction(t *testing.T, gate *devgate.Gate, action string) devgate.Authorization {
	t.Helper()
	result := gate.Verify(context.Background(), devgate.Request{
		Actions:   []string{action},
		Password:  testDevPassword,
		Actor:     "test@example.com",
		ActorKind: string(PrincipalUser),
		Peer:      "203.0.113.9",
	})
	if !result.OK() {
		t.Fatalf("the gate refused a correct password: %s", result.Decision)
	}
	return result.For(action)
}
