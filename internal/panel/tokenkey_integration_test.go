//go:build integration

// The precondition on privacy.ip_storage = "full", against a real
// database.
//
// # Why a fake would measure nothing
//
// The defect this file is about was that the precondition read a field
// no production code ever set. Any test with a hand-supplied answer
// would have passed against that version too - the two that existed did,
// for months - because the thing that was wrong was the *route* to the
// answer, not the arithmetic once it arrived.
//
// So the answer has to come from where the product takes it: rows in
// service_heartbeat, filtered by which roles a deployment granted the
// ability to write an address. Both halves are questions to PostgreSQL,
// and neither is answerable without one.

package panel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// heartbeatFixture is this suite's own service_heartbeat, plus the two
// things only this package can say about it.
//
// The mechanism - a schema of one's own, first on the search path - is
// in internal/testdb, because internal/panel/preflight needs the same
// one and may not import this package. What stays here is the typed
// state and the DSN this suite connects with.
type heartbeatFixture struct {
	*testdb.HeartbeatCopy
	dsn string
}

func newHeartbeatFixture(t *testing.T) *heartbeatFixture {
	t.Helper()
	// Not named "copy": that is a builtin, and shadowing it in a fixture
	// is the kind of small surprise a later edit trips over.
	table := testdb.NewHeartbeatCopy(t, "ca_panel_heartbeat")
	return &heartbeatFixture{HeartbeatCopy: table, dsn: table.DSN(testDatabaseURL)}
}

// says is Says with the typed state, so a caller cannot pass a word the
// reader would treat as unknown by accident.
func (f *heartbeatFixture) says(service string, state heartbeat.TokenKeyState) {
	f.Says(service, string(state))
}

// reaches keeps the t-first shape the rest of this file uses.
func (f *heartbeatFixture) reaches(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	f.Reaches(pool)
}

// TestTheGateReadsWhatTheServicesReported walks the states a deployment
// can be in, through the store's own precondition.
//
// One test with stages rather than six, because the store is the
// expensive part and because the ordering carries an assertion of its
// own: the same store, with nothing changed but a row, answers
// differently. A per-case store could pass while the answer was cached
// at construction, which is one edit away from the field this phase
// removed.
func TestTheGateReadsWhatTheServicesReported(t *testing.T) {
	beats := newHeartbeatFixture(t)
	store := newTestStoreAt(t, "tokenkey", beats.dsn)
	beats.reaches(t, store.Pool())
	ctx := context.Background()

	// ---- nothing has reported: refused, and the sentence says so ----
	//
	// First, because it is what a fresh install looks like, and because
	// an implementation that treated "no rows" as "nobody objected"
	// would pass every other stage below.
	beats.Silence()
	err := store.tokenKeyReady(ctx)
	if !errors.Is(err, ErrPreconditionUnmet) {
		t.Fatalf("with no service reporting at all, the gate said %v; full mode would be "+
			"selectable on a deployment nothing is known about", err)
	}
	if !strings.Contains(err.Error(), "has reported") {
		t.Errorf("the refusal does not distinguish 'nothing has spoken' from "+
			"'somebody has no key': %v", err)
	}

	// ---- the collector holds one: allowed ----
	beats.says(testdb.Collector, heartbeat.TokenKeyPresent)
	if err := store.tokenKeyReady(ctx); err != nil {
		t.Fatalf("the collector reported a usable key and the gate still refused: %v", err)
	}

	// ---- the beacon has none: refused, and named ----
	beats.says(testdb.Beacon, heartbeat.TokenKeyAbsent)
	err = store.tokenKeyReady(ctx)
	if !errors.Is(err, ErrPreconditionUnmet) {
		t.Fatalf("a writer with no key did not stop full mode: %v", err)
	}
	if !strings.Contains(err.Error(), testdb.Beacon) {
		t.Errorf("the refusal does not name the service that said no: %v", err)
	}
	if strings.Contains(err.Error(), "older than the column") {
		t.Errorf("a service that reported 'absent' was described as an old build, which "+
			"sends the reader to upgrade a binary rather than to edit a file: %v", err)
	}

	// ---- an old build: refused, and for the other reason ----
	//
	// The state that cannot be a boolean. A build predating the column
	// reports nothing, and nothing must not read as "no key": the fix is
	// an upgrade, not an edit.
	beats.says(testdb.Beacon, heartbeat.TokenKeyUnknown)
	err = store.tokenKeyReady(ctx)
	if !errors.Is(err, ErrPreconditionUnmet) {
		t.Fatalf("a writer that reports nothing did not stop full mode: %v", err)
	}
	if !strings.Contains(err.Error(), "older than the column") {
		t.Errorf("a silent writer is not described as one: %v", err)
	}

	// ---- and the beacon holds one too: allowed again ----
	beats.says(testdb.Beacon, heartbeat.TokenKeyPresent)
	if err := store.tokenKeyReady(ctx); err != nil {
		t.Fatalf("both writers reported a usable key and the gate refused: %v", err)
	}

	// ---- a question that could not be asked is not a yes ----
	//
	// The last stage, and it needs the one before it: the deployment is
	// now ready, so the only thing changed here is that the query fails.
	// Answering "ready" on a failed query would let a switch into full
	// mode land on a deployment nothing is known about, which is the
	// silent degradation this whole precondition exists for.
	//
	// A mutation showed this was unmeasured - replacing the error branch
	// with `return nil` survived every stage above, because none of them
	// could make the query fail. A cancelled context can.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.tokenKeyReady(cancelled); !errors.Is(err, ErrPreconditionUnmet) {
		t.Errorf("a failed query answered %v; an unanswerable question is not a yes", err)
	}
}

// TestOnlyTheAddressWritersHaveToHoldAKey is the half that measures the
// filter rather than the states.
//
// # The three roles that broke the first version
//
// The writers are derived from privileges, and the first version of that
// query asked has_table_privilege and nothing else. On a correctly
// installed database that returns five roles rather than two: the two
// services, plus the superuser, plus the owner of the tables, plus
// PostgreSQL's pg_write_all_data. None of the last three is a grant and
// none of them describes a service.
//
// Left in, it would have rebuilt the defect this phase removes.
// schema_admin owns every table *and* is a role a component connects as
// - upgrader.example.toml carries schema_admin_dsn - so the day anything
// running as the owner writes a heartbeat row, that row is a silent
// address writer that can never hold a key, and full mode is unreachable
// again for a reason nobody could find from the page.
//
// Measured with real roles, because that is the only place the
// distinction exists: rolsuper, rolcanlogin and relowner are properties
// of a live cluster.
func TestOnlyTheAddressWritersHaveToHoldAKey(t *testing.T) {
	beats := newHeartbeatFixture(t)
	store := newTestStoreAt(t, "tokenkey-filter", beats.dsn)
	beats.reaches(t, store.Pool())
	ctx := context.Background()

	// A deployment that is ready: the one writer that has started holds a
	// key.
	beats.Silence()
	beats.says(testdb.Collector, heartbeat.TokenKeyPresent)
	if err := store.tokenKeyReady(ctx); err != nil {
		t.Fatalf("the baseline this test varies from is already refused: %v", err)
	}

	// Each of these reports nothing, which is the refusing state for a
	// writer - so if any of them counted as one, full mode would be
	// refused from here on.
	for _, role := range []struct {
		name string
		why  string
	}{
		{testdb.Reader, "the read API writes no addresses; it holds SELECT and no INSERT"},
		{testdb.Panel, "the panel cannot touch the analytics tables at all"},
		{testdb.SchemaAdmin, "the owner holds INSERT by owning the table, not by being granted it, " +
			"and it is a role cmd/upgrader connects as"},
		{"postgres", "a superuser holds every privilege on every table, including tables it " +
			"has never heard of"},
		{"pg_write_all_data", "a predefined role cannot log in, so no service is it"},
		{"role-that-no-longer-exists", "a heartbeat row can outlive its role"},
	} {
		beats.says(role.name, heartbeat.TokenKeyUnknown)
		if err := store.tokenKeyReady(ctx); err != nil {
			t.Errorf("a silent heartbeat row for %q made full mode unreachable: %v\n%s",
				role.name, err, role.why)
		}
		beats.Forget(role.name)
	}

	// And the filter is not a way to be excused. A row that *says*
	// something is counted whatever role wrote it - so a collector
	// misconfigured to run as the owner is still held to the rule, which
	// is the direction the owner-exclusion above must not weaken.
	beats.says(testdb.SchemaAdmin, heartbeat.TokenKeyAbsent)
	err := store.tokenKeyReady(ctx)
	if !errors.Is(err, ErrPreconditionUnmet) {
		t.Fatalf("a service reporting 'absent' was excused because of the role it "+
			"connects as: %v", err)
	}
	if !strings.Contains(err.Error(), testdb.SchemaAdmin) {
		t.Errorf("the refusal does not name it: %v", err)
	}
}

// TestTheReportsAreOrderedBySoTheSentenceIsStable.
//
// heartbeat.Read orders by beat time, which is right for a health page
// and arbitrary for a message: two services would be named in whichever
// order they last wrote, so the same deployment would produce two
// different sentences and a screenshot in a support thread would not
// match the page. Ordering is asserted here rather than described,
// because nothing else would notice it changing.
func TestTheReportsAreOrderedBySoTheSentenceIsStable(t *testing.T) {
	beats := newHeartbeatFixture(t)
	store := newTestStoreAt(t, "tokenkey-order", beats.dsn)
	beats.reaches(t, store.Pool())
	ctx := context.Background()

	// The two orders have to disagree, or the assertion below passes
	// whichever one the code uses.
	//
	// A mutation showed the first version did not: it wrote the collector
	// and then the beacon, and heartbeat.Read orders by beat time
	// *descending* - so the newest-first order was beacon, collector,
	// which is also alphabetical. Deleting the sort changed nothing, and
	// the test's own comment claimed it had avoided exactly that. Two
	// sources that give the same answer cannot measure which one was
	// read.
	//
	// So the beacon is written first and the collector second, putting
	// the collector at the front by beat time and at the back by name.
	beats.Silence()
	beats.says(testdb.Beacon, heartbeat.TokenKeyAbsent)
	beats.says(testdb.Collector, heartbeat.TokenKeyAbsent)

	// And the premise is verified rather than assumed. If both rows
	// landed on the same timestamp - one transaction, a coarse clock -
	// the two orders would agree again and this test would be measuring
	// nothing.
	raw, err := heartbeat.Read(ctx, store.Pool())
	if err != nil {
		t.Fatal(err)
	}
	var byBeat []string
	for _, b := range raw {
		byBeat = append(byBeat, b.Service)
	}
	if len(byBeat) < 2 || byBeat[0] != testdb.Collector {
		t.Fatalf("heartbeat.Read returned %v; this test needs the collector first by beat "+
			"time and last by name, so that the two orders disagree", byBeat)
	}

	reports, err := store.TokenKeyReports(ctx)
	if err != nil {
		t.Fatalf("TokenKeyReports: %v", err)
	}
	var names []string
	for _, r := range reports {
		names = append(names, r.Service)
	}
	want := []string{testdb.Beacon, testdb.Collector}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("reports came back as %v, want %v (by service name, not by beat time)",
			names, want)
	}
}
