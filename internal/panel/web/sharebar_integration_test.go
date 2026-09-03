//go:build integration

// The share bar, against a real page.
//
// The unit tests in internal/panel/ui prove BarWidth's arithmetic. This
// proves the number reaches the browser: that the rect is drawn, that
// its width is the percentage printed beside it, and that a row with no
// denominator has no bar rather than an empty one.
//
// Worth its own file because the failure has no error in it. A bar wired
// to the wrong field, or dropped by a template branch, renders a table
// that looks entirely correct - just without the picture, or with a
// picture of the wrong number.

package web

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
)

const shareBarSite = "cubuk-testi"

// shareBarRow pulls one table row's bar width and its printed share out
// of the rendered HTML, so the two can be compared as numbers.
//
// Matched off the rendered page rather than off the view struct: the
// question here is what the browser receives, and a field set correctly
// and never printed is exactly the failure this is for.
var shareBarRow = regexp.MustCompile(
	`(?s)<td class="sayi soluk pay">(.*?)</td>`)

var barRect = regexp.MustCompile(`<rect [^>]*width="([0-9.]+)"`)

// barPresent matches the bar element itself rather than a width that
// parses.
//
// The distinction is the whole of a mutation this file failed to catch
// on its first run. Drawing the bar unconditionally leaves
// `width=""` - an element that renders as a zero-width rect, which is
// exactly the "0%" claim the row must not make - and a pattern looking
// for a number simply does not match it. The test stayed green over a
// page that had regressed.
//
// *Yokluğu, "sayı bulamadım" ile aynı şey sanan bir kontrol, kusurun
// tam da ürettiği şekli görmez.*
var barPresent = regexp.MustCompile(`<svg class="pay-cubuk"`)
var barShare = regexp.MustCompile(`<span class="pay-sayi">([^<]*)</span>`)

// TestEveryBarIsTheWidthOfTheNumberPrintedBesideIt.
//
// A bar whose length disagrees with the number inside its own cell is
// worse than no bar: the picture is the half people believe, and nothing
// on the page contradicts it out loud.
func TestEveryBarIsTheWidthOfTheNumberPrintedBesideIt(t *testing.T) {
	srv, store := setupTestServer(t)
	withRealAPI(t, srv)
	seedBeacon(t, shareBarSite, time.Now().Add(-3*time.Hour), breakdownFixture())

	server, client, _ := signedInOwner(t, srv, store, shareBarSite, "cubuk-sahip")
	status, body := get(t, client, server.URL+sitePath(shareBarSite))
	if status != http.StatusOK {
		t.Fatalf("the dashboard answered %d", status)
	}

	cells := shareBarRow.FindAllStringSubmatch(body, -1)
	if len(cells) == 0 {
		t.Fatal("the page has no share cells, so this test would pass by comparing nothing")
	}

	var withBar int
	for _, cell := range cells {
		rect := barRect.FindStringSubmatch(cell[1])
		share := barShare.FindStringSubmatch(cell[1])
		if share == nil {
			t.Errorf("a share cell prints no number: %s", strings.TrimSpace(cell[1]))
			continue
		}
		if rect == nil {
			continue // covered by the no-denominator test below
		}
		withBar++

		width, err := strconv.ParseFloat(rect[1], 64)
		if err != nil {
			t.Errorf("unreadable bar width %q", rect[1])
			continue
		}
		// The printed share is localised - Turkish writes the decimal
		// separator as a comma - so it comes back through the same
		// substitution rather than being parsed as though it were English.
		printed, err := strconv.ParseFloat(
			strings.ReplaceAll(strings.TrimPrefix(strings.TrimSpace(share[1]), "%"), ",", "."), 64)
		if err != nil {
			t.Errorf("unreadable printed share %q", share[1])
			continue
		}
		// One decimal place each, so they agree to within rounding.
		if diff := width - printed; diff > 0.05 || diff < -0.05 {
			t.Errorf("a bar is %.1f units wide beside a printed share of %.1f%%. "+
				"They are one fact said twice, and the picture is the half people "+
				"believe", width, printed)
		}
	}
	if withBar == 0 {
		t.Error("no row drew a bar at all; every share cell was text, which is the " +
			"state this phase existed to change")
	}
}

// TestARowWithNoDenominatorDrawsNoBar.
//
// Formatter.Share already refuses to print "no requests yet" as "0%".
// A zero-width bar makes that claim in a picture instead, so the whole
// element has to be absent - absent in the rendered HTML, not merely
// zero in a struct.
//
// # Why the summary is faked and the rows are real
//
// The state under test is "rows arrived, the total they divide by did
// not", which is what a summary call that failed or was skipped leaves
// behind. It is reachable in production and awkward to seed: every
// ordinary fixture that produces rows also produces a summary. So the
// breakdown rows come from the real API and only the summary is
// rewritten to zero.
//
// # And the guard that caught this test being empty
//
// The first version matched the printed share with `%?([0-9,.]+)`,
// which cannot match an em dash - so every row this test exists to
// examine fell through the "no number here" branch and was skipped. It
// passed, against fourteen rows it never looked at.
//
// The count at the bottom is what said so. *Bir testin yeşil olması,
// bir şeye baktığı anlamına gelmez.*
func TestARowWithNoDenominatorDrawsNoBar(t *testing.T) {
	srv, store := setupTestServer(t)
	base, token := analyticsAPI(t)
	upstream, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/beacon/summary") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"pageviews":0,"visitors":0,"sessions":0,"events":0,"bounce_rate":0}`))
			return
		}
		httputil.NewSingleHostReverseProxy(upstream).ServeHTTP(w, r)
	}))
	t.Cleanup(proxy.Close)
	client, err := analytics.New(proxy.URL, token)
	if err != nil {
		t.Fatal(err)
	}
	srv.Analytics = client

	site := shareBarSite + "-paysiz"
	seedBeacon(t, site, time.Now().Add(-3*time.Hour), breakdownFixture())
	server, httpClient, _ := signedInOwner(t, srv, store, site, "cubuk-paysiz")

	status, body := get(t, httpClient, server.URL+sitePath(site))
	if status != http.StatusOK {
		t.Fatalf("the dashboard answered %d", status)
	}

	var dashes int
	for _, cell := range shareBarRow.FindAllStringSubmatch(body, -1) {
		share := barShare.FindStringSubmatch(cell[1])
		if share == nil || strings.ContainsAny(share[1], "0123456789") {
			continue
		}
		dashes++
		if barPresent.MatchString(cell[1]) {
			t.Errorf("a row whose share is %q drew a bar anyway. A zero-width bar says "+
				"0%%, which is a different fact from \"not known\" - the one "+
				"Formatter.Share already refuses to print", share[1])
		}
	}
	if dashes == 0 {
		t.Fatal("no row came back without a denominator, so this test examined nothing. " +
			"The summary was rewritten to zero; if the rows are gone too, the fixture " +
			"stopped producing them rather than the bar stopping being drawn")
	}
}
