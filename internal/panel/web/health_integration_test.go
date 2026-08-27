//go:build integration

// The health page against a real database.
//
// The unit tests hold the catalog and the page's types together. What
// only a live run can show is the property the page exists for: that a
// section which cannot be filled says so where it would have been,
// while the others still render.

package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

const healthSite = "saglik-testi"

// healthServer returns a running panel with an owner signed in.
func healthServer(t *testing.T) (*httptest.Server, *http.Client, *panel.Store) {
	t.Helper()

	srv, store := setupTestServer(t)
	ctx := context.Background()

	owner := makeUser(t, store, "saglik-sahip", false)
	if err := store.AddMember(ctx, healthSite, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatalf("AddMember: %v", err)
	}
	admin := testdb.Admin(t)
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(),
			`DELETE FROM service_heartbeat WHERE version LIKE 'saglik-%'`); err != nil {
			t.Logf("clearing the test heartbeat rows: %v", err)
		}
	})

	server := httptest.NewServer(srv.Handler())
	t.Cleanup(server.Close)
	return server, signedIn(t, server.URL, owner.Email), store
}

// writeBeat puts a row in the heartbeat table as the collector.
//
// Through the reporter rather than by hand, so what the page reads is
// what a service actually writes. A test that inserted its own row could
// pass while the reporter wrote something else.
//
// On the collector's own connection, not the panel's. The heartbeat
// table's row-level policy keys writes on current_user, so the row's
// service name *is* the connecting role - and the page draws that name.
// Written through the panel's pool this reported panel_user, which is
// the panel telling itself it is alive rather than the collector saying
// so. It only ever looked right because both suites used to connect as
// the same role.
func writeBeat(t *testing.T, store *panel.Store, version string, started time.Time,
	counters map[string]int64, note error) {
	t.Helper()

	r := heartbeat.New(heartbeat.Options{
		Pool:    testdb.Pool(t, testdb.Collector),
		Version: version,
		Started: started,
		Counters: func() map[string]int64 {
			return counters
		},
	})
	r.Note(note)
	// Run writes once immediately and then waits out its interval, so a
	// cancelled context after the first beat is exactly one row.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := store.Pool().QueryRow(context.Background(),
			`SELECT count(*) FROM service_heartbeat WHERE version = $1`, version).Scan(&n); err == nil && n == 1 {
			cancel()
			<-done
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatalf("the heartbeat row for %q never appeared", version)
}

// TestTheHealthPageShowsAServiceAndItsFailure.
func TestTheHealthPageShowsAServiceAndItsFailure(t *testing.T) {
	server, client, store := healthServer(t)

	writeBeat(t, store, "saglik-v1", time.Now().Add(-3*time.Hour),
		map[string]int64{heartbeat.CounterWritten: 12345, heartbeat.CounterDropped: 9},
		errTestWriteFailed)

	status, body := get(t, client, server.URL+HealthPath)
	if status != http.StatusOK {
		t.Fatalf("the health page answered %d", status)
	}

	for _, want := range []string{
		"saglik-v1",             // the build
		"12.345",                // written, in Turkish digit grouping
		"Düşürülen",             // the counter that means data was lost
		"copy rows: disk full",  // the last error, verbatim
		"Toplayıcı (collector)", // the role, as a name
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}

	// The panel's own row is drawn even though it writes no heartbeat.
	if !strings.Contains(body, "Bu sayfayı gösteren süreç") {
		t.Error("the panel does not report itself")
	}

	// And no visitor number reached the page. The strings below are what
	// a traffic section would say; none of them belongs here.
	for _, forbidden := range []string{"Ziyaretçi", "Görüntüleme", "Hemen çıkma"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the health page shows %q, which is a number about visitors", forbidden)
		}
	}
}

var errTestWriteFailed = &testError{"copy rows: disk full"}

type testError struct{ s string }

func (e *testError) Error() string { return e.s }

// TestASectionThatCannotBeFilledDoesNotTakeThePageDown is the rule the
// page is built around.
//
// The read API is deliberately unset on the test server, so its section
// has nothing to report - and the other two must still render. A health
// page whose sections all go dark together says nothing at the moment it
// is needed, which is the only moment it is read.
func TestASectionThatCannotBeFilledDoesNotTakeThePageDown(t *testing.T) {
	server, client, store := healthServer(t)
	writeBeat(t, store, "saglik-v2", time.Now().Add(-time.Minute), nil, nil)

	status, body := get(t, client, server.URL+HealthPath)
	if status != http.StatusOK {
		t.Fatalf("the health page answered %d with no read API configured", status)
	}

	// The API section says what is wrong with itself...
	if !strings.Contains(body, "adresi tanımlı değil") && !strings.Contains(body, "ulaşılamadı") {
		t.Error("the read API section reports neither an unset address nor a failure")
	}
	// ...and the other two are still there.
	if !strings.Contains(body, "saglik-v2") {
		t.Error("the services section is missing")
	}
	if !strings.Contains(body, "traffic_snapshots") {
		t.Error("the storage section is missing")
	}
}

// The storage section reports sizes rather than row counts, and says so.
// The panel cannot count rows in these tables, which is the isolation
// working - and a page showing a size where a reader expects a count
// would be quietly misleading without the sentence.
func TestTheStorageSectionSaysWhatItIsNotShowing(t *testing.T) {
	server, client, store := healthServer(t)
	writeBeat(t, store, "saglik-v3", time.Now(), nil, nil)

	_, body := get(t, client, server.URL+HealthPath)
	for _, want := range []string{
		"traffic_snapshots",
		"beacon_events",
		"disk kullanımıdır, satır sayısı değil",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the storage section is missing %q", want)
		}
	}

	// And the claim in that sentence is true. Asked as a privilege
	// question about panel_user rather than by trying it on this
	// connection: the suite connects as collector, which legitimately
	// holds SELECT on traffic_snapshots, so a failed SELECT here would
	// have proved nothing about the role the sentence is about.
	//
	// The first version of this assertion did exactly that and failed
	// against correct code - the test was measuring its own connection.
	for _, table := range []string{"traffic_snapshots", "beacon_events"} {
		var allowed bool
		if err := store.Pool().QueryRow(context.Background(),
			`SELECT has_table_privilege('panel_user', $1, 'SELECT')`, table).Scan(&allowed); err != nil {
			t.Fatal(err)
		}
		if allowed {
			t.Errorf("panel_user can read %s; the sentence the page shows about it is now false", table)
		}
	}
}

// TestWhoReachesTheHealthPage.
//
// An owner and a developer, and nobody else. The developer is allowed
// here and refused on the mail page, and the difference is what each
// hands over: mail configuration is close to becoming any user, and a
// byte count is a byte count.
func TestWhoReachesTheHealthPage(t *testing.T) {
	srv, store := setupTestServer(t)
	ctx := context.Background()

	owner := makeUser(t, store, "saglik-yetki-sahip", false)
	if err := store.AddMember(ctx, healthSite, owner.ID, panel.RoleOwner, nil); err != nil {
		t.Fatal(err)
	}
	admin := makeUser(t, store, "saglik-yetki-yonetici", false)
	if err := store.AddMember(ctx, healthSite, admin.ID, panel.RoleAdmin, nil); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	// dbOwner, not admin: `admin` is already a panel user in this test,
	// and the two meanings of the word are a page apart.
	dbOwner := testdb.Admin(t)
	t.Cleanup(func() {
		if _, err := dbOwner.Exec(context.Background(),
			`DELETE FROM service_heartbeat WHERE version LIKE 'saglik-%'`); err != nil {
			t.Logf("clearing the test heartbeat rows: %v", err)
		}
	})

	if status, _ := get(t, signedIn(t, server.URL, owner.Email), server.URL+HealthPath); status != http.StatusOK {
		t.Errorf("the owner got %d from the health page, want 200", status)
	}
	if status, _ := get(t, signedIn(t, server.URL, admin.Email), server.URL+HealthPath); status != http.StatusForbidden {
		t.Errorf("an admin got %d from the health page, want 403", status)
	}

	// A developer session, obtained the only way one can be.
	liveToken, liveReq := requestAccess(t, store, "saglik-yetki")
	if err := store.ApproveDevAccess(ctx, liveReq.ID, owner); err != nil {
		t.Fatal(err)
	}
	dev := newClient(t, server.URL)
	if status, _ := get(t, dev, server.URL+DevAccessPathPrefix+liveToken); status != http.StatusSeeOther {
		t.Fatalf("redeeming the developer link answered %d", status)
	}
	if status, _ := get(t, dev, server.URL+HealthPath); status != http.StatusOK {
		t.Errorf("a developer got %d from the health page, want 200 - this is their diagnostic tool", status)
	}
}
