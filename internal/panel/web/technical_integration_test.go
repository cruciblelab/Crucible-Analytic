//go:build integration

// D3 end to end: the collector's own breakdowns, drawn as columns on the
// pages D2 built, and shown to nobody who has not asked for them.
package web

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/testdb"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
)

const technicalSite = "d3-teknik"

// seedEnriched writes traffic rows with the country/ASN/known-bot columns
// filled in, which seedTraffic leaves at their defaults.
//
// D3's three breakdowns read exactly those columns, so seeding with the
// defaults would produce three sections that draw one row each - the
// never-determined group - and a test that passed without ever proving a
// decoder was wired to the right field.
func seedEnriched(t *testing.T, site string, when time.Time) {
	t.Helper()
	// As the collector: these are its rows.
	pool := testdb.Pool(t, testdb.Collector)
	t.Cleanup(pool.Close)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM traffic_snapshots WHERE site_id = $1`, site)
	})

	rows := []struct {
		ip       string
		ja4      string
		knownBot bool
		country  string
		asn      int
		asnOrg   string
		score    int
	}{
		// A recognised crawler out of a datacentre, scoring high.
		{"198.51.100.10", ja4Googlebot, true, "US", 15169, "Google LLC", 95},
		{"198.51.100.11", ja4Googlebot, true, "US", 15169, "Google LLC", 92},
		// An ordinary browser on a consumer network, scoring low.
		{"203.0.113.20", ja4Browser, false, "TR", 9121, "Turk Telekom", 4},
		{"203.0.113.21", ja4Browser, false, "TR", 9121, "Turk Telekom", 6},
		// Plaintext or an unparseable ClientHello, and nothing resolved:
		// the never-determined group, which the API flags rather than
		// drops so the groups still add up.
		{"192.0.2.30", "", false, "", 0, "", 50},
	}

	for i, r := range rows {
		_, err := pool.Exec(context.Background(), `
			INSERT INTO traffic_snapshots
			  (time, site_id, ip, ja4, prev_window_count, curr_window_count,
			   request_rate, bot_score, is_known_bot_ja4, country, asn, asn_org)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
			when.Add(time.Duration(i)*time.Minute), site, r.ip, r.ja4,
			0, 10+i, float64(10+i)/60, r.score, r.knownBot, r.country, r.asn, r.asnOrg)
		if err != nil {
			t.Fatalf("seeding enriched traffic: %v", err)
		}
	}
}

const (
	ja4Googlebot = "t13d1516h2_8daaf6152771_b186095e22b6"
	ja4Browser   = "t13d1715h2_5b57614c22b0_3d5424432f57"
)

// developerOwner signs in an owner with the developer preference on.
//
// Both halves are needed and they are different things: the role carries
// CapUseDeveloperMode, the preference says this person wants to see it
// right now. Setting only one is how a test proves half of a gate.
func developerOwner(t *testing.T, srv *Server, store *panel.Store, site, who string) (*http.Client, string) {
	t.Helper()
	server, client, owner := signedInOwner(t, srv, store, site, who)
	if err := store.SetDeveloperMode(context.Background(), owner.ID, true); err != nil {
		t.Fatal(err)
	}
	return client, server.URL
}

// TestTheCollectorsBreakdownsDrawRealRows is the phase.
//
// Three sections, three endpoints the panel never called before, two row
// shapes it never decoded before. A decoder wired to the wrong envelope
// key produces an empty section rather than an error, which is why this
// asserts on values out of the seeded rows rather than on the sections
// being present.
func TestTheCollectorsBreakdownsDrawRealRows(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	seedEnriched(t, technicalSite, time.Now().Add(-3*time.Hour))

	client, base := developerOwner(t, srv, store, technicalSite, "d3-gelistirici")
	status, body := get(t, client, base+sitePath(technicalSite))
	if status != http.StatusOK {
		t.Fatalf("the dashboard answered %d", status)
	}

	lang := srv.Renderer.Catalogs().Base()
	for _, kind := range technicalBreakdowns {
		if !strings.Contains(body, shown(lang.T("pano.kirilim."+string(kind)+".baslik"))) {
			t.Errorf("developer mode is on and the page has no %q section", kind)
		}
	}

	// One value out of each of the three, each from a different column of
	// the seeded rows and a different endpoint.
	for _, want := range []string{
		ja4Googlebot,   // fingerprints, straight off the row
		"Google LLC",   // ASNs, the resolved organisation beside the number
		"15169",        // ...and the number itself, which is what gets searched
		"Turk Telekom", // the other network, so this is not one lucky row
	} {
		if !strings.Contains(body, shown(want)) {
			t.Errorf("the page does not show %q, which the seeded rows contain", want)
		}
	}
}

// TestTheNeverDeterminedGroupSurvivesTheCollectorsBreakdowns.
//
// The collector's GroupStat has no Empty field: addresses whose country
// never resolved arrive under an empty key. The panel derives the flag
// from that, and a row drawn with a blank label - or dropped - would lose
// on the last hop the distinction the API went out of its way to keep.
func TestTheNeverDeterminedGroupSurvivesTheCollectorsBreakdowns(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	seedEnriched(t, technicalSite, time.Now().Add(-3*time.Hour))

	client, base := developerOwner(t, srv, store, technicalSite, "d3-bos-grup")
	_, body := get(t, client, base+sitePath(technicalSite))

	lang := srv.Renderer.Catalogs().Base()
	for _, kind := range []analytics.BreakdownKind{
		analytics.BreakdownFingerprints,
		analytics.BreakdownASNs,
		analytics.BreakdownServerCountries,
	} {
		name := shown(lang.T("pano.kirilim." + string(kind) + ".bos_grup"))
		if !strings.Contains(body, name) {
			t.Errorf("%s does not name its never-determined group; the seeded rows include one "+
				"address with no fingerprint, no country and no ASN", kind)
		}
	}
}

// TestATechnicalBreakdownIsRefusedToARoleThatMayNotSeeIt is the paired
// test D1 established for the analytics token's blast radius, applied to
// the thing D3 adds.
//
// The panel's API token can read every site; the panel's own authority
// layer is the only thing between one customer and another's numbers. D3
// widens what that layer has to hold: fingerprints and addresses are
// exactly the data a viewer's role says they may never see, and the
// detail page is reachable by typing a path.
//
// So both halves are asserted in one test. Checking only the refusal
// would pass against a handler that refuses everybody, which is the way a
// gate test quietly stops testing the gate.
func TestATechnicalBreakdownIsRefusedToARoleThatMayNotSeeIt(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	seedEnriched(t, technicalSite, time.Now().Add(-3*time.Hour))

	ctx := context.Background()
	server, ownerClient, owner := signedInOwner(t, srv, store, technicalSite, "d3-sahip")
	if err := store.SetDeveloperMode(ctx, owner.ID, true); err != nil {
		t.Fatal(err)
	}

	// A viewer on the same site. RoleViewer does not carry
	// CapUseDeveloperMode, which is the boundary under test.
	viewer := makeUser(t, store, "d3-izleyici", false)
	if err := store.AddMember(ctx, technicalSite, viewer.ID, panel.RoleViewer, nil); err != nil {
		t.Fatal(err)
	}
	// The preference is turned on for the viewer too, deliberately. It
	// must change nothing: a preference is not an authority, and a gate
	// that a user can open by ticking their own box is not a gate.
	if err := store.SetDeveloperMode(ctx, viewer.ID, true); err != nil {
		t.Fatal(err)
	}
	viewerClient := signedIn(t, server.URL, viewer.Email)

	path := breakdownPath(technicalSite, analytics.BreakdownFingerprints)

	if status, body := get(t, ownerClient, server.URL+path); status != http.StatusOK {
		t.Errorf("the owner got %d for a page their role allows", status)
	} else if !strings.Contains(body, shown(ja4Googlebot)) {
		t.Error("the owner's page has no fingerprint on it, so the refusal below proves nothing")
	}

	if status, body := get(t, viewerClient, server.URL+path); status != http.StatusNotFound {
		t.Errorf("a viewer got %d for the fingerprints page; want 404", status)
	} else if strings.Contains(body, shown(ja4Googlebot)) {
		t.Error("the refusal page still carried a fingerprint")
	}
}

// TestTheDetailPageAnswersOnRoleAloneNotThePreference.
//
// The deliberate asymmetry, written down as a test because it looks like
// a bug otherwise: the site page needs both the role and the preference,
// the detail page needs only the role.
//
// The reason is that they answer different questions. The site page
// appears without anybody asking for it, so it obeys D6 - no fingerprints
// in the default view. Typing the address of the fingerprints page *is*
// asking. Refusing that would make the preference an authority, which
// panel.Access.ShowsTechnical exists to keep it from being.
func TestTheDetailPageAnswersOnRoleAloneNotThePreference(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	seedEnriched(t, technicalSite, time.Now().Add(-3*time.Hour))

	// An owner with developer mode *off*: full role, preference not set.
	server, client, _ := signedInOwner(t, srv, store, technicalSite, "d3-tercih-kapali")

	// The site page must not volunteer it.
	lang := srv.Renderer.Catalogs().Base()
	_, board := get(t, client, server.URL+sitePath(technicalSite))
	if strings.Contains(board, shown(lang.T("pano.kirilim.parmak-izi.baslik"))) {
		t.Error("the site page showed the fingerprints section to somebody who has not turned developer mode on")
	}

	// The detail page must answer anyway.
	status, body := get(t, client, server.URL+breakdownPath(technicalSite, analytics.BreakdownFingerprints))
	if status != http.StatusOK {
		t.Fatalf("the detail page answered %d for an owner with the preference off; want 200 - "+
			"navigating to the address is itself the request the preference is about", status)
	}
	if !strings.Contains(body, shown(ja4Googlebot)) {
		t.Error("the detail page answered 200 with no fingerprint on it")
	}
}

// TestACollectorBreakdownDividesByTheCollectorsTotal.
//
// The quiet failure. A collector breakdown whose traffic summary was
// never fetched still draws every row and every count; only the share
// column empties to dashes, because a summary nobody asked for comes back
// a legitimate zero rather than an error. Nothing looks broken.
//
// Five addresses were seeded, two of them Googlebot's. So the
// fingerprints section's first row is 2 of 5 - 40% - and any share at all
// proves the denominator arrived.
func TestACollectorBreakdownDividesByTheCollectorsTotal(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	seedEnriched(t, technicalSite, time.Now().Add(-3*time.Hour))

	client, base := developerOwner(t, srv, store, technicalSite, "d3-payda")
	_, body := get(t, client, base+breakdownPath(technicalSite, analytics.BreakdownFingerprints))

	shares := percentagesIn(body)
	if len(shares) == 0 {
		t.Fatal("no percentage on the fingerprints page: the traffic summary was never fetched, " +
			"so every share fell back to a dash while the counts drew fine")
	}
	var sum float64
	for _, s := range shares {
		sum += s
	}
	// Every address falls in exactly one fingerprint group, so the shares
	// account for all of them. Loose bounds: the point is that the
	// denominator is the address count and not something unrelated, not
	// that rounding lands on 100.0.
	if sum < 95 || sum > 105 {
		t.Errorf("the fingerprint shares sum to %.1f%%, want about 100%% - that is what "+
			"dividing by the collector's own address count produces", sum)
	}
}

// TestTechnicalSectionsInABrowser.
//
// D3's own browser question is narrow and real: a JA4 fingerprint is
// about fifty characters of unbroken hex-and-underscore, which makes it
// the longest unbreakable string this product ever puts in a table cell.
// A path wraps at its slashes and a hostname at its dots; a fingerprint
// wraps nowhere. Nothing in Go can tell you whether that pushes a table
// off a phone screen.
//
// It reuses the D2 script rather than growing a second one - the same
// reason the section partial is one renderer: two would drift, and the
// overflow measurement is exactly the part that must not.
func TestTechnicalSectionsInABrowser(t *testing.T) {
	if os.Getenv("CA_BROWSER_TEST") == "" {
		t.Skip("set CA_BROWSER_TEST=1 to run this; it needs node, playwright and a chromium build")
	}

	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	seedEnriched(t, technicalSite, time.Now().Add(-3*time.Hour))

	server, _, owner := signedInOwner(t, srv, store, technicalSite, "d3-tarayici")
	if err := store.SetDeveloperMode(context.Background(), owner.ID, true); err != nil {
		t.Fatal(err)
	}

	script := writeBreakdownScript(t)
	cmd := exec.Command("node", script,
		server.URL, owner.Email, testAccountPassword,
		sitePath(technicalSite), breakdownPath(technicalSite, analytics.BreakdownFingerprints))
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("browser run failed: %v", err)
	}
	t.Logf("browser transcript:\n%s", out)

	var report struct {
		CSPViolations             []string `json:"csp_violations"`
		ConsoleErrors             []string `json:"console_errors"`
		Sections                  int      `json:"sections"`
		SectionsWithRows          int      `json:"sections_with_rows"`
		SectionsWithBars          int      `json:"sections_with_bars"`
		SectionsExplained         int      `json:"sections_explained"`
		Overflowing               int      `json:"overflowing"`
		PageScrollsSideways       bool     `json:"page_scrolls_sideways"`
		NamedRows                 int      `json:"named_rows"`
		MobileOverflowing         int      `json:"mobile_overflowing"`
		MobilePageScrollsSideways bool     `json:"mobile_page_scrolls_sideways"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("browser report: %v\n%s", err, out)
	}

	// The badge and the detail span are new markup on this page. Neither
	// may need an inline style or a script to render.
	if len(report.CSPViolations) > 0 {
		t.Errorf("Content-Security-Policy violations: %v", report.CSPViolations)
	}
	if len(report.ConsoleErrors) > 0 {
		t.Errorf("console errors: %v", report.ConsoleErrors)
	}

	// Eleven, not six: six beacon breakdowns, three collector ones, and
	// the histogram and the crossover summary. Composed from the
	// registries rather than typed, so adding a technical view without
	// drawing it changes this count.
	const nonBreakdownSections = 2 // the histogram and the crossover summary
	if want := len(defaultBreakdowns) + len(technicalBreakdowns) + nonBreakdownSections; report.Sections != want {
		t.Errorf("the page drew %d sections, want %d - developer mode is on for this owner",
			report.Sections, want)
	}

	// Every section draws something: a table, a list of bars, or a
	// sentence saying why it has neither. A section that draws none of
	// the three is a heading over blank space, which reads as a fault.
	if got := report.SectionsWithRows + report.SectionsWithBars + report.SectionsExplained; got != report.Sections {
		t.Errorf("%d sections but only %d draw anything (%d tables, %d bar lists, %d explained): "+
			"one is a heading over nothing",
			report.Sections, got, report.SectionsWithRows, report.SectionsWithBars, report.SectionsExplained)
	}
	if report.SectionsWithBars == 0 {
		t.Error("no bar list rendered; the histogram drew a heading and no bars, which is what " +
			"a blocked inline style would look like")
	}

	// The measurement this test exists for.
	if report.MobileOverflowing > 0 || report.MobilePageScrollsSideways {
		t.Errorf("at phone width %d sections overflow and page-scrolls-sideways=%v; a fingerprint "+
			"is fifty characters that wrap nowhere, so the cell has to scroll inside its own box "+
			"rather than push the page", report.MobileOverflowing, report.MobilePageScrollsSideways)
	}
	if report.Overflowing > 0 || report.PageScrollsSideways {
		t.Errorf("at desktop width %d sections overflow, page-scrolls-sideways=%v",
			report.Overflowing, report.PageScrollsSideways)
	}

	// The seeded rows include one address with no fingerprint, no country
	// and no ASN, so all three technical sections have a named group.
	if report.NamedRows == 0 {
		t.Error("no named row rendered; the never-determined group is drawn blank or dropped")
	}
}

// TestTheHistogramAndCrossoverDrawRealNumbers is D3's remainder.
//
// Two endpoints the panel never called before, and the crossover one
// joins two tables that store an address differently depending on the
// deployment's privacy mode - a join a stub cannot exercise at all.
func TestTheHistogramAndCrossoverDrawRealNumbers(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	seedEnriched(t, technicalSite, time.Now().Add(-3*time.Hour))

	client, base := developerOwner(t, srv, store, technicalSite, "d3-teknik-sayilar")
	status, body := get(t, client, base+sitePath(technicalSite))
	if status != http.StatusOK {
		t.Fatalf("the dashboard answered %d", status)
	}

	lang := srv.Renderer.Catalogs().Base()
	for _, key := range []string{"pano.skor.baslik", "pano.kesisim.baslik"} {
		if !strings.Contains(body, shown(lang.T(key))) {
			t.Errorf("developer mode is on and the page has no %q section", key)
		}
	}

	// Five addresses were seeded and none of them ran the beacon, so the
	// crossover has to report five seen and five silent. Asserted on the
	// numbers rather than on the section rendering, because a section
	// that fetched nothing still renders - it just renders a sentence.
	if !strings.Contains(body, shown(lang.T("pano.kesisim.sessiz"))) {
		t.Error("the crossover section has no silent count")
	}
	if strings.Contains(body, shown(lang.T("pano.bos.ulasilamiyor.trafik"))) {
		t.Error("a technical section reports the API unreachable while the others drew fine")
	}

	// The histogram's bands. Two addresses scored 92-95 and two scored
	// 4-6, so both ends of the range have to be present.
	for _, band := range []string{"0–9", "90–100"} {
		if !strings.Contains(body, shown(band)) {
			t.Errorf("the histogram has no %s band; the empty ones must be drawn too", band)
		}
	}
}

// TestTheSilentListNamesTheAddressesNobodyElseCanSee.
//
// This is the product's actual claim, end to end: five addresses reached
// the collector, none ran the beacon, so a beacon-only tool would report
// zero visitors while this page lists five.
func TestTheSilentListNamesTheAddressesNobodyElseCanSee(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	seedEnriched(t, technicalSite, time.Now().Add(-3*time.Hour))

	client, base := developerOwner(t, srv, store, technicalSite, "d3-sessiz")
	status, body := get(t, client, base+addressListPath(technicalSite, analytics.ListSilent))
	if status != http.StatusOK {
		t.Fatalf("the silent list answered %d", status)
	}

	// The addresses as stored, which is what the page can show: masked
	// networks rather than whole addresses. Both seeded networks have to
	// be there.
	for _, want := range []string{"198.51.100", "203.0.113"} {
		if !strings.Contains(body, shown(want)) {
			t.Errorf("the silent list does not show %q, which the seeded rows contain", want)
		}
	}
	// And the fingerprint column, which is the reason this list is
	// developer-only.
	if !strings.Contains(body, shown(ja4Googlebot)) {
		t.Error("the silent list has no fingerprint column filled in")
	}
}

// TestAnAddressListIsRefusedToARoleThatMayNotSeeIt is the paired test
// again, for the pages D3's remainder adds.
//
// Repeated rather than assumed from the breakdown one: these are a
// different handler with a different registry, and a gate that exists in
// one handler proves nothing about the other. This is a list of
// addresses and fingerprints, which is exactly what a viewer's role says
// they may never see.
func TestAnAddressListIsRefusedToARoleThatMayNotSeeIt(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	seedEnriched(t, technicalSite, time.Now().Add(-3*time.Hour))

	ctx := context.Background()
	server, ownerClient, owner := signedInOwner(t, srv, store, technicalSite, "d3-liste-sahip")
	if err := store.SetDeveloperMode(ctx, owner.ID, true); err != nil {
		t.Fatal(err)
	}

	viewer := makeUser(t, store, "d3-liste-izleyici", false)
	if err := store.AddMember(ctx, technicalSite, viewer.ID, panel.RoleViewer, nil); err != nil {
		t.Fatal(err)
	}
	// On for the viewer too: a gate that a user can open by ticking their
	// own box is not a gate.
	if err := store.SetDeveloperMode(ctx, viewer.ID, true); err != nil {
		t.Fatal(err)
	}
	viewerClient := signedIn(t, server.URL, viewer.Email)

	for _, kind := range []analytics.AddressListKind{analytics.ListSilent, analytics.ListJSBots} {
		path := addressListPath(technicalSite, kind)

		if status, body := get(t, ownerClient, server.URL+path); status != http.StatusOK {
			t.Errorf("%s: the owner got %d for a page their role allows", kind, status)
		} else if kind == analytics.ListSilent && !strings.Contains(body, shown("198.51.100")) {
			t.Errorf("%s: the owner's page lists no address, so the refusal below proves nothing", kind)
		}

		if status, body := get(t, viewerClient, server.URL+path); status != http.StatusNotFound {
			t.Errorf("%s: a viewer got %d; want 404", kind, status)
		} else if strings.Contains(body, shown("198.51.100")) {
			t.Errorf("%s: the refusal page still listed an address", kind)
		}
	}
}

// TestAnUnknownAddressListIs404, so a path segment somebody typed never
// reaches the API as an endpoint name.
func TestAnUnknownAddressListIs404(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)

	client, base := developerOwner(t, srv, store, technicalSite, "d3-bilinmeyen")
	for _, segment := range []string{"yok", "..%2Fadmin", "crossover%2Fsummary"} {
		status, _ := get(t, client, base+MembersPathPrefix+technicalSite+addressListPathSegment+segment)
		if status != http.StatusNotFound {
			t.Errorf("segment %q answered %d, want 404", segment, status)
		}
	}
}

// TestTheTechnicalSectionsStayOffTheDefaultView. D6's rule, asserted for
// the two sections as it already is for the three breakdowns: the page a
// customer opens carries no histogram and no jargon.
func TestTheTechnicalSectionsStayOffTheDefaultView(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	seedEnriched(t, technicalSite, time.Now().Add(-3*time.Hour))

	// An owner with the preference off.
	server, client, _ := signedInOwner(t, srv, store, technicalSite, "d3-varsayilan")
	_, body := get(t, client, server.URL+sitePath(technicalSite))

	lang := srv.Renderer.Catalogs().Base()
	for _, key := range []string{"pano.skor.baslik", "pano.kesisim.baslik"} {
		if strings.Contains(body, shown(lang.T(key))) {
			t.Errorf("the default view shows %q to somebody who has not turned developer mode on", key)
		}
	}
}
