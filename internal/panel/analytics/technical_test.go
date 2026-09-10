package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/api"
)

// TestTheScoreHistogramKeepsItsEmptyBands.
//
// The API returns all ten bands whether or not anything landed in them,
// because a histogram that omits its empty bands has a different shape
// from the data. A client that dropped them would draw a chart with the
// gaps closed up, which is a different chart.
func TestTheScoreHistogramKeepsItsEmptyBands(t *testing.T) {
	const body = `{"site_id":"s","buckets":[
		{"min":0,"max":9,"unique_ips":40},
		{"min":10,"max":19,"unique_ips":0},
		{"min":20,"max":29,"unique_ips":0},
		{"min":90,"max":100,"unique_ips":12}]}`

	from, to := window()
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))

	got := c.FetchTechnical(context.Background(), "s", from, to, TechnicalRequest{Scores: true})
	if got.Scores.Err != nil {
		t.Fatalf("scores: %v", got.Scores.Err)
	}
	if len(got.Scores.Bands) != 4 {
		t.Fatalf("got %d bands, want 4 - the empty ones must survive", len(got.Scores.Bands))
	}
	if got.Scores.Bands[1].Addresses != 0 || got.Scores.Bands[1].Min != 10 {
		t.Errorf("the empty band came back as %+v", got.Scores.Bands[1])
	}
	if want := 52; got.Scores.Total() != want {
		t.Errorf("Total() = %d, want %d", got.Scores.Total(), want)
	}
}

// TestCrossoverCarriesTheConfigurationSignalSeparately.
//
// beacon_only_ips is not a measurement beside the others: in a correct
// deployment it is zero, because a browser that loaded the page
// necessarily connected through the collector first. Non-zero means the
// collector is not in the path, or the beacon's trusted_proxies is wrong
// and it is recording a proxy's address.
//
// Decoded into its own field rather than folded into the counts, so the
// panel can say what it is rather than draw it as a fourth number.
func TestCrossoverCarriesTheConfigurationSignalSeparately(t *testing.T) {
	// Band keys are "ips_seen" and "ips_ran_js", which is what
	// api.CoverageBand emits. This fixture used to say "unique_ips" and
	// "ran_js" - the same two wrong names the decoder was reading - so
	// the test and the code agreed with each other and both disagreed
	// with the producer. It passed for as long as that lasted, and the
	// coverage table drew zeros on every real deployment.
	//
	// Fixed here, and derived rather than restated in
	// TestTheCoverageBandsSurviveTheApisOwnJson: a hand-typed wire name
	// is right when it is typed and only stays right by accident.
	const body = `{"site_id":"s","ips_seen":100,"ips_ran_js":40,"ips_silent":60,
		"js_coverage":0.4,"beacon_only_ips":7,
		"bands":[{"min":0,"max":9,"ips_seen":50,"ips_ran_js":38},
		         {"min":90,"max":100,"ips_seen":20,"ips_ran_js":1}]}`

	from, to := window()
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))

	got := c.FetchTechnical(context.Background(), "s", from, to, TechnicalRequest{Crossover: true}).Crossover
	if got.Err != nil {
		t.Fatalf("crossover: %v", got.Err)
	}
	if got.Seen != 100 || got.RanJS != 40 || got.Silent != 60 {
		t.Errorf("counts = seen %d, ran %d, silent %d", got.Seen, got.RanJS, got.Silent)
	}
	if got.BeaconOnly != 7 {
		t.Errorf("BeaconOnly = %d, want 7", got.BeaconOnly)
	}
	if len(got.Bands) != 2 || got.Bands[1].RanJS != 1 {
		t.Errorf("bands = %+v", got.Bands)
	}
}

// TestBothAddressListsDecodeIntoOneShape.
//
// The two endpoints answer under different envelope keys ("ips" and
// "bots") and carry different columns: a silent address has a request
// rate and no browser, a JS bot has a browser and no rate. Wired to the
// wrong key, a decoder produces an empty list rather than an error.
func TestBothAddressListsDecodeIntoOneShape(t *testing.T) {
	cases := []struct {
		kind    AddressListKind
		body    string
		wantIP  string
		wantJS  bool
		checkFn func(*testing.T, AddressRow)
	}{
		{
			kind: ListSilent,
			body: `{"total":3,"ips":[{"ip":"198.51.100.0/24","peak_score":88,
				"peak_request_rate":4.5,"country":"US","asn":15169,"asn_name":"Google LLC",
				"ja4":"t13d","ja4_label":"Googlebot","last_seen":"2026-08-20T10:00:00Z"}]}`,
			wantIP: "198.51.100.0/24",
			checkFn: func(t *testing.T, r AddressRow) {
				if r.Rate == 0 {
					t.Error("a silent row lost its request rate")
				}
				if r.Browser != "" {
					t.Errorf("a silent row has a browser %q; nothing ever reported one", r.Browser)
				}
			},
		},
		{
			kind: ListJSBots,
			body: `{"total":2,"bots":[{"ip":"203.0.113.0/24","peak_score":72,
				"is_bot_ua":true,"browser":"HeadlessChrome","os":"Linux",
				"ja4":"t13d","country":"DE","last_seen":"2026-08-20T11:00:00Z"}]}`,
			wantIP: "203.0.113.0/24",
			checkFn: func(t *testing.T, r AddressRow) {
				if r.Browser != "HeadlessChrome" {
					t.Errorf("Browser = %q", r.Browser)
				}
				if !r.BotUA {
					t.Error("BotUA lost: a User-Agent that says it is automation is a " +
						"different claim from a high score, and the table shows both")
				}
			},
		},
	}

	from, to := window()
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			got := c.FetchTechnical(context.Background(), "s", from, to,
				TechnicalRequest{List: tc.kind, Limit: 20}).List
			if got.Err != nil {
				t.Fatalf("list: %v", got.Err)
			}
			if len(got.Rows) != 1 {
				t.Fatalf("got %d rows, want 1 - a decoder on the wrong envelope key "+
					"produces an empty list rather than an error", len(got.Rows))
			}
			if got.Rows[0].Address != tc.wantIP {
				t.Errorf("Address = %q, want %q", got.Rows[0].Address, tc.wantIP)
			}
			if got.Total == 0 {
				t.Error("Total is zero; the pager would say there is nothing more")
			}
			tc.checkFn(t, got.Rows[0])
		})
	}
}

// TestEachListAsksItsOwnEndpoint. The two paths differ by more than a
// query parameter, and a copy-paste that pointed both at one would draw
// the same table under two headings.
func TestEachListAsksItsOwnEndpoint(t *testing.T) {
	seen := map[AddressListKind]string{}
	from, to := window()

	for kind := range addressLists {
		c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen[kind] = r.URL.Path
			_, _ = w.Write([]byte(`{"total":0,"ips":[],"bots":[]}`))
		}))
		c.FetchTechnical(context.Background(), "s", from, to,
			TechnicalRequest{List: kind, Limit: 5})
	}

	if len(seen) != 2 {
		t.Fatalf("asked %d endpoints, want 2", len(seen))
	}
	if seen[ListSilent] == seen[ListJSBots] {
		t.Errorf("both lists asked %q", seen[ListSilent])
	}
	for kind, path := range seen {
		if !strings.Contains(path, addressLists[kind].path) {
			t.Errorf("%s asked %q, want it to contain %q", kind, path, addressLists[kind].path)
		}
	}
}

// TestAnUnconfiguredClientOnlyFailsWhatWasAsked, matching FetchSite: a
// view nobody wanted is not a failure, and reporting one would put a
// warning on a page that is fine.
func TestAnUnconfiguredClientOnlyFailsWhatWasAsked(t *testing.T) {
	var c *Client
	from, to := window()

	got := c.FetchTechnical(context.Background(), "s", from, to, TechnicalRequest{Scores: true})
	if !errors.Is(got.Scores.Err, ErrUnavailable) {
		t.Errorf("Scores.Err = %v, want ErrUnavailable", got.Scores.Err)
	}
	if got.Crossover.Err != nil {
		t.Errorf("Crossover.Err = %v for a view nobody asked for", got.Crossover.Err)
	}
	if got.List.Err != nil {
		t.Errorf("List.Err = %v for a list nobody asked for", got.List.Err)
	}
}

// TestOneFailingViewDoesNotTakeTheOthers. Three independent reads, three
// independent answers - a section whose call did not return says so while
// the others still draw.
func TestOneFailingViewDoesNotTakeTheOthers(t *testing.T) {
	from, to := window()
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "score-distribution") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"ips_seen":10,"ips_ran_js":4,"ips_silent":6,"js_coverage":0.4}`))
	}))

	got := c.FetchTechnical(context.Background(), "s", from, to,
		TechnicalRequest{Scores: true, Crossover: true})

	if got.Scores.Err == nil {
		t.Error("the failing endpoint reported success")
	}
	if got.Crossover.Err != nil {
		t.Errorf("the healthy endpoint failed too: %v", got.Crossover.Err)
	}
	if got.Crossover.Seen != 10 {
		t.Errorf("Seen = %d, want 10 - the healthy view lost its numbers", got.Crossover.Seen)
	}
}

// TestAHostileSiteIDIsEscaped. The site id reaches a URL path here as it
// does in breakdown.go, so it is escaped rather than concatenated.
func TestAHostileSiteIDIsEscaped(t *testing.T) {
	var got string
	from, to := window()
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.EscapedPath()
		_, _ = w.Write([]byte(`{"buckets":[]}`))
	}))

	c.FetchTechnical(context.Background(), "../../admin", from, to, TechnicalRequest{Scores: true})

	if strings.Contains(got, "../") {
		t.Errorf("the path is %q; a site id climbed out of its segment", got)
	}
}

// TestTheCoverageBandsSurviveTheApisOwnJson ties this decoder to the
// producer instead of to a fixture.
//
// The bug this closes: the decoder read "unique_ips" and "ran_js" while
// api.CoverageBand emits "ips_seen" and "ips_ran_js". JSON decoding says
// nothing about a name it cannot find, so every band arrived as zero and
// the panel's coverage table drew a row of zeros - underneath a summary,
// from the same response, that was correct. It was found by looking at a
// screenshot of a live deployment, not by any test, because the test
// beside it had been given the same two wrong names by hand.
//
// So this one does not name the wire format at all. It marshals the
// API's own type and requires the numbers to arrive, which is a claim
// neither side can satisfy alone: rename a field on either side and this
// fails.
func TestTheCoverageBandsSurviveTheApisOwnJson(t *testing.T) {
	produced := api.CrossoverSummary{
		SiteID: "s", IPsSeen: 70, IPsRanJS: 40, IPsSilent: 30, JSCoverage: 0.4,
		Bands: []api.CoverageBand{
			{Min: 0, Max: 9, IPsSeen: 50, IPsRanJS: 38, JSCoverage: 0.76},
			{Min: 90, Max: 100, IPsSeen: 20, IPsRanJS: 2, JSCoverage: 0.1},
		},
	}
	body, err := json.Marshal(produced)
	if err != nil {
		t.Fatal(err)
	}

	from, to := window()
	c := clientFor(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	got := c.FetchTechnical(context.Background(), "s", from, to,
		TechnicalRequest{Crossover: true}).Crossover
	if got.Err != nil {
		t.Fatalf("crossover: %v", got.Err)
	}

	if len(got.Bands) != len(produced.Bands) {
		t.Fatalf("decoded %d bands from %d produced", len(got.Bands), len(produced.Bands))
	}
	for i, want := range produced.Bands {
		g := got.Bands[i]
		if g.Min != want.Min || g.Max != want.Max {
			t.Errorf("band %d range = %d-%d, want %d-%d", i, g.Min, g.Max, want.Min, want.Max)
		}
		// The two that were silently dropped. Named in the failure so a
		// future rename reads as a rename rather than as a zero.
		if g.Addresses != want.IPsSeen {
			t.Errorf("band %d addresses = %d, want %d; the API emits this as ips_seen",
				i, g.Addresses, want.IPsSeen)
		}
		if g.RanJS != want.IPsRanJS {
			t.Errorf("band %d ran-JS = %d, want %d; the API emits this as ips_ran_js",
				i, g.RanJS, want.IPsRanJS)
		}
	}

	// And the summary, from the same bytes: it was always right, and a
	// fix that broke it while fixing the bands would be a worse trade.
	if got.Seen != produced.IPsSeen || got.RanJS != produced.IPsRanJS {
		t.Errorf("summary = seen %d, ran %d; want %d, %d",
			got.Seen, got.RanJS, produced.IPsSeen, produced.IPsRanJS)
	}
}
