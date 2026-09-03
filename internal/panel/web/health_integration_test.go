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
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/preflight"
	"github.com/cruciblelab/crucible-analytic/internal/profile"
	"github.com/cruciblelab/crucible-analytic/internal/rangerefresh"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

const healthSite = "saglik-testi"

// healthServer returns a running panel with an owner signed in.
func healthServer(t *testing.T) (*httptest.Server, *http.Client, *panel.Store) {
	t.Helper()
	return healthServerTweaked(t, nil)
}

// healthServerTweaked is healthServer with one hook, for the tests that
// need a panel configured differently before it starts serving.
//
// A hook rather than more parameters: the one caller that uses it wants
// a field no other test has an opinion about, and threading that field
// through every call site would make five tests state a preference they
// do not have.
func healthServerTweaked(t *testing.T, tweak func(*Server)) (*httptest.Server, *http.Client, *panel.Store) {
	t.Helper()

	srv, store := setupTestServer(t)
	if tweak != nil {
		tweak(srv)
	}
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

// TestTheDatasetSectionPollsOnlyWhileARefreshIsRunning.
//
// The same shape as the upgrade section's test, for the operation that
// actually takes time. An upgrade is 40-640 ms measured; a dataset
// refresh downloads and parses the IP range files, which is tens of
// seconds. That is the one somebody walks away from, and until this it
// was also the one you could only see finish by reloading.
//
// The unanswered case is the interesting half. asn_lookup ships off, so
// a refresh request nobody claims is an ordinary outcome - and polling
// forever on a request that will never be claimed is precisely the
// failure a stop condition exists to prevent.
func TestTheDatasetSectionPollsOnlyWhileARefreshIsRunning(t *testing.T) {
	srv, client, _ := healthServer(t)
	admin := testdb.Admin(t)
	ctx := context.Background()

	clear := func() {
		_, _ = admin.Exec(context.Background(),
			`DELETE FROM ip_range_refresh_requests WHERE actor_label = 'test-actor'`)
	}
	clear()
	t.Cleanup(clear)

	seed := func(state string, claimed bool) {
		t.Helper()
		clear()
		claimedAt := "NULL"
		if claimed {
			claimedAt = "now()"
		}
		if _, err := admin.Exec(ctx, `
			INSERT INTO ip_range_refresh_requests (actor_kind, actor_label, state, claimed_at)
			VALUES ('test', 'test-actor', $1, `+claimedAt+`)`, state); err != nil {
			t.Fatalf("could not seed a %s refresh request: %v", state, err)
		}
	}

	// Claimed and running: it polls.
	seed("running", true)
	_, body := get(t, client, srv.URL+HealthPath)
	// All three attributes, not just the trigger. A mutation that
	// removed hx-get and hx-select and left hx-trigger behind passed a
	// version of this test that only looked for the trigger - a section
	// that fires every five seconds and has nowhere to fetch from and
	// nothing to swap.
	for _, want := range []string{`id="kaynak-durumu"`, "hx-get", "hx-select", "hx-trigger"} {
		if !strings.Contains(body, want) {
			t.Errorf("a dataset refresh is running and the section does not carry %q, "+
				"so the page sits unchanged for the tens of seconds the parse takes", want)
		}
	}

	// Finished: it stops.
	seed("succeeded", true)
	_, body = get(t, client, srv.URL+HealthPath)
	if strings.Contains(body, "hx-trigger") {
		t.Error("the page still polls after the refresh finished; the stop condition " +
			"is what keeps a poll from outliving the thing it was watching")
	}
}

// TestTheHealthPageShowsTheSetupChecks.
//
// # The gap this closes, measured
//
// preflight's checks were rendered in exactly two templates, both under
// /kurulum/. An installation whose installer finished the wizard never
// saw them again - so a check added in a later version was a check every
// existing customer was invisible to, permanently. That is the shape of
// the customer's question: what happens to somebody who already
// installed, when a new build adds something to the wizard.
//
// # Why derived and not notified
//
// A notification would have to be created when the build changes,
// delivered to the right people, marked read, and cleaned up - four
// places to be wrong, and each of them needs its own state. An unmet
// check is true until it is not. This test asserts the property that
// makes that work: what the page shows comes from running the checks
// now, not from anything recorded earlier.
func TestTheHealthPageShowsTheSetupChecks(t *testing.T) {
	srv, client, _ := healthServer(t)

	status, body := get(t, client, srv.URL+HealthPath)
	if status != http.StatusOK {
		t.Fatalf("the health page answered %d", status)
	}

	if !strings.Contains(body, "Kurulum kontrolleri") {
		t.Fatalf("the health page has no setup-check section, so every check this "+
			"build ships is invisible to an installation that finished the wizard:\n%s",
			lastHealthLines(body))
	}

	// The count of passing checks, which is what stops an empty list
	// from being read as "nothing ran". A test server has a Preflight
	// checker, so at least one check has to have produced a verdict.
	if !strings.Contains(body, "Geçen kontrol") {
		t.Error("the section does not say how many checks passed; an empty list and a " +
			"list nobody ran look identical without it")
	}

	// And a passing check is a count, never a row. The checklist partial
	// renders each row's status as durum-<status>, so a satisfied check
	// that reached the list would say so here.
	//
	// Found by a mutation: dropping the `continue` that skips passes
	// broke no test, and the page would have grown twenty green lines -
	// on the one page somebody opens because they suspect something is
	// wrong, which is where a wall of green is worst.
	if strings.Contains(body, "durum-pass") {
		t.Error("a satisfied check was drawn as a row; passes are a number here, and a " +
			"page of green lines is a page people stop reading")
	}
}

// lastHealthLines trims a page down to something readable in a failure.
func lastHealthLines(body string) string {
	lines := strings.Split(body, "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	return strings.Join(lines, "\n")
}

// TestTheHealthPageDoesNotProbeServicesOverHTTP.
//
// # The measurement that produced this test
//
// preflight.Run probes each configured /healthz in turn, five second
// client timeout each. Worst of three runs against the real database:
//
//	no service URLs                17 ms
//	one service refusing the port   2 ms
//	one service blackholed       5,007 ms
//	two services blackholed     10,011 ms
//
// The wizard can afford that - once, at install, with somebody waiting
// for exactly that answer. This page cannot: it renders on every load,
// T1 re-renders it every five seconds while an upgrade runs, and the
// checks are gathered before the other sections, so a blocked probe
// delays the services, schema and storage sections too. "Each section
// fails on its own" is the whole design of this page.
//
// The trade is not about speed. This page already answers "is that
// service alive" from heartbeat rows, and answers it better: /healthz
// says the process is up right now, a heartbeat row says the last write
// succeeded and when.
//
// A blocking server rather than a blackholed address, because a test
// that depends on how this machine's network drops packets is a test
// that fails somewhere else for reasons that are not about this code.
func TestTheHealthPageDoesNotProbeServicesOverHTTP(t *testing.T) {
	release := make(chan struct{})
	probed := make(chan string, 4)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probed <- r.URL.Path
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		slow.Close()
	})

	srv, client, _ := healthServerTweaked(t, func(s *Server) {
		s.PreflightConfig.ServiceURLs = map[string]string{"yavas": slow.URL + "/healthz"}
	})

	status, body := get(t, client, srv.URL+HealthPath)

	if status != http.StatusOK {
		t.Fatalf("the health page answered %d", status)
	}
	// The checks themselves must still have run - otherwise this test
	// would pass on a page that dropped the whole section.
	if !strings.Contains(body, "Kurulum kontrolleri") {
		t.Fatalf("the setup-check section is gone, so this test is measuring nothing:\n%s",
			lastHealthLines(body))
	}

	// The request either happened or it did not, and this says which.
	//
	// No duration is compared. An earlier draft also asserted the page
	// came back inside three seconds, which measured how fast this
	// machine is rather than what the code does - and it could not fail
	// without this line failing first.
	select {
	case path := <-probed:
		t.Errorf("the health page made an HTTP request to a configured service (%s).\n"+
			"One unreachable service costs five seconds here, and this page re-renders "+
			"itself every five seconds while an upgrade is running", path)
	default:
	}
}

// TestAWedgedDatabaseDoesNotHoldTheWholeHealthPage.
//
// The page's rule is that every section fails on its own. The setup
// checks are the section most able to break it: sixteen queries, and
// they are gathered before the services, schema and storage sections.
//
// Without a deadline those sixteen queries against an unreachable
// database hold the request until the server's own sixty second write
// timeout, and the reader gets nothing at all - on the page they opened
// precisely because something is wrong. With one, they get every section
// that did not depend on the stuck connection.
//
// The wedged database is a listener that accepts connections and then
// says nothing, which is the failure that hangs rather than the one that
// returns an error. A refused port would prove nothing: that fails fast
// on its own.
//
// # Why the client carries the verdict
//
// The failure this catches is a page that never arrives, so there is
// nothing to compare a duration against - the first draft measured the
// elapsed time and never reached that line, because the request was
// still blocked. Verified by mutation: with the deadline removed the
// test did not fail, it *stopped*, for over five minutes, and the stack
// trace named web.(*Server).healthChecks inside preflight.Run.
//
// A test whose only failure mode is a hang is a test that reads as
// "still running" - the third time this session that something silently
// not-failing looked like something passing. So the client is given a
// deadline instead, and the hang becomes a sentence.
func TestAWedgedDatabaseDoesNotHoldTheWholeHealthPage(t *testing.T) {
	const budget = 400 * time.Millisecond

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Held, never answered, never closed. Closing here would
			// turn this into a fast failure and the test would pass
			// without the deadline doing anything.
			t.Cleanup(func() { conn.Close() })
		}
	}()

	stuck, err := pgxpool.New(context.Background(),
		"postgres://nobody@"+ln.Addr().String()+"/nothing?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(stuck.Close)

	srv, client, store := healthServerTweaked(t, func(s *Server) {
		s.Preflight = preflight.New(stuck, false)
		s.HealthCheckBudget = budget
	})
	writeBeat(t, store, "saglik-tikali", time.Now().Add(-time.Minute), nil, nil)

	// Ten budgets. Not a measurement of this machine: the correct page
	// does not wait on the stuck pool at all, and the failure it stands
	// in for is unbounded. It exists so an unbounded wait is reported
	// rather than waited out.
	client.Timeout = 10 * budget

	resp, err := client.Get(srv.URL + HealthPath)
	if err != nil {
		t.Fatalf("the health page never arrived while the check database was unreachable, "+
			"so a stuck check run holds the whole page: %v", err)
	}
	body := readBody(t, resp)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the health page answered %d while the check database was unreachable",
			resp.StatusCode)
	}

	// The sections that do not depend on the stuck pool are still there.
	// This is the assertion the deadline exists for.
	if !strings.Contains(body, "saglik-tikali") {
		t.Errorf("the services section is missing, so a stuck check run took the rest of "+
			"the page with it:\n%s", lastHealthLines(body))
	}
	if !strings.Contains(body, "traffic_snapshots") {
		t.Error("the storage section is missing")
	}
}
