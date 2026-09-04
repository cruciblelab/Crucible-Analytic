//go:build integration

// The release request, against a real database.
//
// The rule this file follows, which this project has had to learn
// twice: when testing a refusal, remove every reason for it except the
// one under test, then check the error's identity. "Something refused"
// is a wish, not a test - a call refused for the wrong reason passes it
// exactly as well as a call refused for the right one.

package panel

import (
	"context"
	"errors"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/relupdate"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

const releaseTestVersion = "v9.9.9"

// releaseStore opens a store and takes the queue's lock.
//
// ReleaseQueueLock, because panel_release_requests has a partial unique
// index that allows exactly one in-flight row for the whole database:
// two suites asking at once make one of them fail with
// ErrAlreadyInFlight, in the other package, on a different runner. This
// project has already paid for that lesson on schema_version.
func releaseStore(t *testing.T) (*Store, *devgate.Gate, context.Context) {
	t.Helper()
	store := &Store{pool: testdb.Pool(t, testdb.Panel)}
	testdb.Lock(t, testdb.Pool(t, testdb.SchemaAdmin), testdb.ReleaseQueueLock)

	// Swept through the panel's own role, and the error is checked.
	//
	// The first version used schema_admin and the sweep silently did
	// nothing: panel_release_requests has FORCE ROW LEVEL SECURITY and
	// its DELETE policy grants panel_user only - schema_admin answers
	// and does not sweep. A cleanup that deletes no rows and reports no
	// error left one pending row behind, and the next test failed with
	// ErrAlreadyInFlight in a place that had nothing to do with it.
	//
	// *Hata döndürmeyen bir temizlik, temizlediğini kanıtlamaz.*
	t.Cleanup(func() {
		if _, err := store.pool.Exec(context.Background(),
			`DELETE FROM panel_release_requests`); err != nil {
			t.Errorf("sweeping the release queue: %v", err)
		}
	})
	return store, testGate(t, store), context.Background()
}

// setReleaseLock moves the lock through the owner's own path, which is
// the only way it moves in production - the setting requires the
// developer password in both directions.
func setReleaseLock(t *testing.T, store *Store, gate *devgate.Gate, on bool) {
	t.Helper()
	if err := store.ApplySetting(context.Background(), operatorAccess(),
		KeyReleaseUpdateLocked, "", on, authorize(t, gate, KeyReleaseUpdateLocked), nil); err != nil {
		t.Fatalf("setting the release lock to %v: %v", on, err)
	}
}

// TestTheLockIsOnBeforeAnybodyConfiguresAnything.
//
// The default is the whole of V5's safety argument, and a default is the
// one thing nobody reads before shipping. It is the opposite of the
// schema upgrade's, deliberately: that one runs against a database that
// keeps serving, this one replaces the collector standing in front of
// the customer's website.
func TestTheLockIsOnBeforeAnybodyConfiguresAnything(t *testing.T) {
	def, ok := Lookup(KeyReleaseUpdateLocked)
	if !ok {
		t.Fatal("release.locked is not in the registry")
	}
	if def.Default != true {
		t.Errorf("release.locked defaults to %v. It has to start locked: replacing the "+
			"binaries replaces the collector, and the panel that would undo it may be "+
			"down beside it", def.Default)
	}
	if !def.RequiresDeveloperPassword {
		t.Error("release.locked can be changed without the developer password. A lock a " +
			"customer can open is not one, and one they can close would shut the " +
			"developer's own button")
	}

	upgradeDef, ok := Lookup(KeyUpgradeLocked)
	if !ok {
		t.Fatal("upgrade.locked is not in the registry")
	}
	if upgradeDef.Default == def.Default {
		t.Errorf("the schema lock and the release lock now default the same way (%v). "+
			"The difference between them is this project's statement about which "+
			"operation is riskier; if it has genuinely changed, the reasoning in "+
			"releaserequest.go has to change with it", def.Default)
	}
}

// TestALockedDeploymentRefusesWithoutThePassword.
//
// Everything except the lock is removed: the actor owns the site, the
// version is valid, the queue is empty. So the only thing left that can
// refuse is the lock, and the assertion is that the error says so.
func TestALockedDeploymentRefusesWithoutThePassword(t *testing.T) {
	store, gate, ctx := releaseStore(t)
	setReleaseLock(t, store, gate, true)

	_, err := store.RequestRelease(ctx, releaseOwner(), devgate.Authorization{},
		"", "v0.1.0", releaseTestVersion)
	if !errors.Is(err, ErrReleaseLocked) {
		t.Fatalf("a locked deployment refused with %v; want ErrReleaseLocked. Anything "+
			"else means the request was stopped by something other than the lock, and "+
			"the lock itself is untested", err)
	}

	if latest, lerr := relupdate.Latest(ctx, store.pool); lerr != nil {
		t.Fatal(lerr)
	} else if latest != nil {
		t.Error("a refused request still wrote a row. It would occupy the one in-flight " +
			"slot, so the next legitimate request would be refused as a duplicate")
	}
}

// TestAnUnlockedDeploymentQueuesTheRequest.
//
// The other side of the same switch, and it has to be asserted or the
// test above passes on a store that refuses everything.
func TestAnUnlockedDeploymentQueuesTheRequest(t *testing.T) {
	store, gate, ctx := releaseStore(t)
	setReleaseLock(t, store, gate, false)

	req, err := store.RequestRelease(ctx, releaseOwner(), devgate.Authorization{},
		"", "v0.1.0", releaseTestVersion)
	if err != nil {
		t.Fatalf("an unlocked deployment refused a valid request: %v", err)
	}
	if req.ToVersion != releaseTestVersion {
		t.Errorf("the row asks for %q, want %q", req.ToVersion, releaseTestVersion)
	}
	if req.FromVersion != "v0.1.0" {
		t.Errorf("the row records %q as the version being replaced; the caller passed "+
			"v0.1.0, and a store that substituted its own would name a version nothing "+
			"is running", req.FromVersion)
	}
}

// TestAnUnreadableVersionIsRefusedBeforeAnythingIsWritten.
//
// The lock is off and the actor is entitled, so a refusal here can only
// be the version. The shapes are the ones somebody actually types: a
// bare number, a tag with a prefix, and a path - the last because a
// version reaches the upgrader and becomes part of a URL.
func TestAnUnreadableVersionIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	store, gate, ctx := releaseStore(t)
	setReleaseLock(t, store, gate, false)

	// The baseline, so the assertion is "no *new* row" rather than "the
	// table is empty". The second is a claim about every other test in
	// this package, and it fails for their reasons rather than for this
	// one's.
	before, err := relupdate.Latest(ctx, store.pool)
	if err != nil {
		t.Fatal(err)
	}
	beforeID := int64(0)
	if before != nil {
		beforeID = before.ID
	}

	for name, version := range map[string]string{
		"empty":            "",
		"no v":             "0.21.0",
		"words":            "latest",
		"path traversal":   "v0.21.0/../../etc",
		"query smuggling":  "v0.21.0?x=1",
		"absolute url":     "https://example.invalid/v0.21.0",
		"trailing newline": "v0.21.0\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := store.RequestRelease(ctx, releaseOwner(), devgate.Authorization{},
				"", "v0.1.0", version)
			if !errors.Is(err, ErrReleaseBadVersion) {
				t.Fatalf("%q was refused with %v; want ErrReleaseBadVersion", version, err)
			}
			latest, lerr := relupdate.Latest(ctx, store.pool)
			if lerr != nil {
				t.Fatal(lerr)
			}
			if latest != nil && latest.ID != beforeID {
				t.Errorf("%q wrote a row before being refused: %+v", version, latest)
			}
		})
	}
}

// TestALockedDeploymentSaysLockedEvenWhenTheVersionIsAlsoWrong.
//
// This is the test that pins the order of the checks, and it exists
// because the first round of mutations found nothing to pin it: with a
// valid version, "lock then version" and "version then lock" give the
// same answer, so every test above passed under both.
//
// The order is not arbitrary. The lock is the fact the caller is not
// entitled to work around; whether the version they named exists is
// something they get to learn once they are past it. A deployment that
// answered "that is not a version" to somebody who cannot install any
// version has told them about the state of the shelf through a door
// they were refused at.
func TestALockedDeploymentSaysLockedEvenWhenTheVersionIsAlsoWrong(t *testing.T) {
	store, gate, ctx := releaseStore(t)
	setReleaseLock(t, store, gate, true)

	_, err := store.RequestRelease(ctx, releaseOwner(), devgate.Authorization{},
		"", "v0.1.0", "kesinlikle-bir-surum-degil")
	if !errors.Is(err, ErrReleaseLocked) {
		t.Fatalf("a locked deployment given a bad version answered %v; want "+
			"ErrReleaseLocked. The lock comes first: what versions exist is "+
			"something a refused caller does not get to learn", err)
	}
}

// TestAViewerIsRefusedOnWhoTheyAreAndNotOnThePassword.
//
// The order matters and is asserted rather than assumed: a viewer who
// supplied a correct password must still be refused, and refused for
// lack of entitlement rather than for the lock. The reverse order would
// make the developer password a way for anybody holding it to act as
// any account.
func TestAViewerIsRefusedOnWhoTheyAreAndNotOnThePassword(t *testing.T) {
	store, gate, ctx := releaseStore(t)
	setReleaseLock(t, store, gate, true)

	viewer := Access{Principal: Principal{Kind: PrincipalUser, UserID: 1, Label: "izleyici"},
		Role: RoleViewer, Member: true}
	authorised := authorizeAction(t, gate, ReleaseGateAction)

	_, err := store.RequestRelease(ctx, viewer, authorised, "", "v0.1.0", releaseTestVersion)
	if !errors.Is(err, ErrSettingNotWritable) {
		t.Fatalf("a viewer holding a valid authorization was refused with %v; want "+
			"ErrSettingNotWritable. Being refused for the lock instead would mean the "+
			"password can stand in for a role", err)
	}
}

// TestAValidPasswordOpensALockedDeployment.
//
// Without this the lock tests above are satisfied by a store that
// refuses every locked request whatever it is given, which is a
// different system from the one that was designed.
func TestAValidPasswordOpensALockedDeployment(t *testing.T) {
	store, gate, ctx := releaseStore(t)
	setReleaseLock(t, store, gate, true)

	req, err := store.RequestRelease(ctx, releaseOwner(),
		authorizeAction(t, gate, ReleaseGateAction), "", "v0.1.0", releaseTestVersion)
	if err != nil {
		t.Fatalf("a locked deployment refused an authorised request: %v", err)
	}
	if req == nil {
		t.Fatal("no row was written")
	}
}

// TestAnAuthorizationForTheSchemaUpgradeDoesNotInstallBinaries.
//
// The two locks are separate settings and the two actions are separate
// strings, so a password typed to migrate a database must not also
// replace the collector. Worth asserting because the failure is silent:
// an authorization that happened to satisfy both would look like the
// system working.
func TestAnAuthorizationForTheSchemaUpgradeDoesNotInstallBinaries(t *testing.T) {
	store, gate, ctx := releaseStore(t)
	setReleaseLock(t, store, gate, true)

	_, err := store.RequestRelease(ctx, releaseOwner(),
		authorizeAction(t, gate, UpgradeGateAction), "", "v0.1.0", releaseTestVersion)
	if !errors.Is(err, ErrReleaseLocked) {
		t.Fatalf("an authorization minted for %q was accepted for %q (err=%v). "+
			"Scoping an authorization to an action is the whole reason it carries one",
			UpgradeGateAction, ReleaseGateAction, err)
	}
}

// releaseOwner is a site owner: entitled to manage settings, and
// stopped only by the lock.
//
// A member owner rather than operatorAccess's superadmin, because the
// claim being tested is about the customer. A superadmin passing the
// entitlement check proves nothing about whether a customer does.
func releaseOwner() Access {
	return Access{
		Principal: Principal{Kind: PrincipalUser, UserID: 1, Label: "surum-sahibi"},
		Role:      RoleOwner,
		Member:    true,
	}
}
