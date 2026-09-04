//go:build integration

// The escape: what happens when the new version does not come back.
//
// This is the path with the least testing in any system, because it
// runs only when something else has already gone wrong. It is also the
// path whose failure costs the customer's website, so it gets the
// tests the happy path does not need.

package relupdate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// beat writes a heartbeat row for one service, as that service, at the
// database's clock.
//
// Written through the service's own role rather than as an
// administrator, because the row-level policy on service_heartbeat
// checks the connected role: a fixture that inserted as a superuser
// would prove the reader works against rows production cannot produce.
//
// # And timed by now(), not by a value this test picked
//
// The same argument, one layer down. The real reporter writes
// `beat_at = now()`; a fixture that chose the timestamp could write one
// production never produces, and the first version of this file wrote
// them a second into the *future*.
//
// That is not a tidiness point. Every test in this package shares one
// database, and a row dated a second from now looks, to whatever runs
// next, exactly like a service that has just come back. It is how
// TestAReleaseThatDoesNotComeBackIsPutBackAutomatically was made to
// pass while the escape it tests did nothing at all: the services in
// that test never reported, and four rows left behind by the test
// before it said they had.
//
// So the fixture writes what a service writes, and the tests get the
// before and after they need by *ordering* rather than by arithmetic:
// beat before Since for a stale row, after it for a fresh one.
func beat(t *testing.T, role string) {
	t.Helper()
	pool := testdb.Pool(t, role)
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO service_heartbeat (service, version, started_at, beat_at, counters)
		VALUES (current_user, 'test', now(), now(), '{}'::jsonb)
		ON CONFLICT (service) DO UPDATE SET beat_at = now()`); err != nil {
		t.Fatalf("writing a heartbeat as %s: %v", role, err)
	}
}

// allBeat makes every watched service report now.
func allBeat(t *testing.T) {
	t.Helper()
	for _, role := range []string{testdb.Collector, testdb.Beacon, testdb.Reader, testdb.Panel} {
		beat(t, role)
	}
}

// clockOf reads the database's clock, failing the test rather than
// returning an error nobody checks.
func clockOf(t *testing.T, d Doorbell) time.Time {
	t.Helper()
	at, err := d.Since(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return at
}

// nothingIsComing shortens the wait for the tests whose answer cannot
// change by waiting.
//
// Those tests write no heartbeat for the services they expect to be
// missing, so the full window would buy three seconds of sleep per test
// and no additional evidence. The tests where waiting *is* the point -
// the runner's, where a goroutine reports partway through - keep the
// real window.
func nothingIsComing(d *Doorbell) { d.Window = 200 * time.Millisecond }

func doorbellIn(t *testing.T, pool *pgxpool.Pool) (Doorbell, string) {
	t.Helper()
	dir := t.TempDir()
	return Doorbell{
		Dir:    dir,
		Pool:   pool,
		Window: 3 * time.Second,
		Poll:   50 * time.Millisecond,
	}, dir
}

// TestNoRestarterMeansNoRestartAndNoFailure.
//
// The default, and it has to stay boring: a deployment that never
// installed the units updates exactly as it did before, and the absence
// is not reported as a fault.
func TestNoRestarterMeansNoRestartAndNoFailure(t *testing.T) {
	d := Doorbell{Dir: filepath.Join(t.TempDir(), "not-created")}
	if d.Configured() {
		t.Fatal("a directory that does not exist was reported as a configured restarter")
	}
	if err := d.Ring(); err == nil {
		t.Error("ringing a doorbell nobody installed reported success")
	} else if !strings.Contains(err.Error(), "restart") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// TestTheRequestCarriesNothing.
//
// The whole security argument for this channel. A request naming units,
// paths or a version would be a request choosing what root runs; this
// one is a doorbell, and the test asserts the file is empty rather than
// trusting the code to keep writing nothing into it.
func TestTheRequestCarriesNothing(t *testing.T) {
	d, dir := doorbellIn(t, nil)
	if err := d.Ring(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, DoorbellName))
	if err != nil {
		t.Fatal(err)
	}
	if len(body) != 0 {
		t.Errorf("the request carries %d bytes (%q). Every field in a request is a "+
			"permission granted to whoever writes it, and this one is triggered by the "+
			"component that fetches archives off the network", len(body), body)
	}
}

// TestRingingTwiceRewritesTheSameFile.
//
// The .path unit fires on modification. A second request that left the
// file untouched would not fire, and the rollback's own restart - the
// second ring - is exactly that case.
func TestRingingTwiceRewritesTheSameFile(t *testing.T) {
	d, dir := doorbellIn(t, nil)
	if err := d.Ring(); err != nil {
		t.Fatal(err)
	}
	first, err := os.Stat(filepath.Join(dir, DoorbellName))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := d.Ring(); err != nil {
		t.Fatal(err)
	}
	second, err := os.Stat(filepath.Join(dir, DoorbellName))
	if err != nil {
		t.Fatal(err)
	}
	if !second.ModTime().After(first.ModTime()) {
		t.Error("the second request did not change the file's modification time, so a " +
			"unit watching for modification would not fire - and the second ring is the " +
			"one that starts the services again after a rollback")
	}
}

// TestEveryServiceHasToReportBackNotJustOne.
//
// The check that would otherwise be satisfied by the panel alone, which
// is the service least likely to be broken by a collector's bad release
// and the one most likely to answer first.
func TestEveryServiceHasToReportBackNotJustOne(t *testing.T) {
	pool := testdb.Pool(t, testdb.SchemaAdmin)
	d, _ := doorbellIn(t, pool)
	nothingIsComing(&d)
	ctx := context.Background()

	since := clockOf(t, d)
	// Only the panel comes back.
	beat(t, testdb.Panel)

	missing, err := d.Healthy(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) == 0 {
		t.Fatal("one service reporting back was accepted as all of them")
	}
	for _, want := range []string{"collector", "beacon_writer", "analytics_reader"} {
		found := false
		for _, m := range missing {
			if m == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s did not report and is not in the missing list %v", want, missing)
		}
	}
}

// TestAStaleHeartbeatIsNotAServiceComingBack.
//
// The subtle one, and the reason Healthy compares against a time rather
// than asking whether a row exists. Every service has a row from before
// the restart; a check that looked for presence would pass instantly and
// always, and the rollback would never fire.
func TestAStaleHeartbeatIsNotAServiceComingBack(t *testing.T) {
	pool := testdb.Pool(t, testdb.SchemaAdmin)
	d, _ := doorbellIn(t, pool)
	nothingIsComing(&d)
	ctx := context.Background()

	// Rows exist, all of them - and then the restart is asked for. The
	// order is the whole fixture: nothing here backdates anything, the
	// rows are simply older than the question.
	allBeat(t)
	since := clockOf(t, d)

	missing, err := d.Healthy(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != len(HealthServices) {
		t.Errorf("with every row written before the restart, %d of %d services were "+
			"treated as having come back. A row that was already there is not evidence "+
			"of a restart", len(HealthServices)-len(missing), len(HealthServices))
	}
}

// TestEveryServiceReportingIsAccepted is the positive case, so the two
// above cannot be satisfied by a check that always says "missing".
func TestEveryServiceReportingIsAccepted(t *testing.T) {
	pool := testdb.Pool(t, testdb.SchemaAdmin)
	d, _ := doorbellIn(t, pool)
	ctx := context.Background()

	since := clockOf(t, d)
	allBeat(t)

	missing, err := d.Healthy(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Errorf("every service reported back and %v were still called missing", missing)
	}
}

// TestTheRestartIsTimedByTheDatabaseClockNotThisMachines.
//
// beat_at is written by the database - the heartbeat's INSERT says
// now() - and the moment the restart was asked for used to be read from
// the upgrader's own clock. Two clocks, one comparison, and what that
// comparison decides is whether to undo a release.
//
// The test runs the local clock an hour fast, which is the direction
// that used to roll back healthy releases: every service reports, every
// row is dated an hour before the restart, and the escape fires on a
// version that was fine. An hour slow is the other failure and needs no
// second test - it is the same single comparison.
//
// The skew has to be invisible. Nothing in the decision may come from
// this machine's clock.
func TestTheRestartIsTimedByTheDatabaseClockNotThisMachines(t *testing.T) {
	pool := testdb.Pool(t, testdb.SchemaAdmin)
	d, _ := doorbellIn(t, pool)
	// A skewed clock that still runs, so Healthy's own deadline still
	// arrives; a frozen one would test something else.
	d.Now = func() time.Time { return time.Now().Add(time.Hour) }
	ctx := context.Background()

	since := clockOf(t, d)
	allBeat(t)

	missing, err := d.Healthy(ctx, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) > 0 {
		t.Errorf("with this machine's clock an hour ahead of the database's, %v were "+
			"reported as not having come back. They did come back. A release is rolled "+
			"back on the strength of this answer, so a wrong clock here uninstalls "+
			"working software", missing)
	}
}

// TestTheCheckpointIsRestoredByteForByte.
//
// The escape's own mechanism. Copied rather than renamed, so a restore
// that fails halfway can be run again - which the test asserts by
// running it twice.
func TestTheCheckpointIsRestoredByteForByte(t *testing.T) {
	prefix := t.TempDir()
	binDir := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	previous := filepath.Join(binDir, ".previous-test")
	if err := os.MkdirAll(previous, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(previous, "collector"), []byte("the old one"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "collector"), []byte("the new one"), 0o755); err != nil {
		t.Fatal(err)
	}

	in := Installer{Prefix: prefix}
	for attempt := 1; attempt <= 2; attempt++ {
		if err := in.Restore(previous); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		body, err := os.ReadFile(filepath.Join(binDir, "collector"))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "the old one" {
			t.Fatalf("attempt %d: the binary is %q", attempt, body)
		}
		// And the file is still executable, or nothing installed runs.
		info, err := os.Stat(filepath.Join(binDir, "collector"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("attempt %d: restored as %v, which systemd cannot execute",
				attempt, info.Mode().Perm())
		}
	}
}

// TestAnEmptyCheckpointSaysSoRatherThanReportingSuccess.
//
// A release that only *added* binaries leaves an empty checkpoint.
// Restoring it does nothing, and a nil error there would tell an
// operator their rollback worked.
func TestAnEmptyCheckpointSaysSoRatherThanReportingSuccess(t *testing.T) {
	prefix := t.TempDir()
	previous := filepath.Join(prefix, "bin", ".previous-empty")
	if err := os.MkdirAll(previous, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (Installer{Prefix: prefix}).Restore(previous); err == nil {
		t.Error("restoring an empty checkpoint reported success. Nothing was put back, " +
			"and an operator told otherwise stops looking")
	}
}

// TestForgetOnlyHappensAfterTheServicesReport is asserted through the
// runner in runner_integration_test.go; here it is the narrow claim that
// Forget removes the directory and tolerates being handed nothing.
func TestForgetRemovesTheCheckpointAndToleratesNothing(t *testing.T) {
	in := Installer{Prefix: t.TempDir()}
	if err := in.Forget(""); err != nil {
		t.Errorf("forgetting no checkpoint reported an error: %v", err)
	}
	dir := filepath.Join(t.TempDir(), ".previous-x")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := in.Forget(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("the checkpoint is still there")
	}
}
