package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
)

// TestARangeIsWholeLocalDays.
//
// §6 records the reason and this is where it is honoured: sessions are
// counted inside the range, so one that began before it is truncated at
// the boundary. A range starting at 14:37 cuts every session running at
// 14:37 - in the period being shown *and* in the one before it - so
// neighbouring periods cannot be added up, and neither matches what a
// customer means by "yesterday".
//
// Local rather than UTC is the other half. A shop in Istanbul asking for
// today wants their today; computing it in UTC hands them three hours of
// yesterday and loses three hours of today, every day, invisibly.
func TestARangeIsWholeLocalDays(t *testing.T) {
	istanbul, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		t.Skip("this machine has no timezone database")
	}
	// Mid-afternoon on purpose: the bug this guards against is invisible
	// at midnight.
	now := time.Date(2026, 8, 18, 14, 37, 12, 500, istanbul)

	cases := []struct {
		days      int
		wantFrom  string
		wantTo    string
		wantHours int
	}{
		{days: 1, wantFrom: "2026-08-18 00:00:00", wantTo: "2026-08-19 00:00:00", wantHours: 24},
		{days: 7, wantFrom: "2026-08-12 00:00:00", wantTo: "2026-08-19 00:00:00", wantHours: 24 * 7},
		{days: 30, wantFrom: "2026-07-20 00:00:00", wantTo: "2026-08-19 00:00:00", wantHours: 24 * 30},
	}
	for _, tc := range cases {
		from, to := wholeDays(now, tc.days)

		if got := from.Format("2006-01-02 15:04:05"); got != tc.wantFrom {
			t.Errorf("%d days: from = %s, want %s", tc.days, got, tc.wantFrom)
		}
		if got := to.Format("2006-01-02 15:04:05"); got != tc.wantTo {
			t.Errorf("%d days: to = %s, want %s", tc.days, got, tc.wantTo)
		}
		if from.Location() != istanbul || to.Location() != istanbul {
			t.Errorf("%d days: the range left the site's zone (%v..%v)", tc.days, from.Location(), to.Location())
		}
		// n days means n days. A range one hour short would still look
		// right in both timestamps above.
		if got := to.Sub(from).Hours(); got != float64(tc.wantHours) {
			t.Errorf("%d days spans %v hours, want %d", tc.days, got, tc.wantHours)
		}
	}
}

// TestARangeSurvivesADaylightSavingChange.
//
// A day is not always 24 hours, and this is the week it matters. The
// boundaries still have to be local midnights: a range built by
// subtracting 24-hour multiples drifts by an hour and starts reporting
// 23 or 25 hours of a day as though it were the whole of it.
func TestARangeSurvivesADaylightSavingChange(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skip("this machine has no timezone database")
	}
	// The Monday after Europe's spring-forward Sunday in 2026.
	now := time.Date(2026, 3, 30, 11, 0, 0, 0, berlin)

	from, to := wholeDays(now, 7)
	if h, m, s := from.Clock(); h != 0 || m != 0 || s != 0 {
		t.Errorf("from is %s, not a local midnight", from.Format(time.RFC3339))
	}
	if h, m, s := to.Clock(); h != 0 || m != 0 || s != 0 {
		t.Errorf("to is %s, not a local midnight", to.Format(time.RFC3339))
	}
	// Seven days across a spring-forward is 167 hours, not 168. The
	// point is that the *boundaries* are right; the elapsed time simply
	// is what the calendar says.
	if got := to.Sub(from).Hours(); got != 167 {
		t.Errorf("the week spans %v hours; across a spring-forward it is 167", got)
	}
}

// TestEmptinessSaysWhichKindOfNothing walks every combination.
//
// Three facts wear one appearance, and collapsing them would present a
// setup step nobody performed as though it were a measurement. The order
// of the checks is what this pins: a failure first, because a number
// that was never fetched is not zero.
func TestEmptinessSaysWhichKindOfNothing(t *testing.T) {
	withRows := analytics.Dashboard{
		Traffic: analytics.Summary{Snapshots: 12, HumanIPs: 5},
		Beacon:  analytics.BeaconSummary{Pageviews: 40, Visitors: 9},
	}
	empty := analytics.Dashboard{}
	unreachableBoard := analytics.Dashboard{
		TrafficErr: analytics.ErrUnavailable,
		BeaconErr:  analytics.ErrUnavailable,
	}
	refusedBoard := analytics.Dashboard{
		TrafficErr: analytics.ErrRefused,
		BeaconErr:  analytics.ErrRefused,
	}

	both := sourcePresence{known: true, traffic: true, bacon: true}
	neither := sourcePresence{known: true}
	unknown := sourcePresence{}

	cases := []struct {
		name   string
		source cardSource
		board  analytics.Dashboard
		seen   sourcePresence
		want   emptiness
	}{
		{"numbers present", sourceBeacon, withRows, both, hasData},
		{"installed, empty period", sourceBeacon, empty, both, nothingInRange},
		{"never installed", sourceBeacon, empty, neither, neverInstalled},
		{"empty, and we could not find out which", sourceBeacon, empty, unknown, nothingInRange},
		{"api unreachable", sourceBeacon, unreachableBoard, both, unreachable},
		{"api refused", sourceBeacon, refusedBoard, both, refused},

		// The collector's side reads a different field, and a site with
		// a collector but no snippet is an ordinary deployment - so the
		// two sources have to be able to disagree.
		{"traffic present", sourceTraffic, withRows, both, hasData},
		{"traffic never installed", sourceTraffic, empty, neither, neverInstalled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := emptinessFor(tc.source, tc.board, tc.seen); got != tc.want {
				t.Errorf("emptinessFor = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAFailureIsNeverReadAsZero is the one worth stating on its own.
//
// A card that says "0 visitors" because the API timed out is not a
// missing number, it is a wrong one - and the customer has no way to
// tell. Every failure has to win over every emptiness, whatever the
// zero-valued struct alongside it says.
func TestAFailureIsNeverReadAsZero(t *testing.T) {
	for _, err := range []error{analytics.ErrUnavailable, analytics.ErrRefused, errors.New("something else")} {
		board := analytics.Dashboard{BeaconErr: err, TrafficErr: err}
		for _, source := range []cardSource{sourceBeacon, sourceTraffic} {
			got := emptinessFor(source, board, sourcePresence{known: true})
			if got == hasData || got == nothingInRange || got == neverInstalled {
				t.Errorf("%v on %s produced %q; a number that was never fetched is not zero",
					err, source, got)
			}
		}
	}
}

// TestEveryCardAndEmptinessHasWordsInEveryLanguage.
//
// Both a card's label and the sentence replacing its number are
// assembled at runtime from an id, so neither the template walk nor the
// source scan in the ui package can see the family. That package carries
// a mirrored list for its dead-key check; this is the other direction,
// read from the real constants.
func TestEveryCardAndEmptinessHasWordsInEveryLanguage(t *testing.T) {
	srv := newTestServer(t)
	empties := []emptiness{neverInstalled, nothingInRange, unreachable, refused}
	sources := []cardSource{sourceBeacon, sourceTraffic}

	for _, lang := range srv.Renderer.Catalogs().Languages() {
		for id := range cards {
			for _, suffix := range []string{".baslik", ".aciklama"} {
				key := "pano.kart." + string(id) + suffix
				if !lang.Has(key) {
					t.Errorf("%s has no %s", lang.Code, key)
				}
			}
		}
		for _, why := range empties {
			for _, source := range sources {
				key := "pano.bos." + string(why) + "." + string(source)
				if !lang.Has(key) {
					t.Errorf("%s has no %s", lang.Code, key)
				}
			}
		}
	}
}

// TestTheDefaultCardsAreAllRealCards. defaultCards names ids; a typo
// would silently drop a card from the page rather than fail anywhere.
func TestTheDefaultCardsAreAllRealCards(t *testing.T) {
	if len(defaultCards) != 6 {
		t.Errorf("the default view has %d cards; the phase is six", len(defaultCards))
	}
	seen := map[cardID]bool{}
	for _, id := range defaultCards {
		if _, ok := cards[id]; !ok {
			t.Errorf("%q is in the default view and is not a card", id)
		}
		if seen[id] {
			t.Errorf("%q appears twice in the default view", id)
		}
		seen[id] = true
	}

	// Both sources represented. A default view drawn only from the
	// beacon would look like every JavaScript analytics tool and hide
	// the thing this product measures that they cannot.
	var beacon, traffic int
	for _, id := range defaultCards {
		switch cards[id].Source {
		case sourceBeacon:
			beacon++
		case sourceTraffic:
			traffic++
		}
	}
	if beacon == 0 || traffic == 0 {
		t.Errorf("the default view draws on %d beacon and %d collector cards; "+
			"a page from one source alone does not show what running both is for",
			beacon, traffic)
	}
}

// TestAnUnknownRangeFallsBackRatherThanFailing.
//
// The value ends up in a request to another service, so it is a closed
// set - but a mistyped URL should show the dashboard, not refuse. The
// list the page offers is the honest bound.
func TestAnUnknownRangeFallsBackRatherThanFailing(t *testing.T) {
	srv := newTestServer(t)
	for _, raw := range []string{"", "0", "-1", "365", "yedi", "7; DROP TABLE", "١٤", "7 "} {
		req := requestWithQuery(t, raw)
		got := srv.rangeFrom(req)
		if !contains(rangeDays, got) {
			t.Errorf("gun=%q produced %d, which is not one of the offered periods", raw, got)
		}
	}
	// And a real one is honoured.
	for _, days := range rangeDays {
		req := requestWithQuery(t, strconv.Itoa(days))
		if got := srv.rangeFrom(req); got != days {
			t.Errorf("gun=%d produced %d", days, got)
		}
	}
}

// TestTheDashboardNeverReadsAnalyticsFromTheDatabase is the
// architectural rule, asserted rather than remembered.
//
// The panel's database role has no access to the analytics tables, and
// that is the deployment's security shape rather than an inconvenience:
// the process the whole internet can reach must not also be the one with
// broad database rights. A handler that reached for a pool directly
// would compile, work in development against a superuser, and fail in
// production - which is the worst order to discover it in.
func TestTheDashboardNeverReadsAnalyticsFromTheDatabase(t *testing.T) {
	files, err := packageSources()
	if err != nil {
		t.Fatal(err)
	}
	banned := []string{
		"traffic_snapshots",
		"beacon_events",
		"internal/api",
	}
	for name, src := range files {
		for _, needle := range banned {
			if strings.Contains(src, needle) {
				t.Errorf("%s mentions %q; the panel reads analytics over HTTP from the "+
					"read-only API, never from the database - see internal/panel/analytics",
					name, needle)
			}
		}
	}
}

// requestWithQuery builds a GET whose ?gun= carries raw.
//
// Encoded rather than pasted into the target string: httptest.NewRequest
// parses its argument as a request line, so an unescaped space in the
// value makes the *test* panic on a malformed HTTP version - which says
// nothing about the handler and cost ten minutes to read once already.
// A browser would percent-encode it, so this does too.
func requestWithQuery(t *testing.T, raw string) *http.Request {
	t.Helper()
	target := "/site/bir?" + url.Values{"gun": {raw}}.Encode()
	return httptest.NewRequest(http.MethodGet, target, nil)
}

func contains(haystack []int, needle int) bool {
	for _, n := range haystack {
		if n == needle {
			return true
		}
	}
	return false
}
