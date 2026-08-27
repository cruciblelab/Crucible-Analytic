package analytics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// The views that are not breakdowns.
//
// D3's other three: the score histogram, the cross-source summary, and
// the two address lists behind it. None of them is a grouped list of
// key-and-count, so none goes through breakdown.go's registry - forcing
// them into Row{Key, Count, Visitors} would throw away most of what they
// carry. What they share with the breakdowns is only that a panel asks
// for them and a failure of one is not a failure of the page.

// ScoreBand is one ten-point band of the bot-score histogram.
//
// All ten always arrive, empty ones included, because the API builds the
// range rather than returning only the bands that have rows. A histogram
// that silently omits its empty bands is a histogram with a different
// shape from the data.
type ScoreBand struct {
	Min       int
	Max       int
	Addresses int
}

// ScoreDistribution is the histogram plus whether fetching it worked.
type ScoreDistribution struct {
	Bands []ScoreBand
	Err   error
}

// Total is how many addresses the histogram covers.
func (s ScoreDistribution) Total() int {
	var n int
	for _, b := range s.Bands {
		n += b.Addresses
	}
	return n
}

// CoverageBand is one score band's JavaScript coverage.
type CoverageBand struct {
	Min       int
	Max       int
	Addresses int
	RanJS     int
}

// Crossover is what the collector saw against what the beacon heard.
//
// This is the measurement the product exists to make: a conventional
// analytics tool only ever sees the addresses that ran its JavaScript, so
// it cannot report the size of the population it is missing. This can.
type Crossover struct {
	Seen   int
	RanJS  int
	Silent int
	// Coverage is RanJS/Seen, and 0 when nothing was seen - which is not
	// the same as 0% coverage, so a caller must check Seen first.
	Coverage float64
	// BeaconOnly counts addresses the beacon heard and the collector
	// never saw. In a correct deployment it is zero, because a browser
	// that loaded the page necessarily connected through the collector
	// first. Non-zero is a configuration signal rather than a
	// measurement, and the panel says so rather than drawing it as a
	// number beside the others.
	BeaconOnly int
	Bands      []CoverageBand
	Err        error
}

// AddressRow is one address in the silent or JS-bot lists.
//
// The address here is whatever the deployment stores, which is never a
// whole one: privacy.ip_storage keeps a masked network, plus a keyed
// token in full mode. So this field cannot carry an individual's address
// even when a page draws it - see internal/privacy.
type AddressRow struct {
	Address   string
	PeakScore int
	Rate      float64
	Country   string
	ASN       int
	ASNName   string
	JA4       string
	JA4Label  string
	// Browser and OS are set only in the JS-bot list, where the beacon
	// heard a User-Agent. Silent addresses ran no JavaScript, so nothing
	// ever reported one.
	Browser string
	OS      string
	// BotUA marks a User-Agent that identified itself as automation,
	// which is a different claim from a high score: one is what the
	// client says about itself, the other is what its behaviour showed.
	BotUA bool
	Last  time.Time
}

// AddressList is one page of either list.
type AddressList struct {
	Rows   []AddressRow
	Total  int
	Limit  int
	Offset int
	Err    error
}

// AddressListKind names which of the two lists to fetch.
//
// A closed type for the same reason BreakdownKind is one: it decides a
// path segment in a request to another service.
type AddressListKind string

const (
	// ListSilent: addresses the collector saw that never ran any
	// JavaScript. The population a beacon-only tool cannot see at all.
	ListSilent AddressListKind = "sessiz"
	// ListJSBots: addresses that did run JavaScript and still look like
	// automation. The opposite blind spot - what a beacon counts as a
	// visitor.
	ListJSBots AddressListKind = "js-bot"
)

var addressLists = map[AddressListKind]struct{ path, key string }{
	ListSilent: {path: "crossover/silent-ips", key: "ips"},
	ListJSBots: {path: "crossover/js-bots", key: "bots"},
}

// KnownAddressList reports whether a kind is one this client can fetch,
// so a panel can refuse an unknown segment before it reaches a URL.
func KnownAddressList(kind AddressListKind) bool {
	_, ok := addressLists[kind]
	return ok
}

// TechnicalRequest asks for the developer-mode views of one site.
type TechnicalRequest struct {
	Scores    bool
	Crossover bool
	List      AddressListKind
	Limit     int
	Offset    int
}

// Technical is everything the developer-mode sections need.
type Technical struct {
	Scores    ScoreDistribution
	Crossover Crossover
	List      AddressList
}

// FetchTechnical gets whichever of the three the caller asked for,
// concurrently and under the caller's deadline.
//
// Concurrent for the same reason FetchSite is: these are three
// independent reads and doing them in series would make the page's
// latency their sum. Each carries its own error, so one endpoint being
// unreachable costs its own section rather than the page.
func (c *Client) FetchTechnical(ctx context.Context, site string, from, to time.Time,
	req TechnicalRequest) Technical {

	var out Technical
	if !c.Configured() {
		// Only what was asked for is reported unavailable, matching
		// FetchSite: a view nobody wanted is not a failure, and saying it
		// failed would put a warning on a page that is fine.
		if req.Scores {
			out.Scores.Err = ErrUnavailable
		}
		if req.Crossover {
			out.Crossover.Err = ErrUnavailable
		}
		if req.List != "" {
			out.List.Err = ErrUnavailable
		}
		return out
	}

	// One deadline for the whole page, as FetchSite does. Three
	// concurrent reads that each waited their own PageTimeout could make
	// a page take three times as long as the number that was chosen to
	// bound it.
	ctx, cancel := context.WithTimeout(ctx, PageTimeout)
	defer cancel()

	var wg sync.WaitGroup
	if req.Scores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out.Scores = c.scoreDistribution(ctx, site, from, to)
		}()
	}
	if req.Crossover {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out.Crossover = c.crossover(ctx, site, from, to)
		}()
	}
	if req.List != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out.List = c.addressList(ctx, site, req, from, to)
		}()
	}
	wg.Wait()
	return out
}

func (c *Client) scoreDistribution(ctx context.Context, site string, from, to time.Time) ScoreDistribution {
	var body struct {
		Buckets []struct {
			Min       int `json:"min"`
			Max       int `json:"max"`
			UniqueIPs int `json:"unique_ips"`
		} `json:"buckets"`
	}
	var out ScoreDistribution
	if err := c.get(ctx, "/api/v1/sites/"+url.PathEscape(site)+"/score-distribution",
		from, to, nil, &body); err != nil {
		out.Err = err
		return out
	}
	for _, b := range body.Buckets {
		out.Bands = append(out.Bands, ScoreBand{Min: b.Min, Max: b.Max, Addresses: b.UniqueIPs})
	}
	return out
}

func (c *Client) crossover(ctx context.Context, site string, from, to time.Time) Crossover {
	var body struct {
		IPsSeen       int     `json:"ips_seen"`
		IPsRanJS      int     `json:"ips_ran_js"`
		IPsSilent     int     `json:"ips_silent"`
		JSCoverage    float64 `json:"js_coverage"`
		BeaconOnlyIPs int     `json:"beacon_only_ips"`
		Bands         []struct {
			Min       int `json:"min"`
			Max       int `json:"max"`
			UniqueIPs int `json:"unique_ips"`
			RanJS     int `json:"ran_js"`
		} `json:"bands"`
	}
	var out Crossover
	if err := c.get(ctx, "/api/v1/sites/"+url.PathEscape(site)+"/crossover/summary",
		from, to, nil, &body); err != nil {
		out.Err = err
		return out
	}
	out.Seen, out.RanJS, out.Silent = body.IPsSeen, body.IPsRanJS, body.IPsSilent
	out.Coverage, out.BeaconOnly = body.JSCoverage, body.BeaconOnlyIPs
	for _, b := range body.Bands {
		out.Bands = append(out.Bands, CoverageBand{
			Min: b.Min, Max: b.Max, Addresses: b.UniqueIPs, RanJS: b.RanJS,
		})
	}
	return out
}

func (c *Client) addressList(ctx context.Context, site string, req TechnicalRequest,
	from, to time.Time) AddressList {

	out := AddressList{Limit: req.Limit, Offset: req.Offset}
	spec, ok := addressLists[req.List]
	if !ok {
		// Unreachable through the panel, which checks its own registry
		// first. Kept because the alternative is a path segment from
		// somewhere reaching another service's URL.
		out.Err = fmt.Errorf("%w: no such list %q", ErrUnavailable, req.List)
		return out
	}

	q := url.Values{}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	if req.Offset > 0 {
		q.Set("offset", strconv.Itoa(req.Offset))
	}

	var env map[string]json.RawMessage
	if err := c.get(ctx, "/api/v1/sites/"+url.PathEscape(site)+"/"+spec.path,
		from, to, q, &env); err != nil {
		out.Err = err
		return out
	}
	if raw, ok := env["total"]; ok {
		if err := json.Unmarshal(raw, &out.Total); err != nil {
			out.Err = fmt.Errorf("%w: total is not a number: %v", ErrUnavailable, err)
			return out
		}
	}
	raw, ok := env[spec.key]
	if !ok {
		out.Err = fmt.Errorf("%w: the response has no %q field", ErrUnavailable, spec.key)
		return out
	}

	// One shape for both lists: the JS-bot rows carry a browser and an
	// operating system that the silent rows cannot have, and the silent
	// rows carry a request rate the JS-bot rows do not. Decoding both
	// into one struct leaves the absent fields zero, which is what they
	// are - the alternative is two nearly identical types and two nearly
	// identical templates.
	var rows []struct {
		IP              string    `json:"ip"`
		PeakScore       int       `json:"peak_score"`
		PeakRequestRate float64   `json:"peak_request_rate"`
		Country         string    `json:"country"`
		ASN             int       `json:"asn"`
		ASNName         string    `json:"asn_name"`
		JA4             string    `json:"ja4"`
		JA4Label        string    `json:"ja4_label"`
		Browser         string    `json:"browser"`
		OS              string    `json:"os"`
		IsBotUA         bool      `json:"is_bot_ua"`
		LastSeen        time.Time `json:"last_seen"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		out.Err = fmt.Errorf("%w: %q is not the expected shape: %v", ErrUnavailable, spec.key, err)
		return out
	}
	for _, r := range rows {
		out.Rows = append(out.Rows, AddressRow{
			Address: r.IP, PeakScore: r.PeakScore, Rate: r.PeakRequestRate,
			Country: r.Country, ASN: r.ASN, ASNName: r.ASNName,
			JA4: r.JA4, JA4Label: r.JA4Label,
			Browser: r.Browser, OS: r.OS, BotUA: r.IsBotUA, Last: r.LastSeen,
		})
	}
	return out
}
