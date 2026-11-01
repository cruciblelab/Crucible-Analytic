//go:build integration

// The restart section (B3e), drawn from real rows and pressed through the
// page's own form.
//
// Every state the section can draw is drawn here at least once, from
// heartbeat rows the reporter wrote and request rows in the real
// panel_settings: a template branch no test draws is a branch that can
// carry a 500 for as long as nobody reaches it, and this repository has
// had one for thirty-three days.

package web

import (
	"context"
	"errors"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/devgate"
	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/relupdate"
	"github.com/cruciblelab/crucible-analytic/internal/restartreq"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

// restartPage is a running panel with an owner signed in, every service's
// heartbeat row written, and the restart rows cleared at both ends.
type restartPage struct {
	t      *testing.T
	srv    *Server
	server *httptest.Server
	client *http.Client
	store  *panel.Store
	admin  *pgxpool.Pool
}

func newRestartPage(t *testing.T) *restartPage {
	t.Helper()
	srv, store := setupTestServer(t) // AccountsLock
	admin := testdb.Admin(t)
	// After AccountsLock and before healthServerOn's HeartbeatLock: the
	// order testdb declares.
	testdb.Lock(t, admin, testdb.RestartRequestLock)
	clear := func() {
		if _, err := admin.Exec(context.Background(),
			`DELETE FROM panel_settings WHERE scope = 'global' AND site_id = '' AND key LIKE $1`,
			restartreq.KeyPrefix+"%"); err != nil {
			t.Errorf("clearing the restart requests: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)

	server, client, _ := healthServerOn(t, srv, store)
	p := &restartPage{t: t, srv: srv, server: server, client: client, store: store, admin: admin}
	// Every service reports, through the reporter and as its own role:
	// the page draws what a service writes, not what a test inserted. A
	// version per role, because writeBeatDetail waits for a row with its
	// version: with one version for all four it returned after the first
	// row, before the other three had been written.
	for _, role := range relupdate.HealthServices {
		writeBeatDetail(t, store, role, "saglik-yb-"+role, time.Now().Add(-time.Hour),
			map[string]int64{}, nil, "", heartbeat.TokenKeyUnknown)
	}
	return p
}

// ran sets a service's start and last beat, relative to the database's
// now, on the row its reporter wrote.
func (p *restartPage) ran(service string, startedAgo, beatAgo time.Duration) {
	p.t.Helper()
	p.exec(`UPDATE service_heartbeat SET started_at = now() - make_interval(secs => $2),
		beat_at = now() - make_interval(secs => $3) WHERE service = $1`,
		service, startedAgo.Seconds(), beatAgo.Seconds())
}

// startedAfterRequest moves a service's start to d after its request.
func (p *restartPage) startedAfterRequest(service string, d time.Duration) {
	p.t.Helper()
	p.exec(`UPDATE service_heartbeat SET started_at = (
		  SELECT (value #>> '{}')::timestamptz + make_interval(secs => $3) FROM panel_settings
		   WHERE scope = 'global' AND site_id = '' AND key = $2)
		WHERE service = $1`, service, restartreq.Key(service), d.Seconds())
}

// asked writes a request made ago before the database's now, the shape
// the panel writes: value and updated_at the same instant.
func (p *restartPage) asked(service string, ago time.Duration) {
	p.t.Helper()
	p.exec(`INSERT INTO panel_settings (scope, site_id, key, value, updated_at)
		VALUES ('global', '', $1, to_jsonb(now() - make_interval(secs => $2)), now() - make_interval(secs => $2))
		ON CONFLICT (scope, site_id, key) DO UPDATE SET value = EXCLUDED.value, updated_at = EXCLUDED.updated_at`,
		restartreq.Key(service), ago.Seconds())
}

func (p *restartPage) silenced(service string) {
	p.t.Helper()
	p.exec(`DELETE FROM service_heartbeat WHERE service = $1`, service)
}

func (p *restartPage) exec(sql string, args ...any) {
	p.t.Helper()
	tag, err := p.admin.Exec(context.Background(), sql, args...)
	if err != nil || tag.RowsAffected() != 1 {
		p.t.Fatalf("%s: %v (%d rows)", sql, err, tag.RowsAffected())
	}
}

// requested reports whether a service has a request row.
func (p *restartPage) requested(service string) bool {
	p.t.Helper()
	var n int
	if err := p.admin.QueryRow(context.Background(), `SELECT count(*) FROM panel_settings
		WHERE scope = 'global' AND site_id = '' AND key = $1`, restartreq.Key(service)).Scan(&n); err != nil {
		p.t.Fatal(err)
	}
	return n == 1
}

// section is the restart section of the page, or the test fails.
func (p *restartPage) section() string {
	p.t.Helper()
	status, body := get(p.t, p.client, p.server.URL+HealthPath)
	if status != http.StatusOK {
		p.t.Fatalf("the health page answered %d", status)
	}
	return restartSectionOf(p.t, body)
}

func restartSectionOf(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `id="yeniden-baslatma-durumu"`)
	if start < 0 {
		t.Fatalf("the page has no restart section")
	}
	end := strings.Index(body[start:], "</section>")
	if end < 0 {
		t.Fatal("the restart section is not closed")
	}
	return body[start : start+end]
}

// restartRowOf is one service's table row in a section.
func restartRowOf(t *testing.T, section, service string) string {
	t.Helper()
	label := html.EscapeString(testLang(t).T("saglik.servis." + service))
	for _, row := range strings.Split(section, "<tr>") {
		if strings.Contains(row, `<th scope="row">`+label+`</th>`) {
			return row
		}
	}
	t.Fatalf("no row for %s (%q) in the restart section", service, label)
	return ""
}

// restartOffered lists the services the select offers - not the empty
// placeholder, which is not a service.
func restartOffered(section string) []string {
	var out []string
	for _, part := range strings.Split(section, `<option value="`)[1:] {
		if value := part[:strings.Index(part, `"`)]; value != "" {
			out = append(out, value)
		}
	}
	return out
}

// restartSays asserts a row carries a state's sentence, taken from the catalog,
// and none of the others'.
func restartSays(t *testing.T, row, service, key string) {
	t.Helper()
	lang := testLang(t)
	keys := []string{"yok", "bekliyor", "basladi", "sessiz", "yok_saydi", "duymaz", "raporsuz"}
	for _, k := range keys {
		text := lang.T("saglik.yb.durum." + k)
		if k == "basladi" {
			// "%s" stands for the time; the words before it are the state.
			text = text[:strings.Index(text, "(")]
		}
		if has := strings.Contains(row, html.EscapeString(text)); has != (k == key) {
			t.Errorf("%s's row: state %q present = %v, want %v\nrow: %s", service, k, has, k == key, row)
		}
	}
}

// TestTheRestartSectionDrawsEveryState: two configurations of real rows,
// seven states between them, each sentence in its own row and in no
// other.
func TestTheRestartSectionDrawsEveryState(t *testing.T) {
	p := newRestartPage(t)

	// First: not applied with a fresh heartbeat, not applied with a stale
	// one, restarted, and a role that cannot hear.
	p.ran(testdb.Collector, 10*time.Minute, 5*time.Second)
	p.asked(testdb.Collector, 2*time.Minute)
	p.ran(testdb.Beacon, 10*time.Minute, 4*time.Minute)
	p.asked(testdb.Beacon, 10*time.Second)
	p.ran(testdb.Panel, 10*time.Minute, 5*time.Second)
	p.asked(testdb.Panel, 30*time.Second)
	p.startedAfterRequest(testdb.Panel, 3*time.Second)

	section := p.section()
	restartSays(t, restartRowOf(t, section, testdb.Collector), testdb.Collector, "yok_saydi")
	restartSays(t, restartRowOf(t, section, testdb.Beacon), testdb.Beacon, "sessiz")
	restartSays(t, restartRowOf(t, section, testdb.Panel), testdb.Panel, "basladi")
	restartSays(t, restartRowOf(t, section, testdb.Reader), testdb.Reader, "duymaz")
	if !strings.Contains(restartRowOf(t, section, testdb.Collector), testLang(t).T("saglik.yb.durum.uygulanmadi")) {
		t.Error("the not-applied row does not say 'not applied'")
	}
	// The stale row's label names its own cause. It used to read "not
	// applied" too, beside a sentence saying the service is not running -
	// a label about the process's choice on a row where there is no
	// process to choose.
	if beacon := restartRowOf(t, section, testdb.Beacon); !strings.Contains(beacon, testLang(t).T("saglik.yb.durum.sessiz_etiket")) ||
		strings.Contains(beacon, testLang(t).T("saglik.yb.durum.uygulanmadi")) {
		t.Errorf("the stale row is not labelled 'not reporting' alone\nrow: %s", beacon)
	}
	// The read API cannot hear, so it is not offered; the other three are.
	if got, want := strings.Join(restartOffered(section), ","), "collector,beacon_writer,panel_user"; got != want {
		t.Errorf("the select offers %s, want %s", got, want)
	}
	if strings.Contains(section, "hx-trigger") {
		t.Error("the section refreshes itself with nothing waiting")
	}
	if !strings.Contains(section, testLang(t).T("saglik.yb.uyari_collector")) {
		t.Error("the form is drawn without the collector's warning")
	}
	if !strings.Contains(section, `name="`+devgate.FormField+`"`) {
		t.Errorf("the form draws no password field named %q", devgate.FormField)
	}
	// Nothing chosen for anybody: the placeholder is, and no service is.
	if !strings.Contains(section, `<option value="" selected>`) || strings.Count(section, " selected>") != 1 {
		t.Error("the select arrives with a service already chosen; the first is the collector")
	}

	// Second: waiting, never asked, never reported.
	p.ran(testdb.Collector, 10*time.Minute, 5*time.Second)
	p.asked(testdb.Collector, 5*time.Second)
	p.exec(`DELETE FROM panel_settings WHERE scope = 'global' AND site_id = '' AND key = $1`,
		restartreq.Key(testdb.Beacon))
	p.ran(testdb.Beacon, 10*time.Minute, 5*time.Second)
	p.silenced(testdb.Panel)

	section = p.section()
	restartSays(t, restartRowOf(t, section, testdb.Collector), testdb.Collector, "bekliyor")
	restartSays(t, restartRowOf(t, section, testdb.Beacon), testdb.Beacon, "yok")
	restartSays(t, restartRowOf(t, section, testdb.Panel), testdb.Panel, "raporsuz")
	if got, want := strings.Join(restartOffered(section), ","), "collector,beacon_writer"; got != want {
		t.Errorf("the select offers %s, want %s", got, want)
	}
	if !strings.Contains(section, `hx-trigger="every 5s"`) {
		t.Error("a waiting request does not make the section refresh itself")
	}
}

// TestNoServiceToOfferSaysSo: with every service unaskable the form is
// not drawn, and the section says why.
func TestNoServiceToOfferSaysSo(t *testing.T) {
	p := newRestartPage(t)
	for _, role := range []string{testdb.Collector, testdb.Beacon, testdb.Panel} {
		p.silenced(role)
	}
	section := p.section()
	if len(restartOffered(section)) != 0 {
		t.Errorf("the select offers %v with nothing askable", restartOffered(section))
	}
	if !strings.Contains(section, testLang(t).T("saglik.yb.secilecek_yok")) {
		t.Error("the section does not say there is nothing to restart")
	}
	if strings.Contains(section, `name="eylem" value="`+eylemYenidenBaslat+`"`) {
		t.Error("a form is drawn with nothing to choose")
	}
}

// TestPressingRestartWritesTheRequestAndShowsItWaiting: the form, posted
// with the fields the page draws, writes the row, its audit entry and an
// operation record pointing at it - and the section that comes back
// shows the request waiting.
func TestPressingRestartWritesTheRequestAndShowsItWaiting(t *testing.T) {
	p := newRestartPage(t)
	p.ran(testdb.Collector, 10*time.Minute, 5*time.Second)

	// The form's own action value, taken from the page.
	if !strings.Contains(p.section(), `name="eylem" value="`+eylemYenidenBaslat+`"`) {
		t.Fatalf("the form does not post the action %q the handler dispatches on", eylemYenidenBaslat)
	}

	status, body := postForm(t, p.client, p.server.URL+HealthPath, url.Values{
		"eylem":           {eylemYenidenBaslat},
		restartFormField:  {testdb.Collector},
		devgate.FormField: {testDevPassword},
	})
	if status != http.StatusOK {
		t.Fatalf("the press answered %d", status)
	}
	if !p.requested(testdb.Collector) {
		t.Fatal("no request row was written")
	}
	section := restartSectionOf(t, body)
	lang := testLang(t)
	notice := lang.Tf("saglik.yb.istendi", lang.T("saglik.servis.collector"))
	if !strings.Contains(section, notice) {
		t.Errorf("the page does not say the restart was requested (%q)", notice)
	}
	restartSays(t, restartRowOf(t, section, testdb.Collector), testdb.Collector, "bekliyor")
	if strings.Contains(section, `" selected>`) && !strings.Contains(section, `<option value="" selected>`) {
		t.Error("after an accepted press a service is left chosen in the select")
	}

	var entries, linked int
	if err := p.admin.QueryRow(context.Background(), `
		SELECT (SELECT count(*) FROM panel_audit_log WHERE action = $1 AND target = $2
		          AND actor_label LIKE $3),
		       (SELECT count(*) FROM panel_operations o JOIN panel_audit_log a ON a.id = o.audit_id
		         WHERE o.action = $1 AND o.target = $2 AND o.outcome = 'succeeded'
		           AND o.actor_label LIKE $3)`,
		panel.ActionRestartRequested, testdb.Collector, "%"+testEmailSuffix).Scan(&entries, &linked); err != nil {
		t.Fatal(err)
	}
	if entries != 1 || linked != 1 {
		t.Errorf("%d audit entries and %d operation records linked to one, want 1 and 1", entries, linked)
	}

	// A second press inside the minute is refused with the sentence for
	// it, and writes nothing more.
	_, body = postForm(t, p.client, p.server.URL+HealthPath, url.Values{
		"eylem":           {eylemYenidenBaslat},
		restartFormField:  {testdb.Collector},
		devgate.FormField: {testDevPassword},
	})
	if !strings.Contains(restartSectionOf(t, body), lang.T("saglik.yb.cok_yakin")) {
		t.Error("a second press inside the minute does not say it is too soon")
	}
}

// TestPressingRestartWithAWrongPasswordWritesNothing.
func TestPressingRestartWithAWrongPasswordWritesNothing(t *testing.T) {
	p := newRestartPage(t)
	p.ran(testdb.Collector, 10*time.Minute, 5*time.Second)

	since := dbNow(t)
	_, body := postForm(t, p.client, p.server.URL+HealthPath, url.Values{
		"eylem":           {eylemYenidenBaslat},
		restartFormField:  {testdb.Collector},
		devgate.FormField: {"bu-parola-yanlis"},
	})
	if p.requested(testdb.Collector) {
		t.Error("a wrong password wrote a request")
	}
	// And the operation record calls it a refusal. B3e merged its own
	// classifier into outcomeFor; nothing asked what the record said,
	// so recording every restart refusal as a failure passed the suite.
	if _, _, outcome, found := operationSince(t, panel.ActionRestartRequested, since); !found {
		t.Error("no operation was recorded for the refused press")
	} else if outcome != string(panel.OutcomeRefused) {
		t.Errorf("a wrong password is recorded as %q, want %q", outcome, panel.OutcomeRefused)
	}
	section := restartSectionOf(t, body)
	if !strings.Contains(section, testLang(t).T("saglik.yb.parola_gerek")) {
		t.Error("the page does not say the password was not accepted")
	}
	// A refusal keeps the choice, so the person types only the password.
	if !strings.Contains(section, `<option value="collector" selected>`) {
		t.Error("after a refused press the service that was chosen is not chosen any more")
	}
}

// TestPressingRestartWithoutTheFormsTokenIsRefused: no CSRF token, no
// request - with the right password, so the token is the only thing
// missing.
func TestPressingRestartWithoutTheFormsTokenIsRefused(t *testing.T) {
	p := newRestartPage(t)
	p.ran(testdb.Collector, 10*time.Minute, 5*time.Second)

	resp, err := p.client.PostForm(p.server.URL+HealthPath, url.Values{
		"eylem":           {eylemYenidenBaslat},
		restartFormField:  {testdb.Collector},
		devgate.FormField: {testDevPassword},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != statusCSRFExpired {
		t.Errorf("a post without the token answered %d, want %d", resp.StatusCode, statusCSRFExpired)
	}
	if p.requested(testdb.Collector) {
		t.Error("a post without the form's token wrote a request")
	}
}

// TestOnlyWhoMayReadTheHealthPageCanPressRestart: a member who is not an
// owner is refused at the door, with a valid token of their own and the
// right password; a superadmin is let through.
func TestOnlyWhoMayReadTheHealthPageCanPressRestart(t *testing.T) {
	p := newRestartPage(t)
	p.ran(testdb.Collector, 10*time.Minute, 5*time.Second)
	ctx := context.Background()

	viewer := makeUser(t, p.store, "yb-izleyici", false)
	if err := p.store.AddMember(ctx, healthSite, viewer.ID, panel.RoleAdmin, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	client := signedIn(t, p.server.URL, viewer.Email)
	_, account := get(t, client, p.server.URL+AccountPath)
	status, _ := postWithToken(t, client, p.server.URL+HealthPath, csrfFrom(t, account), url.Values{
		"eylem":           {eylemYenidenBaslat},
		restartFormField:  {testdb.Collector},
		devgate.FormField: {testDevPassword},
	})
	if status != http.StatusForbidden {
		t.Errorf("a site administrator's press answered %d, want 403", status)
	}
	if p.requested(testdb.Collector) {
		t.Fatal("somebody who may not read the health page wrote a request")
	}

	operator := makeUser(t, p.store, "yb-isletmeci", true)
	_, _ = postForm(t, signedIn(t, p.server.URL, operator.Email), p.server.URL+HealthPath, url.Values{
		"eylem":           {eylemYenidenBaslat},
		restartFormField:  {testdb.Collector},
		devgate.FormField: {testDevPassword},
	})
	if !p.requested(testdb.Collector) {
		t.Error("a superadmin with the developer password could not ask for a restart")
	}
}

// unreadableRestarts is a restart status that cannot be read.
type unreadableRestarts struct{}

func (unreadableRestarts) RestartStatus(context.Context, panel.Access) (panel.RestartStatus, error) {
	return panel.RestartStatus{}, errors.New("the restart status could not be read")
}

// TestAnUnreadableRequestDarkensOnlyItsOwnSection: when the restart
// status cannot be read, the section says so where it would have been,
// and the services above it are still drawn: every section on this page
// fails on its own.
//
// The failure is put in through Server.restartReadOverride, not a row.
// This test used to write a value that is not a time, and that row is no
// longer a failure: the page reads it as no request, the way the service
// does (panel's TestAValueThatIsNotATimeIsNoRequestHereEither).
func TestAnUnreadableRequestDarkensOnlyItsOwnSection(t *testing.T) {
	p := newRestartPage(t)
	p.srv.restartReadOverride = unreadableRestarts{}

	status, body := get(t, p.client, p.server.URL+HealthPath)
	if status != http.StatusOK {
		t.Fatalf("the health page answered %d with one unreadable request", status)
	}
	lang := testLang(t)
	if !strings.Contains(body, html.EscapeString(lang.T("saglik.yb.okunamadi"))) {
		t.Error("the page does not say the restart status could not be read")
	}
	if strings.Contains(body, `id="yeniden-baslatma-durumu"`) {
		t.Error("the restart section is drawn as if it had been read")
	}
	if !strings.Contains(body, "saglik-yb-collector") {
		t.Error("the services section went dark with the restart section")
	}
}

// TestWithoutADeveloperPasswordTheButtonIsNotDrawn: on a deployment with
// no developer password the button could only be refused, so the section
// says why instead of drawing it - and still shows every service's state.
func TestWithoutADeveloperPasswordTheButtonIsNotDrawn(t *testing.T) {
	srv, store := setupTestServer(t)
	shut, err := devgate.New(devgate.Config{}, devgate.Options{})
	if err != nil {
		t.Fatal(err)
	}
	srv.Gate = shut
	testdb.Lock(t, testdb.Admin(t), testdb.RestartRequestLock)
	server, client, _ := healthServerOn(t, srv, store)
	writeBeatDetail(t, store, testdb.Collector, "saglik-yb-kapisiz", time.Now().Add(-time.Hour),
		map[string]int64{}, nil, "", heartbeat.TokenKeyUnknown)

	status, body := get(t, client, server.URL+HealthPath)
	if status != http.StatusOK {
		t.Fatalf("the health page answered %d", status)
	}
	section := restartSectionOf(t, body)
	if !strings.Contains(section, testLang(t).T("saglik.yb.kapi_yok")) {
		t.Error("the section does not say there is no developer password")
	}
	if strings.Contains(section, `name="eylem" value="`+eylemYenidenBaslat+`"`) {
		t.Error("the button is drawn on a deployment whose gate is shut")
	}
	restartSays(t, restartRowOf(t, section, testdb.Collector), testdb.Collector, "yok")
}
