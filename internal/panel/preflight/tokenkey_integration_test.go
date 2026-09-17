//go:build integration

package preflight

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// The wizard's IP-token-key check, against real rows.
//
// # Why this check had no test before, and what that cost
//
// It had no database to ask. It read a boolean the caller was supposed to
// know, cmd/panel took it from a store field no production code set, and
// so the check reported "not configured" on every installation there has
// ever been - including the ones that had a key. A test with a
// hand-supplied answer would have agreed with it.
//
// Nothing here is a fake. The rows are real, the roles are real, and the
// three answers this check can give correspond to three deployments.
//
// The rows go into a copy of service_heartbeat in this suite's own schema
// - see testdb.NewHeartbeatCopy for why the shared table will not do.
func newTokenKeyChecker(t *testing.T) (*Checker, *testdb.HeartbeatCopy) {
	t.Helper()
	// Before the pool, so the schema is dropped after the pool closes
	// rather than under it.
	beats := testdb.NewHeartbeatCopy(t, "ca_preflight_heartbeat")

	pool, err := pgxpool.New(context.Background(), beats.DSN(testDatabaseURL))
	if err != nil {
		t.Fatalf("pgxpool.New: %v (is the database up and installed? see internal/testdb)", err)
	}
	t.Cleanup(pool.Close)
	beats.Reaches(pool)
	return New(pool), beats
}

// TestTheWizardAsksTheServicesAboutTheIPTokenKey.
//
// Three deployments, three answers, and the middle one is the reason the
// states are kept apart: a service that reports nothing needs a newer
// build, and a service that reports no key needs a line in a file. The
// wizard is read by somebody standing at a terminal, so telling them
// which is the whole value of the row.
//
// Skip rather than fail throughout, and that is deliberate: masked mode
// is a correct deployment and this key is needed only to leave it.
// Blocking handover over it would teach an installer that a red row is
// something to ignore.
func TestTheWizardAsksTheServicesAboutTheIPTokenKey(t *testing.T) {
	checker, beats := newTokenKeyChecker(t)
	ctx := context.Background()

	// ---- a fresh install: nothing has started ----
	beats.Silence()
	got := checker.checkIPTokenKey(ctx)
	if got.Status != CheckSkip {
		t.Errorf("status = %s, want skip: on a fresh install the services have not run yet, "+
			"and reporting a missing key there is reporting a fault that does not exist",
			got.Status)
	}
	if !strings.Contains(got.Detail, "henüz") {
		t.Errorf("the detail does not say the services have not spoken yet: %q", got.Detail)
	}
	if got.Fix == "" {
		t.Error("no fix offered; every row on this page that is not a pass has to say what to run")
	}

	// ---- a deployment with a key ----
	beats.Says(testdb.Collector, string(heartbeat.TokenKeyPresent))
	got = checker.checkIPTokenKey(ctx)
	if got.Status != CheckPass {
		t.Errorf("status = %s, want pass: the one writer that has started reported a usable "+
			"key (%s)", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, testdb.Collector) {
		t.Errorf("the pass does not name the service it heard from: %q", got.Detail)
	}
	// The sentence an installer reads next to it has to stay true: full
	// mode stores a token, never the address.
	if !strings.Contains(got.Detail, "ham adres") {
		t.Errorf("the pass does not say the raw address is still not stored: %q", got.Detail)
	}

	// ---- a second writer with none ----
	beats.Says(testdb.Beacon, string(heartbeat.TokenKeyAbsent))
	got = checker.checkIPTokenKey(ctx)
	if got.Status != CheckSkip {
		t.Errorf("status = %s, want skip", got.Status)
	}
	if !strings.Contains(got.Detail, testdb.Beacon) {
		t.Errorf("the detail does not name the service with no key: %q", got.Detail)
	}
	if strings.Contains(got.Detail, "eski sürüm") {
		t.Errorf("a service reporting 'absent' was described as an old build: %q", got.Detail)
	}

	// ---- and the same writer, too old to answer ----
	beats.Says(testdb.Beacon, string(heartbeat.TokenKeyUnknown))
	got = checker.checkIPTokenKey(ctx)
	if got.Status != CheckSkip {
		t.Errorf("status = %s, want skip", got.Status)
	}
	if !strings.Contains(got.Detail, "eski sürüm") {
		t.Errorf("a silent writer is not described as one, so the installer is sent to edit a "+
			"file rather than to upgrade a binary: %q", got.Detail)
	}
}

// TestTheWizardIgnoresTheServicesThatWriteNoAddresses.
//
// The same filter the panel's gate uses, measured through the other
// reader. Both call internal/tokenkey, and the point of asserting it
// twice is that only one of them is on the path an installer walks: a
// wizard that demanded a key from the read API would be unfinishable on
// a correct deployment, and nobody would know why.
func TestTheWizardIgnoresTheServicesThatWriteNoAddresses(t *testing.T) {
	checker, beats := newTokenKeyChecker(t)
	ctx := context.Background()

	beats.Silence()
	beats.Says(testdb.Collector, string(heartbeat.TokenKeyPresent))
	if got := checker.checkIPTokenKey(ctx); got.Status != CheckPass {
		t.Fatalf("the baseline this test varies from is already %s: %s", got.Status, got.Detail)
	}

	for _, role := range []string{testdb.Reader, testdb.Panel, testdb.SchemaAdmin, "postgres"} {
		beats.Says(role, string(heartbeat.TokenKeyUnknown))
		got := checker.checkIPTokenKey(ctx)
		if got.Status != CheckPass {
			t.Errorf("a silent heartbeat row for %q turned the check into %s: %s\n"+
				"That role holds no granted INSERT on an address table, so it is not a "+
				"service this key is about.", role, got.Status, got.Detail)
		}
		beats.Forget(role)
	}
}
