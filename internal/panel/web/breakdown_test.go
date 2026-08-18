package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// siteWith builds a Site whose beacon summary and one breakdown are set.
func siteWith(pageviews, events int, kind analytics.BreakdownKind, b analytics.Breakdown) analytics.Site {
	return analytics.Site{
		Dashboard: analytics.Dashboard{
			Beacon:  analytics.BeaconSummary{Pageviews: pageviews, Events: events},
			Traffic: analytics.Summary{Snapshots: 1},
		},
		Breakdowns: map[analytics.BreakdownKind]analytics.Breakdown{kind: b},
	}
}

// TestTheDefaultBreakdownsAreAllRealBreakdowns. defaultBreakdowns names
// kinds; a typo would silently drop a section from the page.
func TestTheDefaultBreakdownsAreAllRealBreakdowns(t *testing.T) {
	if len(defaultBreakdowns) != 6 {
		t.Errorf("the default view has %d sections; the phase is six", len(defaultBreakdowns))
	}
	seen := map[analytics.BreakdownKind]bool{}
	for _, kind := range defaultBreakdowns {
		if _, ok := breakdownDefs[kind]; !ok {
			t.Errorf("%q is in the default view and is not a breakdown", kind)
		}
		// The registry in the web package and the one in the analytics
		// package are two halves of one thing: labels here, transport
		// there. A kind in one and not the other renders a section whose
		// every fetch fails.
		if !analytics.KnownBreakdown(kind) {
			t.Errorf("%q is drawn here and the client cannot fetch it", kind)
		}
		if seen[kind] {
			t.Errorf("%q appears twice in the default view", kind)
		}
		seen[kind] = true
	}
	for kind := range breakdownDefs {
		if !seen[kind] {
			t.Errorf("%q is defined and never shown", kind)
		}
	}
}

// TestEveryBreakdownHasWordsInEveryLanguage.
//
// A section's title, its two column headings and the name given to its
// never-determined group are all assembled from the kind at runtime, so
// the template walk cannot see them. The ui package mirrors this list
// for its dead-key check; this is the other direction, from the real
// registry.
func TestEveryBreakdownHasWordsInEveryLanguage(t *testing.T) {
	srv := newTestServer(t)
	for _, lang := range srv.Renderer.Catalogs().Languages() {
		for kind, def := range breakdownDefs {
			suffixes := []string{".baslik", ".aciklama", ".sutun", ".sayi"}
			if def.NamedGroup {
				suffixes = append(suffixes, ".bos_grup")
			}
			for _, suffix := range suffixes {
				key := "pano.kirilim." + string(kind) + suffix
				if !lang.Has(key) {
					t.Errorf("%s has no %s", lang.Code, key)
				}
			}
			// And the other direction: a breakdown with no
			// never-determined group must not carry a word for one.
			if key := "pano.kirilim." + string(kind) + ".bos_grup"; !def.NamedGroup && lang.Has(key) {
				t.Errorf("%s has %s for a breakdown whose endpoint returns no such group", lang.Code, key)
			}
		}
	}
}

// TestTheNeverDeterminedGroupIsANamedRow.
//
// The API flags it rather than dropping it so the groups add up. Drawing
// it with a blank label would show a row nobody can read; dropping it
// would make the column quietly short. It gets the breakdown's own word
// for it - "Doğrudan" for referrers is not the same sentence as
// "Bilinmiyor" for devices, and a shared one would be wrong for both.
func TestTheNeverDeterminedGroupIsANamedRow(t *testing.T) {
	srv := newTestServer(t)
	lang := srv.Renderer.Catalogs().Base()
	f := ui.NewFormatter(lang, srv.zone(t.Context()))

	for kind, def := range breakdownDefs {
		if !def.NamedGroup {
			continue
		}
		t.Run(string(kind), func(t *testing.T) {
			site := siteWith(100, 100, kind, analytics.Breakdown{
				Kind: kind, Total: 2,
				Rows: []analytics.Row{
					{Key: "bilinen", Count: 60, Visitors: 30},
					{Key: "", Empty: true, Count: 40, Visitors: 20},
				},
			})
			view := srv.section(lang, f, def, site, sourcePresence{})
			if len(view.Rows) != 2 {
				t.Fatalf("got %d rows, want 2 - the flagged group must not be dropped", len(view.Rows))
			}
			named := view.Rows[1]
			if !named.Named {
				t.Error("the flagged row is not marked as named")
			}
			if named.Label == "" {
				t.Error("the flagged row has a blank label")
			}
			if want := lang.T("pano.kirilim." + string(kind) + ".bos_grup"); named.Label != want {
				t.Errorf("label = %q, want %q", named.Label, want)
			}
			if view.Rows[0].Named {
				t.Error("an ordinary row is marked as named")
			}
		})
	}
}

// TestAShareWithNoDenominatorIsNotZero.
//
// The panel refuses to draw a missing total as 0%. A row showing "0%"
// when the summary never arrived is not a missing number, it is a wrong
// one, and the reader has no way to tell - the same rule the cards
// follow for a failed fetch, applied to the column that divides.
func TestAShareWithNoDenominatorIsNotZero(t *testing.T) {
	srv := newTestServer(t)
	lang := srv.Renderer.Catalogs().Base()
	f := ui.NewFormatter(lang, srv.zone(t.Context()))
	def := breakdownDefs[analytics.BreakdownPages]

	// Rows exist; the summary reports nothing. That is not a real
	// deployment state, which is exactly why it has to be handled: it is
	// what a partly failed page looks like.
	site := siteWith(0, 0, analytics.BreakdownPages, analytics.Breakdown{
		Kind: analytics.BreakdownPages, Total: 1,
		Rows: []analytics.Row{{Key: "/", Count: 9, Visitors: 4}},
	})
	view := srv.section(lang, f, def, site, sourcePresence{})
	if len(view.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(view.Rows))
	}
	if got := view.Rows[0].Share; got != f.Dash() {
		t.Errorf("share = %q with a zero denominator, want the dash %q", got, f.Dash())
	}
	if strings.Contains(view.Rows[0].Share, "0") {
		t.Errorf("share = %q; a missing total must never render as a percentage", view.Rows[0].Share)
	}
}

// TestTheShareUsesTheBreakdownsOwnMetric.
//
// Events are counted in occurrences and everything else in pageviews.
// Dividing an event count by the pageview total produces a percentage
// that looks entirely plausible and is meaningless - the failure nobody
// catches by looking at the page.
func TestTheShareUsesTheBreakdownsOwnMetric(t *testing.T) {
	srv := newTestServer(t)
	lang := srv.Renderer.Catalogs().Base()
	f := ui.NewFormatter(lang, srv.zone(t.Context()))

	// 200 pageviews, 50 events. An event row of 25 is half the events and
	// an eighth of the pageviews; only one of those is the right answer.
	site := siteWith(200, 50, analytics.BreakdownEvents, analytics.Breakdown{
		Kind: analytics.BreakdownEvents, Total: 1,
		Rows: []analytics.Row{{Key: "kayit", Count: 25, Visitors: 10}},
	})
	view := srv.section(lang, f, breakdownDefs[analytics.BreakdownEvents], site, sourcePresence{})
	if len(view.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(view.Rows))
	}
	want := f.Share(25, 50)
	if got := view.Rows[0].Share; got != want {
		t.Errorf("share = %q, want %q (25 of 50 events, not of 200 pageviews)", got, want)
	}
	if got, wrong := view.Rows[0].Share, f.Share(25, 200); got == wrong {
		t.Errorf("share = %q, which is the count over the pageview total", got)
	}
}

// TestASectionSaysWhichKindOfNothing. The four states a card can be in
// apply here too, for the same reason: a section drawn as "no data"
// presents an uninstalled snippet as a measurement.
func TestASectionSaysWhichKindOfNothing(t *testing.T) {
	def := breakdownDefs[analytics.BreakdownPages]
	live := analytics.Dashboard{
		Beacon:  analytics.BeaconSummary{Pageviews: 40},
		Traffic: analytics.Summary{Snapshots: 3},
	}

	cases := []struct {
		name     string
		board    analytics.Dashboard
		b        analytics.Breakdown
		presence sourcePresence
		want     emptiness
	}{
		{
			name: "rows present", board: live,
			b:    analytics.Breakdown{Rows: []analytics.Row{{Key: "/", Count: 1}}},
			want: hasData,
		},
		{
			// The site is measuring; this breakdown simply has nothing in
			// the period. Not "never installed" - the summary proves the
			// snippet is there.
			name: "installed, nothing in this period", board: live,
			b:    analytics.Breakdown{},
			want: nothingInRange,
		},
		{
			name:  "the source never wrote for this site",
			board: analytics.Dashboard{},
			b:     analytics.Breakdown{}, presence: sourcePresence{known: true},
			want: neverInstalled,
		},
		{
			// This is the one worth having: the cards above are fine and
			// this one section's call did not answer. Drawing it as an
			// empty table would report a timeout as a measurement of zero.
			name: "the section's own call failed", board: live,
			b:    analytics.Breakdown{Err: analytics.ErrUnavailable},
			want: unreachable,
		},
		{
			name: "the section's own call was refused", board: live,
			b:    analytics.Breakdown{Err: analytics.ErrRefused},
			want: refused,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			site := analytics.Site{Dashboard: tc.board}
			if got := breakdownEmptiness(def, tc.b, site, tc.presence); got != tc.want {
				t.Errorf("breakdownEmptiness = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestASectionFailureNeverReadsAsZero, stated on its own because it is
// the property the whole emptiness apparatus exists for.
func TestASectionFailureNeverReadsAsZero(t *testing.T) {
	def := breakdownDefs[analytics.BreakdownPages]
	live := analytics.Dashboard{Beacon: analytics.BeaconSummary{Pageviews: 40}}
	for _, err := range []error{analytics.ErrUnavailable, analytics.ErrRefused, errors.New("other")} {
		b := analytics.Breakdown{Err: err}
		got := breakdownEmptiness(def, b, analytics.Site{Dashboard: live}, sourcePresence{known: true})
		if got == hasData || got == nothingInRange || got == neverInstalled {
			t.Errorf("%v produced %q; a list that was never fetched is not an empty one", err, got)
		}
	}
}

// TestTheMoreLinkAppearsOnlyWhenThereIsMore. A "see all 8" link on a
// section already showing all eight is a click that changes nothing,
// which is how a reader learns the links are decorative.
func TestTheMoreLinkAppearsOnlyWhenThereIsMore(t *testing.T) {
	srv := newTestServer(t)
	lang := srv.Renderer.Catalogs().Base()
	f := ui.NewFormatter(lang, srv.zone(t.Context()))

	rows := make([]analytics.Row, sectionRows)
	for i := range rows {
		rows[i] = analytics.Row{Key: "/" + strconv.Itoa(i), Count: 10 - i, Visitors: 1}
	}

	for _, tc := range []struct {
		total    int
		wantLink bool
	}{
		{total: sectionRows, wantLink: false},
		{total: sectionRows + 1, wantLink: true},
		{total: 143, wantLink: true},
	} {
		site := analytics.Site{
			Dashboard: analytics.Dashboard{Beacon: analytics.BeaconSummary{Pageviews: 100}},
			Breakdowns: map[analytics.BreakdownKind]analytics.Breakdown{
				analytics.BreakdownPages: {Kind: analytics.BreakdownPages, Rows: rows, Total: tc.total},
			},
		}
		views := srv.sections(lang, f, "site", site, sourcePresence{}, 7, defaultBreakdowns)
		var pages breakdownView
		for _, v := range views {
			if v.Kind == analytics.BreakdownPages {
				pages = v
			}
		}
		if got := pages.MoreURL != ""; got != tc.wantLink {
			t.Errorf("total=%d: link present = %v, want %v", tc.total, got, tc.wantLink)
		}
		if tc.wantLink {
			if !strings.Contains(pages.MoreURL, "/detay/") {
				t.Errorf("total=%d: link %q does not point at the detail page", tc.total, pages.MoreURL)
			}
			// The period has to survive the click. A link that dropped it
			// would show the last seven days' list under a page that said
			// ninety.
			if !strings.Contains(pages.MoreURL, "gun=7") {
				t.Errorf("total=%d: link %q loses the period", tc.total, pages.MoreURL)
			}
			if !strings.Contains(pages.MoreText, f.Number(int64(tc.total))) {
				t.Errorf("total=%d: label %q does not carry the localised total", tc.total, pages.MoreText)
			}
		}
	}
}

// TestPageCountAndBounds.
//
// The page number becomes an offset in a request to another service, so
// the bound is not cosmetic: an unbounded one is a request to walk a
// table. Anything unreadable takes page 1 rather than erroring, for the
// reason the range picker gives - a mistyped URL should show the list.
func TestPageCountAndBounds(t *testing.T) {
	for _, tc := range []struct{ total, want int }{
		{0, 0}, {1, 1}, {detailRows, 1}, {detailRows + 1, 2},
		{detailRows * maxDetailPage, maxDetailPage},
		{detailRows*maxDetailPage + 1, maxDetailPage},
		{1 << 30, maxDetailPage},
	} {
		if got := pageCount(tc.total, detailRows); got != tc.want {
			t.Errorf("pageCount(%d) = %d, want %d", tc.total, got, tc.want)
		}
	}

	for raw, want := range map[string]int{
		"":                              1,
		"1":                             1,
		"3":                             3,
		"0":                             1,
		"-4":                            1,
		"abc":                           1,
		"1e3":                           1,
		"9999":                          1,
		strconv.Itoa(maxDetailPage):     maxDetailPage,
		strconv.Itoa(maxDetailPage + 1): 1,
	} {
		target := "/site/x/detay/sayfa?" + url.Values{"sayfa": {raw}}.Encode()
		r := httptest.NewRequest(http.MethodGet, target, nil)
		if got := detailPageFrom(r); got != want {
			t.Errorf("sayfa=%q gives page %d, want %d", raw, got, want)
		}
	}
}

// TestDetailURLLeavesTheFirstPageUnnumbered, so the first page has one
// address rather than two.
func TestDetailURLLeavesTheFirstPageUnnumbered(t *testing.T) {
	base := breakdownPath("site", analytics.BreakdownPages)
	if got := detailURL(base, 7, 1); strings.Contains(got, "sayfa=") {
		t.Errorf("page 1 URL is %q, want no sayfa parameter", got)
	}
	got := detailURL(base, 30, 4)
	for _, want := range []string{"gun=30", "sayfa=4"} {
		if !strings.Contains(got, want) {
			t.Errorf("page 4 URL is %q, want it to contain %q", got, want)
		}
	}
}

// TestABreakdownPathEscapesTheSiteID. The id reaches a URL, and this
// panel's own links are the last place a traversal would be noticed.
func TestABreakdownPathEscapesTheSiteID(t *testing.T) {
	got := breakdownPath("../gizli", analytics.BreakdownPages)
	if strings.Contains(got, "../") {
		t.Errorf("path = %q, which walks out of the site", got)
	}
	if breakdownPath("", analytics.BreakdownPages) != "" {
		t.Error("an empty site id produced a path")
	}
}
