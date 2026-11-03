//go:build integration

// B3f, through the settings page with the real read API over the real
// table: a save that adds a country or ASN rule stops once, shows each
// new rule's addresses in the last day, and saves when the same form
// comes back confirmed.

package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
	"github.com/cruciblelab/crucible-analytic/internal/testdb"
)

const (
	previewSite  = "bl-onizleme"
	previewOther = "bl-onizleme-baska"
	// Codes and a network no other suite writes, so their counts are this
	// test's whatever else the shared table holds. QZ, XQ, XR, XS are
	// user-assigned in ISO 3166 and never a real country.
	previewASN = 999991
)

// seedGeo writes collector rows with a country and network, as the
// collector, one hour ago - inside the preview's day.
func seedGeo(t *testing.T, site string, rows []struct {
	ip, country string
	asn         int
}) {
	t.Helper()
	testdb.CleanSite(t, testdb.Admin(t), site)
	pool := testdb.Pool(t, testdb.Collector)
	for _, r := range rows {
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO traffic_snapshots
			  (time, site_id, ip, ja4, prev_window_count, curr_window_count,
			   request_rate, bot_score, country, asn)
			VALUES (now() - interval '1 hour', $1, $2::inet, '', 0, 1, 0, 0, $3, $4)`,
			site, r.ip, r.country, r.asn); err != nil {
			t.Fatal(err)
		}
	}
}

// previewFields is the confirm form's hidden fields, read off the page -
// sent back as the browser would send them.
func previewFields(t *testing.T, body string) url.Values {
	t.Helper()
	section := regexp.MustCompile(`(?s)<section class="ayar onizleme" id="onizleme">.*?</section>`).FindString(body)
	if section == "" {
		t.Fatalf("the page has no preview:\n%s", messageOf(body))
	}
	out := url.Values{}
	for _, m := range regexp.MustCompile(`<input type="hidden" name="([a-z_]+)" value="([^"]*)">`).FindAllStringSubmatch(section, -1) {
		out.Set(m[1], htmlUnescape(m[2]))
	}
	return out
}

func htmlUnescape(s string) string {
	return strings.NewReplacer("&#43;", "+", "&amp;", "&", "&#34;", `"`, "&#39;", "'", "&lt;", "<", "&gt;", ">").Replace(s)
}

// previewRow is the count a preview draws for one rule, or false.
func previewRow(body, rule string) (string, bool) {
	m := regexp.MustCompile(`<tr data-kural="` + regexp.QuoteMeta(rule) + `"><td><code>[^<]*</code></td><td>([^<]*)</td></tr>`).
		FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// TestANewBlockedCountryIsShownBeforeItIsSaved.
func TestANewBlockedCountryIsShownBeforeItIsSaved(t *testing.T) {
	srv, store := setupSoleTenantServer(t)
	withRealAPI(t, srv)
	ctx := context.Background()
	restoreGlobal(t, store, panel.KeyBlockedCountries, panel.KeyBlockedASNs)
	cleanOperations(t, store)

	// Three addresses in QZ - one on a second site, which still counts:
	// the rule refuses on every site - and two on the test network.
	seedGeo(t, previewSite, []struct {
		ip, country string
		asn         int
	}{
		{"198.51.100.1", "QZ", previewASN},
		{"198.51.100.1", "QZ", previewASN},
		{"198.51.100.2", "QZ", previewASN},
		{"198.51.100.3", "", 0},
	})
	seedGeo(t, previewOther, []struct {
		ip, country string
		asn         int
	}{{"198.51.100.9", "QZ", 1}})

	owner := makeUser(t, store, "bl-sahip", false)
	if err := store.AddMember(ctx, previewSite, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	c := signedIn(t, server.URL, owner.Email)
	page := server.URL + settingsPath(previewSite)
	lang := srv.Renderer.Catalogs().Base()
	stored := func(key panel.Key) []string {
		t.Helper()
		v, err := store.GetSetting(ctx, key, "")
		if err != nil {
			t.Fatal(err)
		}
		return toStringList(v)
	}
	save := func(key panel.Key, value string) (int, string) {
		t.Helper()
		return post(t, c, page, url.Values{"anahtar": {string(key)}, "islem": {"kaydet"}, "deger": {value}})
	}

	// Adding two: the save stops, nothing is stored, each rule has its row.
	status, body := save(panel.KeyBlockedCountries, "qz, XQ")
	if status != http.StatusOK {
		t.Fatalf("the preview answered %d: %s", status, messageOf(body))
	}
	if got := stored(panel.KeyBlockedCountries); len(got) != 0 {
		t.Fatalf("the list was saved before it was confirmed: %v", got)
	}
	if n, ok := previewRow(body, "QZ"); !ok || n != "3" {
		t.Errorf("QZ is drawn %q (%v), want 3: two addresses on one site and one on another", n, ok)
	}
	if n, ok := previewRow(body, "XQ"); !ok || n != "0" {
		t.Errorf("XQ is drawn %q (%v), want 0", n, ok)
	}
	if !strings.Contains(body, shown(lang.T("ayarlar.onizleme.giris"))) {
		t.Error("the preview does not say what the rules do")
	}
	// What will be saved is on the page, as it will be saved.
	if !strings.Contains(body, `<p id="onizleme-liste">`+shown(lang.T("ayarlar.onizleme.liste"))+` <code>qz, XQ</code></p>`) {
		t.Error("the preview does not show the list the button saves")
	}
	fields := previewFields(t, body)
	if fields.Get("onay") != "QZ,XQ" || fields.Get("anahtar") != string(panel.KeyBlockedCountries) {
		t.Fatalf("the confirm form carries %v", fields)
	}

	// Sent back: saved, as typed, and recorded as an ordinary change.
	status, body = post(t, c, page, fields)
	if status != http.StatusOK || strings.Contains(body, `id="onizleme"`) {
		t.Fatalf("the confirmed save answered %d with a preview: %s", status, messageOf(body))
	}
	// As typed: the store keeps the case it was given, and the collector
	// compares without it (limiter.GeoBlocklist.Set).
	if got := stored(panel.KeyBlockedCountries); !slices.Equal(got, []string{"qz", "XQ"}) {
		t.Fatalf("saved %v, want [qz XQ]", got)
	}
	if op := lastOperation(t, store, string(panel.KeyBlockedCountries)); op.outcome != string(panel.OutcomeSucceeded) {
		t.Errorf("the confirmed save was recorded %q", op.outcome)
	}

	// Adding a third to a list in force: only the new rule is previewed,
	// and the two already in force are named as not countable.
	_, body = save(panel.KeyBlockedCountries, "QZ, XQ, XR")
	if _, ok := previewRow(body, "QZ"); ok {
		t.Error("a rule already in force is previewed; its traffic is never recorded, so its count would read 0")
	}
	if _, ok := previewRow(body, "XR"); !ok {
		t.Error("the added rule has no row")
	}
	if !strings.Contains(body, shown(lang.Tf("ayarlar.onizleme.yururlukte", 2))) {
		t.Error("the preview does not say two rules in force are left out")
	}
	// Which of the two depends on what the shared heartbeat table holds;
	// GeoRefusals' own test pins both. Here: one of them, whole.
	ret := regexp.MustCompile(strings.ReplaceAll(regexp.QuoteMeta(shown(lang.T("ayarlar.onizleme.ret"))), "%s", `[^<]+`))
	if !ret.MatchString(body) && !strings.Contains(body, shown(lang.T("ayarlar.onizleme.ret_bilinmiyor"))) {
		t.Error("the preview says nothing about what the rules in force turned away")
	}

	// A confirmation for another set of rules is not this one's: the list
	// changed between the preview and the save, so it is previewed again.
	stale := previewFields(t, body)
	stale.Set("deger", "QZ, XQ, XR, XS")
	status, body = post(t, c, page, stale)
	if _, ok := previewRow(body, "XS"); status != http.StatusOK || !ok {
		t.Errorf("a stale confirmation answered %d without previewing XS", status)
	}
	if got := stored(panel.KeyBlockedCountries); len(got) != 2 {
		t.Errorf("a stale confirmation saved %v", got)
	}

	// Removing a rule turns nothing new away: saved at once.
	status, body = save(panel.KeyBlockedCountries, "QZ")
	if status != http.StatusOK || strings.Contains(body, `id="onizleme"`) {
		t.Errorf("a removal answered %d with a preview", status)
	}
	if got := stored(panel.KeyBlockedCountries); !slices.Equal(got, []string{"QZ"}) {
		t.Errorf("after the removal the list is %v", got)
	}

	// The same for networks, compared as numbers.
	_, body = save(panel.KeyBlockedASNs, fmt.Sprint(previewASN))
	if n, ok := previewRow(body, fmt.Sprint(previewASN)); !ok || n != "2" {
		t.Errorf("AS%d is drawn %q (%v), want 2", previewASN, n, ok)
	}
}

// TestAPreviewThatCannotBeReadStillOffersTheSave.
//
// The read API down: each new rule is drawn with a dash, never a zero -
// zero is the one answer that reads as "safe to add" - the page says the
// matches could not be counted, and the confirm form still saves.
func TestAPreviewThatCannotBeReadStillOffersTheSave(t *testing.T) {
	srv, store := setupSoleTenantServer(t)
	dead, err := analytics.New("http://127.0.0.1:1", "jeton")
	if err != nil {
		t.Fatal(err)
	}
	srv.Analytics = dead
	restoreGlobal(t, store, panel.KeyBlockedCountries)
	owner := makeUser(t, store, "bl-kapali", false)
	if err := store.AddMember(context.Background(), previewSite, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	c := signedIn(t, server.URL, owner.Email)
	page := server.URL + settingsPath(previewSite)
	lang := srv.Renderer.Catalogs().Base()

	status, body := post(t, c, page, url.Values{
		"anahtar": {string(panel.KeyBlockedCountries)}, "islem": {"kaydet"}, "deger": {"QZ"}})
	if status != http.StatusOK {
		t.Fatalf("answered %d", status)
	}
	if n, ok := previewRow(body, "QZ"); !ok || n == "0" {
		t.Errorf("an unreadable count is drawn %q (%v)", n, ok)
	}
	if !strings.Contains(body, shown(lang.T("ayarlar.onizleme.okunamadi"))) {
		t.Error("the page does not say the matches could not be counted")
	}
	if status, _ := post(t, c, page, previewFields(t, body)); status != http.StatusOK {
		t.Errorf("the confirmed save answered %d", status)
	}
	if v, _ := store.GetSetting(context.Background(), panel.KeyBlockedCountries, ""); !slices.Equal(toStringList(v), []string{"QZ"}) {
		t.Errorf("the save did not go through: %v", v)
	}
}

// TestATenantIsShownNoDeploymentCounts.
//
// The owner of one site among others is refused the save (H10), and the
// refusal carries no preview: the counts are every customer's traffic.
func TestATenantIsShownNoDeploymentCounts(t *testing.T) {
	srv, store := setupSoleTenantServer(t)
	withRealAPI(t, srv)
	ctx := context.Background()
	restoreGlobal(t, store, panel.KeyBlockedCountries)
	owner := makeUser(t, store, "bl-kiraci", false)
	other := makeUser(t, store, "bl-kiraci-oteki", false)
	if err := store.AddMember(ctx, previewSite, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, previewOther, other.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	c := signedIn(t, server.URL, owner.Email)

	status, body := post(t, c, server.URL+settingsPath(previewSite), url.Values{
		"anahtar": {string(panel.KeyBlockedCountries)}, "islem": {"kaydet"}, "deger": {"QZ"}})
	if status == http.StatusOK || strings.Contains(body, `id="onizleme"`) {
		t.Errorf("a tenant got %d and a preview of the deployment's traffic", status)
	}
}

// TestAPreviewSaysWhenTheLookupPlacesNothing.
//
// A read API that saw traffic but placed no address in a country: every
// country rule reads 0, and that zero is not "safe to add" but "this rule
// can match nothing" - the page says so for countries, and not for
// networks, which were placed. The fake answers what the real one would
// on a deployment with the country lookup off.
func TestAPreviewSaysWhenTheLookupPlacesNothing(t *testing.T) {
	srv, store := setupSoleTenantServer(t)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		rows := `"countries":[],"asns":[]`
		if r.URL.Query().Has("country") {
			rows = `"countries":[{"country":"QZ","addresses":0}],"asns":[]`
		} else if r.URL.Query().Has("asn") {
			rows = `"countries":[],"asns":[{"asn":999991,"addresses":0}]`
		}
		fmt.Fprintf(w, `{"from":"2026-11-02T00:00:00Z","to":"2026-11-03T00:00:00Z","addresses":40,"with_country":0,"with_asn":31,%s}`, rows)
	}))
	defer api.Close()
	client, err := analytics.New(api.URL, "jeton")
	if err != nil {
		t.Fatal(err)
	}
	srv.Analytics = client
	restoreGlobal(t, store, panel.KeyBlockedCountries, panel.KeyBlockedASNs)
	owner := makeUser(t, store, "bl-cozumsuz", false)
	if err := store.AddMember(context.Background(), previewSite, owner.ID, panel.RoleOwner, panel.Grant{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(srv.Handler())
	defer server.Close()
	c := signedIn(t, server.URL, owner.Email)
	page := server.URL + settingsPath(previewSite)
	lang := srv.Renderer.Catalogs().Base()

	_, body := post(t, c, page, url.Values{"anahtar": {string(panel.KeyBlockedCountries)}, "islem": {"kaydet"}, "deger": {"QZ"}})
	if !strings.Contains(body, shown(lang.T("ayarlar.onizleme.cozumsuz_ulke"))) {
		t.Error("a country rule over traffic with no country placed is not said to match nothing")
	}
	if !strings.Contains(body, shown(lang.Tf("ayarlar.onizleme.toplam", "40", "0", "31"))) {
		t.Error("the window's totals are not drawn")
	}
	_, body = post(t, c, page, url.Values{"anahtar": {string(panel.KeyBlockedASNs)}, "islem": {"kaydet"}, "deger": {"999991"}})
	if _, ok := previewRow(body, "999991"); !ok {
		t.Fatal("the network rule has no row")
	}
	if strings.Contains(body, shown(lang.T("ayarlar.onizleme.cozumsuz_asn"))) ||
		strings.Contains(body, shown(lang.T("ayarlar.onizleme.cozumsuz_ulke"))) {
		t.Error("a network rule is said to match nothing when networks were placed")
	}
}
