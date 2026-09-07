//go:build integration

package beacon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/browsertest"
	"github.com/cruciblelab/crucible-analytic/internal/privacy"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// The visitor's own switch, driven by a real browser against a real
// server writing to a real database.
//
// # Why none of this can be checked from Go alone
//
// Every claim P1 makes is about what a browser does. The opt-out lives
// in localStorage, which only a browser has; the three calls hang off
// window.crucible, which only a browser defines; and the thing being
// asserted is a *negative* - that navigating writes nothing - which a
// unit test can only prove about a function it called itself.
//
// # The defect this phase was written against
//
// The flag was already read. What it did was return out of the whole
// script before window.crucible was defined, which meant a visitor who
// had opted out had no optIn to call and a consent banner asking
// status() got a TypeError. The feature that existed could be entered
// and not left.
//
// *Bir ayarın var olması, ziyaretçinin ona ulaşabildiği anlamına
// gelmez.*
func TestTheVisitorSwitchInARealBrowser(t *testing.T) {
	if os.Getenv("CA_BROWSER_TEST") == "" {
		t.Skip("set CA_BROWSER_TEST=1 to run this; it needs node, playwright and a chromium build")
	}

	const site = "p1-tarayici"
	writer := newTestWriter(t, site, WriterConfig{FlushInterval: 50 * time.Millisecond})

	// The writer's own goroutine, which is what turns an enqueued row
	// into a row in the table. Without it the server accepts everything,
	// its counters say so, and nothing is written - which is how this
	// test first reported zero rows against a beacon that was working
	// perfectly.
	runCtx, stopWriter := context.WithCancel(context.Background())
	written := make(chan struct{})
	go func() { defer close(written); writer.Run(runCtx) }()

	srv := &Server{
		Sites:     []string{site},
		Sink:      writer,
		Visitors:  newTestVisitorIDs(t),
		IPMode:    privacy.IPMasked,
		IPHashKey: []byte("otuz-iki-baytlik-test-anahtari!!"),
	}

	// One server for both the snippet and the events, which is the
	// ordinary deployment: the script derives its endpoint from its own
	// src, so same-origin needs no configuration and no CORS.
	mux := http.NewServeMux()
	mux.Handle("/", srv.Handler())
	// A page to embed it in. Two paths, because the second navigation is
	// what a pageview needs in order to be a new one.
	page := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html><head><title>%s</title></head>`+
			`<body><h1>%s</h1>`+
			`<script src="/_ca/ca.js" data-site=%q></script></body></html>`,
			r.URL.Path, r.URL.Path, site)
	}
	mux.HandleFunc("/bir", page)
	mux.HandleFunc("/iki", page)
	mux.HandleFunc("/uc", page)
	mux.HandleFunc("/dort", page)
	// A page whose script runs with storage that throws.
	//
	// sandbox without allow-same-origin gives the frame an opaque
	// origin, and every localStorage access inside it raises a
	// SecurityError. That is not a contrived case: it is an ad frame, a
	// preview pane, and any browser set to block site data.
	mux.HandleFunc("/kum", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><body>`+
			`<iframe sandbox="allow-scripts" src="/bir"></iframe></body></html>`)
	})
	// Served rather than left to 404, so the console-error assertion can
	// stay strict. A browser asks for this on every navigation and a
	// missing one is a fact about this fixture, not about the snippet -
	// but an assertion that has to make an exception for one message is
	// an assertion that will be taught to make a second.
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.WriteHeader(http.StatusNoContent)
	})

	// Under a mutex, because a browser is not one caller.
	//
	// The first version appended to a bare slice from the handler and
	// read it from the test, and `go test -race ./...` said so:
	//
	//	WARNING: DATA RACE
	//	Read at ... by goroutine 388: ...optout_browser_test.go:109
	//	Previous write at ... by goroutine 391: ...optout_browser_test.go:109
	//
	// Chromium fetches the page, the script and the favicon on separate
	// connections, so net/http serves them on separate goroutines - and
	// the POST can land while a GET is still being written. Every
	// assertion in the test passed on the run that reported this; the
	// transcript was right and the log that produced it was undefined
	// behaviour.
	//
	// The read is guarded too, and that is the half worth naming.
	// Deferred calls run last-registered-first, so the log below runs
	// *before* server.Close() - the one call that waits for outstanding
	// handlers. Ordering the defers the other way would have fixed the
	// read and left the appends racing each other.
	var mu sync.Mutex
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		mux.ServeHTTP(w, r)
	}))
	defer server.Close()
	defer func() {
		a, d, r := srv.Counters()
		mu.Lock()
		defer mu.Unlock()
		t.Logf("requests: %v | accepted=%d dropped=%d rejected=%d", seen, a, d, r)
	}()

	// The server's own count of what it took, which separates "the
	// browser did not send it" from "the write did not land". Both look
	// like a missing row and they are opposite defects.
	defer func() {
		if accepted, _, _ := srv.Counters(); accepted != 4 {
			t.Errorf("the server accepted %d events and 4 were expected. If this "+
				"disagrees with the row count above, the fault is between the "+
				"server and the table rather than in the browser", accepted)
		}
	}()

	out, err := exec.Command("node", writeOptOutScript(t), server.URL).Output()
	if err != nil {
		t.Fatalf("browser run failed: %v", err)
	}
	t.Logf("browser transcript:\n%s", out)

	var report struct {
		ConsoleErrors []string `json:"console_errors"`

		// TypeOfCrucible is what window.crucible still is. The three
		// calls were added as properties of the existing function, and
		// "function" is the whole of that claim.
		TypeOfCrucible string `json:"type_of_crucible"`

		StatusFresh string `json:"status_fresh"`
		StatusOut   string `json:"status_out"`
		StatusIn    string `json:"status_in"`

		// OptOutStored is what optOut() reported about durability.
		OptOutStored bool `json:"opt_out_stored"`
		// StoredFlag is what is actually in localStorage afterwards, read
		// independently of what optOut claimed.
		StoredFlag string `json:"stored_flag"`
		// FlagAfterOptIn is the raw getItem result stringified, so
		// "null" means the key is gone and "" means it is present and
		// empty.
		//
		// The distinction is the whole assertion, and the first version
		// of this test could not make it: it reported
		// `getItem(...) || ''`, which is "" in both cases. A mutation
		// that set the key to an empty string instead of removing it
		// walked straight through. Our own script reads the value for
		// truthiness and would be satisfied either way - but the
		// documented manual opt-out is the key's *presence*, and
		// anything checking for that, including a site's own code, would
		// still see somebody opted out.
		FlagAfterOptIn string `json:"flag_after_opt_in"`

		// StatusSurvivesReload is whether a fresh page load on the same
		// browser still reports 'out'. Without it this test would prove
		// only that a variable was set.
		StatusSurvivesReload bool `json:"status_survives_reload"`

		// The sandboxed frame, where every localStorage access throws.
		SandboxDefined  bool   `json:"sandbox_defined"`
		SandboxStatus   string `json:"sandbox_status"`
		SandboxOptOut   string `json:"sandbox_opt_out"`
		SandboxAfterOut string `json:"sandbox_after_out"`
		SandboxThrew    string `json:"sandbox_threw"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("browser report: %v\n%s", err, out)
	}
	for _, e := range report.ConsoleErrors {
		t.Errorf("console error: %s", e)
	}

	if report.TypeOfCrucible != "function" {
		t.Errorf("window.crucible is a %q.\n"+
			"Sites embed crucible('event', 'signup') today. The three calls "+
			"were meant to hang off that function, not replace it",
			report.TypeOfCrucible)
	}
	for _, tc := range []struct{ name, got, want string }{
		{"fresh", report.StatusFresh, "in"},
		{"after optOut", report.StatusOut, "out"},
		{"after optIn", report.StatusIn, "in"},
	} {
		if tc.got != tc.want {
			t.Errorf("status() %s reported %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	if !report.OptOutStored {
		t.Errorf("optOut() reported that it could not store the choice, in a " +
			"browser with working storage. A banner reads that return value " +
			"to know whether the switch will still be there tomorrow")
	}
	if report.StoredFlag == "" {
		t.Error("optOut() set no localStorage flag. Setting it by hand has always " +
			"been the documented way to opt out and has to keep working")
	}
	if report.FlagAfterOptIn != "null" {
		t.Errorf("after optIn() localStorage still holds %q for the flag; the key "+
			"has to be gone.\n"+
			"\"\" means it is present and empty, which this script reads as "+
			"opted in and anything checking for the key's presence reads as "+
			"opted out", report.FlagAfterOptIn)
	}
	if !report.StatusSurvivesReload {
		t.Error("the choice did not survive a reload, so it is a variable rather " +
			"than a decision")
	}

	// Storage that throws, which is an ad frame, a preview pane, and any
	// browser set to block site data. The snippet has always wrapped its
	// one read in try/catch; what is new is three more entry points, and
	// the whole point of measuring is that "we wrapped it" is a claim
	// about code rather than about a browser.
	if report.SandboxThrew != "" {
		t.Errorf("in a sandboxed frame the calls threw: %s\n"+
			"A visitor's page must not break because our snippet asked for "+
			"storage it is not allowed to have", report.SandboxThrew)
	}
	if !report.SandboxDefined {
		t.Error("window.crucible is not defined in a sandboxed frame. The script " +
			"has to finish loading even where it can read nothing")
	}
	if report.SandboxStatus != "in" {
		t.Errorf("status() reported %q where storage throws, want \"in\".\n"+
			"Nothing could be read, so nobody has opted out - and reading an "+
			"unreadable store as 'opted out' would silently stop collecting "+
			"for every visitor whose browser blocks site data",
			report.SandboxStatus)
	}
	if report.SandboxOptOut != "false" {
		t.Errorf("optOut() reported %q where storage throws, want \"false\".\n"+
			"The choice cannot be persisted here and the return value is the "+
			"only thing that says so", report.SandboxOptOut)
	}
	if report.SandboxAfterOut != "out" {
		t.Errorf("after optOut() in a sandboxed frame status() said %q, want "+
			"\"out\".\nA browser that cannot remember the decision must still "+
			"honour it for the life of the page: the visitor asked now",
			report.SandboxAfterOut)
	}

	// And the measurement nothing above makes: what reached the
	// database. Three navigations happened - one before opting out, one
	// while opted out, one after opting back in - plus one custom event.
	//
	// Drained before counting: Run writes what it has buffered when its
	// context ends, so waiting for it is the difference between "nothing
	// was sent" and "we looked before it landed".
	stopWriter()
	<-written
	//
	// Four: the first navigation, the one after opting back in, the
	// custom event, and the sandboxed frame's own load. That last one is
	// not an accident to be filtered out - a frame that cannot read
	// storage has nobody opted out in it, so sending is the correct
	// behaviour and counting it is how this test would notice if the
	// unreadable case started being treated as opted out.
	const want = 4
	if got := countRows(t, site); got != want {
		t.Errorf("%d rows were written and %d were expected.\n"+
			"Two pageviews, one custom event and the sandboxed frame's load "+
			"should have been recorded, and the navigation made while opted "+
			"out should have written nothing at all", got, want)
	}
	assertNoOptedOutPath(t, site)
}

// assertNoOptedOutPath is the negative, named rather than counted.
//
// A count alone would pass if the opted-out navigation had been written
// and one of the others lost - the same total, the opposite meaning. The
// path that must not be there is asked for by name.
func assertNoOptedOutPath(t *testing.T, site string) {
	t.Helper()
	pool := testdb.Pool(t, testdb.Reader)
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM beacon_events WHERE site_id = $1 AND path = '/iki'`,
		site).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d rows were written for the page visited while opted out.\n"+
			"That navigation is the one thing this whole phase promises will "+
			"not be recorded", n)
	}
}

func writeOptOutScript(t *testing.T) string {
	t.Helper()

	const script = `
import playwright from '/opt/node22/lib/node_modules/playwright/index.js';
const { chromium } = playwright;

const [base] = process.argv.slice(2);

const browser = await chromium.launch({ executablePath: '/opt/pw-browsers/chromium' });
const report = { console_errors: [] };

// One context for the whole run, so localStorage persists across
// navigations the way it does for a real visitor. A fresh context per
// step would make the reload check meaningless.
const context = await browser.newContext();
const page = await context.newPage();
page.on('console', (m) => { if (m.type() === 'error') report.console_errors.push(m.text()); });
page.on('pageerror', (e) => report.console_errors.push(String(e)));

// The snippet posts with sendBeacon, which returns before the request is
// on the wire. Waiting for the response is what makes "no row" mean
// "none was sent" rather than "we looked too early".
const settle = async () => {
  await page.waitForTimeout(400);
};

try {

// ---- a fresh browser sends, and says so ----
await page.goto(base + '/bir');
await settle();
report.type_of_crucible = await page.evaluate(() => typeof window.crucible);
report.status_fresh = await page.evaluate(() => window.crucible.status());

// ---- opting out ----
report.opt_out_stored = await page.evaluate(() => window.crucible.optOut());
report.status_out = await page.evaluate(() => window.crucible.status());
report.stored_flag = await page.evaluate(() => localStorage.getItem('crucible.disabled') || '');

// A navigation while opted out. This is the one that must write nothing.
await page.goto(base + '/iki');
await settle();

// And the state is a decision, not a variable: a fresh document reads it
// back off storage.
report.status_survives_reload = await page.evaluate(() => window.crucible.status()) === 'out';

// A custom event while opted out must also stay silent - the gate is on
// sending, not on one entry point.
await page.evaluate(() => window.crucible('event', 'opted_out_event'));
await settle();

// ---- opting back in ----
await page.evaluate(() => window.crucible.optIn());
report.status_in = await page.evaluate(() => window.crucible.status());
// String(...) rather than a falsy fallback: getItem returns null for an
// absent key and an empty string for a present empty one, and those are
// the two answers this assertion has to tell apart.
report.flag_after_opt_in = await page.evaluate(() => String(localStorage.getItem('crucible.disabled')));

await page.goto(base + '/uc');
await settle();

// The old call still works, which is the backwards-compatibility half.
await page.evaluate(() => window.crucible('event', 'kayit'));
await settle();

// ---- storage that throws ----
// A separate page, because the frame gets its own document and its own
// script; nothing above is disturbed by it.
const outer = await context.newPage();
await outer.goto(base + '/kum');
const frame = outer.frames().find((f) => f !== outer.mainFrame());
report.sandbox_defined = await frame.evaluate(() => typeof window.crucible === 'function');
report.sandbox_status = await frame.evaluate(() => window.crucible.status());
// Stringified: a thrown error and a returned false are different
// answers and a boolean field would flatten them into one.
report.sandbox_opt_out = await frame.evaluate(() => String(window.crucible.optOut()));
report.sandbox_after_out = await frame.evaluate(() => window.crucible.status());
await frame.evaluate(() => window.crucible.optIn());
await outer.close();

} catch (e) {
  // Distinguished from console noise: a throw out of the sandbox block
  // is the thing that block exists to detect, and burying it in
  // console_errors would report it as somebody else's warning.
  report.sandbox_threw = String(e);
}

await browser.close();
process.stdout.write(JSON.stringify(report));
`

	prepared, err := browsertest.Prepare(script)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "optout.mjs")
	if err := os.WriteFile(path, []byte(prepared), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
