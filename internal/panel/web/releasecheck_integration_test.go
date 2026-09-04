//go:build integration

// The four things the section can say about new versions.
//
// # Why four and not two
//
// A boolean would collapse the two states that matter most into one
// sentence. "We have never asked" and "we asked and you are current"
// both render as "no update available" - and the first is what every
// deployment with a misconfigured upgrader looks like, forever, with
// nothing anywhere saying so.
//
// The third is worse. A deployment whose last check failed still has an
// older good answer, and reporting that as "up to date" is the single
// sentence this section must never print by accident: it is exactly what
// a release host taken offline by somebody would produce.

package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/buildinfo"
	"github.com/cruciblelab/crucible-analytic/internal/relupdate"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

const releaseCheckSite = "surum-kontrol"

// clearAvailable removes the recorded answer so each case starts from a
// deployment that has never been asked.
//
// Through schema_admin, because that is the role the policy grants
// writes to - the panel's role deliberately cannot touch this table, and
// a cleanup that used it would silently do nothing.
func clearAvailable(t *testing.T) {
	t.Helper()
	admin := testdb.Pool(t, testdb.SchemaAdmin)
	if _, err := admin.Exec(context.Background(),
		`DELETE FROM panel_release_available`); err != nil {
		t.Fatalf("clearing the recorded version: %v", err)
	}
}

// TestThePanelCannotWriteWhatTheUpgraderFound.
//
// The load-bearing permission. If the panel could write here it could
// tell itself that a version exists, and the entire reason the upgrader
// does the asking is that the upgrader is the process holding the
// public key.
//
// Asserted against the database rather than trusted to the Go code
// never calling an insert, because "no caller does this" is a property
// of today's callers.
func TestThePanelCannotWriteWhatTheUpgraderFound(t *testing.T) {
	panelPool := testdb.Pool(t, testdb.Panel)
	_, err := panelPool.Exec(context.Background(), `
		INSERT INTO panel_release_available (id, version, checked_at, succeeded_at)
		VALUES (1, 'v99.0.0', now(), now())
		ON CONFLICT (id) DO UPDATE SET version = 'v99.0.0'`)
	if err == nil {
		t.Fatal("the panel's role wrote the available-version row. A panel that can " +
			"write here can tell itself a version exists, which is the one thing the " +
			"upgrader holding the key was supposed to prevent")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "policy") &&
		!strings.Contains(strings.ToLower(err.Error()), "permission") &&
		!strings.Contains(strings.ToLower(err.Error()), "denied") {
		t.Errorf("the write was refused, but not by a permission: %v.\n"+
			"A refusal for some other reason would disappear the moment that reason "+
			"changed", err)
	}
}

// TestTheSectionDistinguishesNeverAskedFromUpToDate.
func TestTheSectionDistinguishesNeverAskedFromUpToDate(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	testdb.Lock(t, testdb.Pool(t, testdb.SchemaAdmin), testdb.ReleaseQueueLock)
	t.Cleanup(func() { clearAvailable(t) })

	server, client, _ := signedInOwner(t, srv, store, releaseCheckSite, "kontrol-sahip")
	admin := testdb.Pool(t, testdb.SchemaAdmin)
	ctx := context.Background()

	// ---- never asked ----
	clearAvailable(t)
	_, body := get(t, client, server.URL+HealthPath)
	never := sectionOf(body)
	if !strings.Contains(never, "Henüz bakılmadı") {
		t.Errorf("a deployment that has never been checked does not say so:\n%s", never)
	}

	// ---- asked, and current ----
	current := runningVersion(srv)
	if err := relupdate.RecordAvailable(ctx, admin,
		relupdate.Manifest{Version: current}, time.Now()); err != nil {
		t.Fatal(err)
	}
	_, body = get(t, client, server.URL+HealthPath)
	uptodate := sectionOf(body)
	if strings.Contains(uptodate, "Henüz bakılmadı") {
		t.Error("a checked deployment still says it has never been checked")
	}
	if !strings.Contains(uptodate, "En yeni sürümü kullanıyorsunuz") {
		t.Errorf("a current deployment does not say so:\n%s", uptodate)
	}
	if never == uptodate {
		t.Error("the two states render identically, which is the collapse this test exists " +
			"to prevent")
	}
}

// TestABehindDeploymentNamesTheVersionAndPrefillsIt.
func TestABehindDeploymentNamesTheVersionAndPrefillsIt(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	testdb.Lock(t, testdb.Pool(t, testdb.SchemaAdmin), testdb.ReleaseQueueLock)
	t.Cleanup(func() { clearAvailable(t) })

	server, client, _ := signedInOwner(t, srv, store, releaseCheckSite, "kontrol-geride")
	if err := relupdate.RecordAvailable(context.Background(),
		testdb.Pool(t, testdb.SchemaAdmin),
		relupdate.Manifest{Version: "v99.0.0"}, time.Now()); err != nil {
		t.Fatal(err)
	}

	_, body := get(t, client, server.URL+HealthPath)
	section := sectionOf(body)
	if !strings.Contains(section, "v99.0.0") {
		t.Errorf("the section does not name the newer version:\n%s", section)
	}
	if !strings.Contains(section, "yeni sürüm var") {
		t.Errorf("the section does not say a newer version exists:\n%s", section)
	}
	// The whole point of the check: the field is filled in, so nobody
	// has to know a version number.
	if !strings.Contains(section, `value="v99.0.0"`) {
		t.Errorf("the version field was not prefilled with the version the upgrader "+
			"verified. That prefill is the reason this phase exists:\n%s", section)
	}
}

// TestAFailedCheckNeverReadsAsUpToDate.
//
// The sentence that must not be printed by accident, and the reason
// releaseCheck puts the failing case first.
func TestAFailedCheckNeverReadsAsUpToDate(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	testdb.Lock(t, testdb.Pool(t, testdb.SchemaAdmin), testdb.ReleaseQueueLock)
	t.Cleanup(func() { clearAvailable(t) })

	server, client, _ := signedInOwner(t, srv, store, releaseCheckSite, "kontrol-hata")
	admin := testdb.Pool(t, testdb.SchemaAdmin)
	ctx := context.Background()

	// A good answer first, so the failure has something older to sit
	// beside - which is the case that would otherwise read as "current".
	current := runningVersion(srv)
	if err := relupdate.RecordAvailable(ctx, admin,
		relupdate.Manifest{Version: current}, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := relupdate.RecordCheckFailure(ctx, admin,
		"dial tcp: connection refused", time.Now()); err != nil {
		t.Fatal(err)
	}

	_, body := get(t, client, server.URL+HealthPath)
	section := sectionOf(body)
	if strings.Contains(section, "En yeni sürümü kullanıyorsunuz") {
		t.Errorf("a deployment whose last check failed is being told it is up to date. "+
			"That sentence is what a release host somebody took offline would "+
			"produce:\n%s", section)
	}
	if !strings.Contains(section, "ulaşılamıyor") {
		t.Errorf("the section does not say the source could not be reached:\n%s", section)
	}
	// And the older answer survives, because a failed check is not
	// evidence that the previous answer became untrue.
	if !strings.Contains(section, current) {
		t.Errorf("the last good answer was dropped when a check failed:\n%s", section)
	}
}

// TestBehindAndUnreachableSaysBothThings.
//
// The state the first round of mutations found nothing to pin. Moving
// the failing case below "behind" in releaseCheck left every test green:
// the failure only becomes visible when a deployment is behind *and*
// the source is unreachable, and no case covered that pair.
//
// It is the pair that matters most. A source that published v99 and
// then went down leaves a page that can either say "a newer version
// exists" - true, and silently three days old - or say that plus "we
// cannot reach the source right now". The second is the one that lets
// somebody judge whether to act on it.
func TestBehindAndUnreachableSaysBothThings(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	testdb.Lock(t, testdb.Pool(t, testdb.SchemaAdmin), testdb.ReleaseQueueLock)
	t.Cleanup(func() { clearAvailable(t) })

	server, client, _ := signedInOwner(t, srv, store, releaseCheckSite, "kontrol-ikisi")
	admin := testdb.Pool(t, testdb.SchemaAdmin)
	ctx := context.Background()

	if err := relupdate.RecordAvailable(ctx, admin,
		relupdate.Manifest{Version: "v99.0.0"}, time.Now().Add(-72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := relupdate.RecordCheckFailure(ctx, admin,
		"dial tcp: connection refused", time.Now()); err != nil {
		t.Fatal(err)
	}

	_, body := get(t, client, server.URL+HealthPath)
	section := sectionOf(body)

	if !strings.Contains(section, "ulaşılamıyor") {
		t.Errorf("a deployment that is behind and cannot reach the source does not say "+
			"the second part. The version it is being offered is three days old and "+
			"nothing on the page says so:\n%s", section)
	}
	if !strings.Contains(section, "v99.0.0") {
		t.Errorf("the known newer version was dropped:\n%s", section)
	}
}

// sectionOf cuts the release section out of the page, so a failure
// message is the section rather than forty kilobytes of health page.
func sectionOf(body string) string {
	i := strings.Index(body, `id="surum-durumu"`)
	if i < 0 {
		return "(the release section is not on the page)"
	}
	rest := body[i:]
	if j := strings.Index(rest, "</section>"); j > 0 {
		return rest[:j]
	}
	return rest
}

// runningVersion is the version string the panel reports for itself.
//
// Read the same way release.go reads it rather than assembled here: a
// test that computed the "current" version by its own route would pass
// while the page showed a different one, which is the exact shape of
// defect this whole file is about.
func runningVersion(srv *Server) string {
	return buildinfo.Version(srv.Renderer.Version)
}
