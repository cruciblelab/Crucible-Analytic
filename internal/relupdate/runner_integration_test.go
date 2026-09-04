//go:build integration

// The chain, not the links.
//
// # Why this file exists
//
// Every piece of the update path had tests and worked. Claim had one,
// Fetch had one, Install had several. Nothing tested the sentence that
// joins them, so nothing noticed that the sentence had never been
// written: no process called Claim, and pressing the panel's button
// wrote a row that sat at "pending" forever.
//
// The failure had no error in it. It was not a crash, a refusal or a
// log line - it was waiting, indefinitely, which is the one outcome a
// customer cannot distinguish from "this is slow".
//
// *Her halkası test edilmiş bir zincir, test edilmiş bir zincir
// değildir.*

package relupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// runnerQueue opens the queue as both roles and takes its lock.
//
// Two pools, because the separation is the design: the panel's role
// asks and cannot answer, the upgrader's answers and cannot ask. A test
// that used one connection for both halves would run against a
// permission arrangement this project does not ship - and would go on
// passing if the policies were removed entirely.
//
// The first run of this file did exactly that, and the schema refused
// it. That refusal is the feature.
func runnerQueue(t *testing.T) (asks, answers *pgxpool.Pool) {
	t.Helper()
	answers = testdb.Pool(t, testdb.SchemaAdmin)
	asks = testdb.Pool(t, testdb.Panel)
	testdb.Lock(t, answers, testdb.ReleaseQueueLock)
	t.Cleanup(func() {
		// Swept by the role the DELETE policy names, which is the panel's.
		if _, err := asks.Exec(context.Background(),
			`DELETE FROM panel_release_requests`); err != nil {
			t.Errorf("sweeping the release queue: %v", err)
		}
	})
	return asks, answers
}

// TestAQueuedRequestIsActuallyCarriedOut is the headline, and the test
// whose absence was the defect.
//
// It asks the only question that matters about the whole path: press
// the button, and does anything happen. Everything below it is about
// what happens when something goes wrong; this one is about the
// ordinary case, which is the one that was broken.
func TestAQueuedRequestIsActuallyCarriedOut(t *testing.T) {
	asks, pool := runnerQueue(t)
	ctx := context.Background()

	src, version := servedPackage(t)
	prefix := t.TempDir()

	if _, err := Ask(ctx, asks, Actor{Kind: "user", Label: "test"}, "", "v0.0.1", version); err != nil {
		t.Fatal(err)
	}

	runner := Runner{
		Pool:   pool,
		Source: src,
		Name:   "test-upgrader",
		Install: Installer{
			Prefix: prefix,
			// A verifier that accepts, because the package's binaries are
			// fixtures rather than real programs. What is under test here
			// is the chain; whether a binary runs is install_test.go's.
			Verify: func(context.Context, string) error { return nil },
		},
	}

	req, err := runner.RunOnce(ctx)
	if err != nil {
		t.Fatalf("the queued request was not carried out: %v", err)
	}
	if req == nil {
		t.Fatal("RunOnce reported nothing to do, with a request waiting in the queue. " +
			"That is the exact state this whole path was in before the runner existed")
	}

	// The row says it finished.
	latest, err := Latest(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != StateSucceeded {
		t.Fatalf("the request ended in state %q: %s", latest.State, latest.ErrorChain)
	}
	if latest.InstalledVersion != version {
		t.Errorf("the row records %q as installed, want %q", latest.InstalledVersion, version)
	}

	// And the binaries are actually on disk, which is the half a row
	// cannot vouch for.
	if _, err := os.Stat(filepath.Join(prefix, "bin", "collector")); err != nil {
		t.Errorf("the request succeeded and no binary was installed: %v", err)
	}
}

// TestARequestOnADeploymentWithNoSourceFailsInsteadOfWaiting.
//
// The state that produced the original defect's symptom, and it has to
// end on the row rather than in silence: somebody is watching the page,
// and "nothing is configured" is the answer they need. A request left
// pending would additionally hold the one in-flight slot forever.
func TestARequestOnADeploymentWithNoSourceFailsInsteadOfWaiting(t *testing.T) {
	asks, pool := runnerQueue(t)
	ctx := context.Background()

	if _, err := Ask(ctx, asks, Actor{Kind: "user", Label: "test"}, "", "v0.0.1", "v9.9.9"); err != nil {
		t.Fatal(err)
	}

	runner := Runner{Pool: pool, Name: "test-upgrader"} // no source at all
	if _, err := runner.RunOnce(ctx); err == nil {
		t.Fatal("a request on a deployment with no release source reported success")
	}

	latest, err := Latest(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State != StateFailed {
		t.Errorf("the request is in state %q; it has to end failed, with a reason, or it "+
			"holds the one in-flight slot and every later request is refused as a "+
			"duplicate", latest.State)
	}
	if latest.ErrorChain == "" {
		t.Error("the row carries no reason, so the page has nothing to show")
	}
}

// TestAPackageThatDoesNotVerifyChangesNothingOnTheMachine.
//
// The claim the page makes in its own words - "çalışmazsa eskisi geri
// konur" - has a stronger version that holds earlier: a package that
// fails verification never reaches the install step at all, so there is
// nothing to put back.
func TestAPackageThatDoesNotVerifyChangesNothingOnTheMachine(t *testing.T) {
	asks, pool := runnerQueue(t)
	ctx := context.Background()

	src, version := servedPackage(t)
	// A different key: the package is real and signed, and this source
	// checks it against somebody else's public key.
	_, other := keys(t)
	src.PublicKey = other

	prefix := t.TempDir()
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(prefix, "bin", "collector")
	if err := os.WriteFile(existing, []byte("the one that was already here"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Ask(ctx, asks, Actor{Kind: "user", Label: "test"}, "", "v0.0.1", version); err != nil {
		t.Fatal(err)
	}

	runner := Runner{Pool: pool, Source: src, Name: "test-upgrader",
		Install: Installer{Prefix: prefix, Verify: func(context.Context, string) error { return nil }}}
	if _, err := runner.RunOnce(ctx); err == nil {
		t.Fatal("a package signed by the wrong key was installed")
	}

	body, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "the one that was already here" {
		t.Error("the binary on disk changed even though the package did not verify")
	}

	latest, err := Latest(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if latest.RolledBack {
		t.Error("the row says it rolled back. Nothing was replaced, so there was nothing " +
			"to roll back - and telling an operator otherwise sends them looking for " +
			"a restore that never happened")
	}
}

// TestAClaimNobodyFinishedIsTakenBack.
//
// One in-flight slot for the whole deployment means an upgrader that
// died mid-request blocks every later one, not just its own.
func TestAClaimNobodyFinishedIsTakenBack(t *testing.T) {
	asks, pool := runnerQueue(t)
	ctx := context.Background()

	src, version := servedPackage(t)
	if _, err := Ask(ctx, asks, Actor{Kind: "user", Label: "test"}, "", "v0.0.1", version); err != nil {
		t.Fatal(err)
	}
	// Claimed and then abandoned, older than the stale age.
	if _, err := Claim(ctx, pool, "an-upgrader-that-died"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE panel_release_requests SET claimed_at = now() - $1::interval WHERE state = 'running'`,
		"1 hour"); err != nil {
		t.Fatal(err)
	}

	runner := Runner{Pool: pool, Source: src, Name: "test-upgrader",
		Install: Installer{Prefix: t.TempDir(),
			Verify: func(context.Context, string) error { return nil }}}

	// The abandoned row is expired, so this pass finds nothing waiting -
	// which is correct: the request failed, and the customer presses the
	// button again rather than having an install start an hour later
	// without anybody watching.
	_, err := runner.RunOnce(ctx)
	if err != nil && !errors.Is(err, ErrNothingToDo) {
		t.Fatalf("the pass failed for an unexpected reason: %v", err)
	}

	latest, err := Latest(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if latest.State == StateRunning {
		t.Error("the abandoned claim is still running, so the one in-flight slot is held " +
			"and no later request can be queued")
	}

	// And a new request can now be made, which is the property that
	// actually matters to somebody at the page.
	if _, err := Ask(ctx, asks, Actor{Kind: "user", Label: "test"}, "", "v0.0.1", version); err != nil {
		t.Fatalf("a new request was refused after a stale claim was expired: %v", err)
	}
}

// servedPackage builds a signed package, serves it, and returns the
// source that reads it plus the version it carries.
//
// Built on fetch_test.go's fixtures rather than a second set: a helper
// that produced a package the real fetcher would not accept would make
// every test above pass against a shape this project never ships.
func servedPackage(t *testing.T) (Source, string) {
	t.Helper()
	priv, pub := keys(t)
	p := newPkg(t)
	p.signWith = priv
	base, client := serve(t, p.build(t))
	return newSource(t, base, client, pub), "v0.20.0"
}
