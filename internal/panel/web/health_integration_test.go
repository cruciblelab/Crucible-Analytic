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
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/profile"
	"github.com/cruciblelab/crucible-analytic/internal/rangerefresh"
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
	writeBeatWithProfile(t, store, version, started, counters, note, "")
}

// writeBeatWithProfile is writeBeat plus the one field the profile test
// needs. Two functions rather than one more parameter on every existing
// call site, which would have been six edits saying "no profile here".
func writeBeatWithProfile(t *testing.T, store *panel.Store, version string, started time.Time,
	counters map[string]int64, note error, prof string) {
	t.Helper()

	r := heartbeat.New(heartbeat.Options{
		Pool:    testdb.Pool(t, testdb.Collector),
		Version: version,
		Profile: prof,
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

// --- M3: the refresh button on the page ---

// TestTheRefreshButtonWorksFromThePage.
//
// M3's done criterion end to end, through the page a customer actually
// uses: the section draws, the button posts, the request lands, and the
// answer comes back on the page rather than in a log.
//
// A signed-in owner with no developer password, which is the default
// this phase promises works.
func TestTheRefreshButtonWorksFromThePage(t *testing.T) {
	server, client, store := healthServer(t)
	ctx := context.Background()
	admin := testdb.Admin(t)
	testdb.Lock(t, admin, testdb.RefreshQueueLock)
	testdb.Lock(t, admin, testdb.FetchLogLock)

	clear := func() {
		for _, table := range []string{"ip_range_refresh_requests", "ip_range_fetches"} {
			if _, err := admin.Exec(context.Background(), `DELETE FROM `+table); err != nil {
				t.Logf("cleanup: clearing %s: %v", table, err)
			}
		}
	}
	clear()
	t.Cleanup(clear)

	// A deployment that has fetched at least once, so the section is
	// drawn at all: a page that showed this panel on an installation with
	// asn_lookup off would be a permanent notice about a feature nobody
	// turned on.
	if _, err := admin.Exec(ctx, `
		INSERT INTO ip_range_fetches
		  (started_at, finished_at, source_id, kind, family, origin,
		   outcome, rows_parsed, bytes_read, error_chain)
		VALUES (now() - interval '2 days', now() - interval '2 days', 'user-country',
		        'country', 'ipv4', 'download', 'failed', 0, 0, 'HTTP 404')`); err != nil {
		t.Fatalf("seeding a fetch: %v", err)
	}

	status, body := get(t, client, server.URL+HealthPath)
	if status != http.StatusOK {
		t.Fatalf("the health page answered %d", status)
	}
	if !strings.Contains(body, shown("IP veri kümeleri")) {
		t.Fatal("the health page does not draw the dataset section, so the button " +
			"and the fetch log are both invisible")
	}
	if !strings.Contains(body, "user-country") {
		t.Error("the section does not name the dataset whose fetch failed; the " +
			"result M3 has to put on screen is which one broke")
	}
	if !strings.Contains(body, `value="kaynak_yenile"`) {
		t.Fatal("the page draws no refresh button for an owner, so the default " +
			"this phase promises does not work")
	}

	resp, after := postNoFollow(t, client, server.URL+HealthPath, url.Values{
		"csrf_token": {csrfFrom(t, body)},
		"eylem":      {"kaynak_yenile"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the button answered %d", resp.StatusCode)
	}
	if !strings.Contains(after, shown("Yenileme istendi")) {
		t.Error("the page does not confirm the press, so somebody presses again")
	}

	req, err := rangerefresh.Latest(ctx, store.Pool())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if req == nil {
		t.Fatal("the button was pressed and no request was written")
	}
	if req.State != rangerefresh.StatePending {
		t.Errorf("the request is %q, want pending", req.State)
	}

	// And pressing again says so rather than queueing a second refresh.
	resp2, again := postNoFollow(t, client, server.URL+HealthPath, url.Values{
		"csrf_token": {csrfFrom(t, after)},
		"eylem":      {"kaynak_yenile"},
	})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("the second press answered %d", resp2.StatusCode)
	}
	if !strings.Contains(again, shown("zaten sırada")) {
		t.Error("the second press does not say one is already going; a page that " +
			"answers identically teaches people to keep pressing")
	}
}

// TestAnUnknownActionIsRefused.
//
// The page carries two buttons now, and the handler dispatches on a
// named action rather than on which field happens to be present. A
// handler that guessed would be one that can be made to guess wrong.
func TestAnUnknownActionIsRefused(t *testing.T) {
	server, client, _ := healthServer(t)

	_, body := get(t, client, server.URL+HealthPath)
	resp, _ := postNoFollow(t, client, server.URL+HealthPath, url.Values{
		"csrf_token": {csrfFrom(t, body)},
		"eylem":      {"bir-sey-uydur"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an unnamed action answered %d, want 400", resp.StatusCode)
	}
}

// TestTheHealthPageShowsTheResourceProfile.
//
// # What this is checking that the unit tests cannot
//
// The chain, end to end: a collector reports a profile id through the
// heartbeat, the row survives a real database, the panel reads it back,
// resolves it to a label out of internal/profile, and draws it. Four
// packages and a table, each of which has its own tests and none of
// which can tell whether the next one is listening.
//
// The label is asserted rather than the id, because the label is what
// the customer reads. A page that showed "dengeli" would be showing an
// internal identifier to somebody who never chose one.
func TestTheHealthPageShowsTheResourceProfile(t *testing.T) {
	srv, client, store := healthServer(t)

	writeBeatWithProfile(t, store, "v-profile-page", time.Now().Add(-time.Hour),
		map[string]int64{}, nil, "dengeli")

	status, body := get(t, client, srv.URL+HealthPath)
	if status != http.StatusOK {
		t.Fatalf("the health page answered %d", status)
	}

	// The label from internal/profile, not the id and not a copy of the
	// word: a second spelling here would pass while the two drifted.
	want, ok := profile.ByID("dengeli")
	if !ok {
		t.Fatal("internal/profile no longer offers \"dengeli\"; this test names a " +
			"profile that is gone")
	}
	if !strings.Contains(body, want.Label) {
		t.Errorf("the health page does not show the reported profile %q", want.Label)
	}
	// And the column exists at all, so a page that happened to contain
	// the word somewhere else would not pass.
	if !strings.Contains(body, "Profil") {
		t.Error("the services table has no profile column")
	}
}

// TestTheUpgradeSectionPollsOnlyWhileSomethingIsRunning.
//
// # What this is for
//
// An upgrade takes minutes, and the message beside it used to say "bu
// sayfayı yenileyerek sonucu görebilirsiniz" - refresh the page yourself
// to see the result. That is an operation asking a person to keep
// pressing a key, on a page they opened because they were already
// worried about something.
//
// The section polls itself instead, and the stop condition is
// structural: the hx-trigger is rendered only while the request is in
// flight, so the swapped-in copy arrives without one and the polling
// ends by itself. Nothing has to remember to turn it off - which is the
// half worth testing, because a poll that never stops is a page that
// reloads forever on a machine nobody is watching.
//
// Measured before it was written: the panel's pages render in 2-38 ms
// and do not slow with data volume (13 ms at 50,000 rows), so a request
// every five seconds costs nothing worth counting.
func TestTheUpgradeSectionPollsOnlyWhileSomethingIsRunning(t *testing.T) {
	srv, client, _ := healthServer(t)

	// Nothing in flight: the section may or may not be shown at all,
	// but it must not be polling.
	_, body := get(t, client, srv.URL+HealthPath)
	if strings.Contains(body, "hx-trigger") {
		t.Error("the health page polls itself with no upgrade in flight; that is a " +
			"request every five seconds, forever, from every open tab")
	}

	// A request in flight. Written straight into the table rather than
	// through RequestUpgrade, which needs the developer password: this
	// test is about what the page draws for a given state, not about
	// how the state is reached.
	//
	// Fatal rather than a skip when the insert fails. It was a skip
	// first, and the first run skipped on a column name this test had
	// guessed - which reads as a pass in every summary that is not run
	// with -v. A test that cannot set up the state it is about has not
	// been skipped by circumstance; it is broken.
	if _, err := testdb.Admin(t).Exec(context.Background(), `
		INSERT INTO panel_upgrade_requests
		    (actor_kind, actor_label, state, from_version, to_version, to_fingerprint)
		VALUES ('test', 'test-actor', 'running', 7, 8, 'test-fingerprint')`); err != nil {
		t.Fatalf("could not seed a running upgrade request: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testdb.Admin(t).Exec(context.Background(),
			`DELETE FROM panel_upgrade_requests WHERE actor_label = 'test-actor'`)
	})

	_, body = get(t, client, srv.URL+HealthPath)
	for _, want := range []string{`id="yukseltme-durumu"`, "hx-trigger", "hx-select"} {
		if !strings.Contains(body, want) {
			t.Errorf("an upgrade is running and the section does not carry %q, so the "+
				"page will sit unchanged until somebody reloads it", want)
		}
	}
}
