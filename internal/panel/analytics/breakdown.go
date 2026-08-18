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

// Breakdowns: the grouped lists behind a summary number.
//
// A dashboard card says how many pageviews there were. A breakdown says
// which pages. The API has close to thirty of these; this package
// carries the ones the panel draws, as a closed set, because the kind
// becomes a path segment in a request to another service and a
// request-derived string must never do that.

// BreakdownKind names one grouped list.
//
// The set is closed and it is closed here rather than in the web
// package, because the fact it encodes is a transport fact: which path
// to call, which key the rows arrive under, and which of the API's row
// shapes they are. What a breakdown is *called* on screen belongs to the
// panel's registry; what it *is* belongs here.
type BreakdownKind string

const (
	// BreakdownPages groups by path: which pages were read.
	BreakdownPages BreakdownKind = "sayfa"
	// BreakdownReferrers groups by referring host: where people arrived
	// from.
	BreakdownReferrers BreakdownKind = "kaynak"
	// BreakdownCampaigns groups by the utm_* parameters a visit carried.
	BreakdownCampaigns BreakdownKind = "kampanya"
	// BreakdownDevices groups by device class.
	BreakdownDevices BreakdownKind = "cihaz"
	// BreakdownCountries groups the *beacon's* countries - people who
	// rendered a page.
	//
	// The API also has /countries, which counts addresses the collector
	// saw. They are different populations and the API's own routing
	// deliberately keeps them under different paths, so naming this one
	// "countries" without saying which is exactly the confusion that
	// separation exists to prevent. The panel says so on the section
	// heading; this comment says so to whoever adds the collector's one.
	BreakdownCountries BreakdownKind = "ulke"
	// BreakdownEvents groups by custom event name.
	BreakdownEvents BreakdownKind = "olay"
)

// Row is one line of a breakdown.
//
// Four of the API's row shapes flatten into this one, which is a
// rendering decision rather than a claim that they measure the same
// thing: Count is pageviews for pages and occurrences for events, and
// what it is called on screen comes from the panel's registry, not from
// here. Naming the field for one of its meanings would make the other
// wrong at the point somebody reads it.
type Row struct {
	// Key is the grouping value: a path, a host, an event name. Empty
	// for the group below.
	Key string
	// Empty marks the group where the value was never determined: a
	// direct visit with no referrer, an unrecognised browser, an
	// unresolved country.
	//
	// Carried rather than dropped, because the API deliberately does not
	// drop it - the groups have to add up to the site's total. A panel
	// that discarded these rows would show a table whose numbers are
	// quietly short, and a panel that drew them with a blank label would
	// show a row nobody can read. It gets a name instead.
	Empty bool
	// Count is the breakdown's own headline number.
	Count int
	// Visitors is how many distinct people are behind Count, which is
	// what separates three hundred clicks from one person from three
	// hundred people clicking once.
	Visitors int
	// Params is set only for campaigns: the utm_* parameters decoded out
	// of Key by the API, so a panel does not re-implement query parsing.
	Params map[string]string
}

// Breakdown is one grouped list as fetched.
//
// Err is carried per breakdown rather than returned, because six of
// these are fetched for one page and one failing is not the page
// failing. A section whose call did not answer says so; the other five
// still show their numbers.
type Breakdown struct {
	Kind BreakdownKind
	Rows []Row
	// Total is how many distinct groups exist, not the sum of Count.
	// It is what a pager needs and what a "showing 8 of 143" line reads.
	Total  int
	Limit  int
	Offset int
	Err    error
}

// BreakdownRequest asks for one breakdown, one page of it.
type BreakdownRequest struct {
	Kind   BreakdownKind
	Limit  int
	Offset int
}

// SiteRequest is everything one page wants fetched.
//
// The two summaries are flags rather than always-on because a
// deployment chooses which blocks its customer sees, and a call whose
// answer nothing draws is a query somebody's database runs for nothing.
// That is the whole point of making the visible set configurable: the
// saving has to reach the database, not stop at the template.
//
// A summary that was not asked for comes back zero with a nil error,
// which is indistinguishable from "installed, nothing in this period" -
// so a caller must not skip one whose numbers it then divides by. The
// panel decides that from its own registries; see the web package.
type SiteRequest struct {
	Traffic    bool
	Beacon     bool
	Breakdowns []BreakdownRequest
}

// Site is everything one panel page needs from the API.
//
// The summaries come along with the breakdowns because a share needs a
// denominator, and the denominator has to come from the same call shape
// that produced the rows - see the bots note on breakdownQuery.
type Site struct {
	Dashboard
	Breakdowns map[BreakdownKind]Breakdown
}

// breakdownSpec is what fetching one kind requires.
type breakdownSpec struct {
	// path is the segment after /beacon/.
	path string
	// key is the envelope field the rows arrive under.
	//
	// Separate from path because the API's two naming conventions do not
	// always agree - /beacon/operating-systems answers under
	// "operating_systems", /beacon/entry-pages under "entry_pages". None
	// of the six below differ, and the pair is kept anyway so that the
	// first one that does is a table edit rather than a bug.
	key string
	// decode turns that field's JSON into rows.
	decode func(json.RawMessage) ([]Row, error)
}

var breakdowns = map[BreakdownKind]breakdownSpec{
	BreakdownPages:     {path: "pages", key: "pages", decode: decodeGroups},
	BreakdownReferrers: {path: "referrers", key: "referrers", decode: decodeGroups},
	BreakdownDevices:   {path: "devices", key: "devices", decode: decodeGroups},
	BreakdownCountries: {path: "countries", key: "countries", decode: decodeGroups},
	BreakdownCampaigns: {path: "campaigns", key: "campaigns", decode: decodeCampaigns},
	BreakdownEvents:    {path: "events", key: "events", decode: decodeEvents},
}

// KnownBreakdown reports whether a kind is one this client can fetch.
//
// Exported so the panel can refuse an unknown path segment before it
// reaches a URL, rather than discovering it in an API 404.
func KnownBreakdown(kind BreakdownKind) bool {
	_, ok := breakdowns[kind]
	return ok
}

// The API's row shapes, decoded only as far as the panel draws them.
type groupRow struct {
	Key       string `json:"key"`
	Pageviews int    `json:"pageviews"`
	Visitors  int    `json:"visitors"`
	Empty     bool   `json:"empty"`
}

type campaignRow struct {
	Key       string            `json:"key"`
	Params    map[string]string `json:"params"`
	Pageviews int               `json:"pageviews"`
	Visitors  int               `json:"visitors"`
}

type eventRow struct {
	Name     string `json:"name"`
	Count    int    `json:"count"`
	Visitors int    `json:"visitors"`
}

func decodeGroups(raw json.RawMessage) ([]Row, error) {
	var in []groupRow
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := make([]Row, 0, len(in))
	for _, r := range in {
		out = append(out, Row{Key: r.Key, Empty: r.Empty, Count: r.Pageviews, Visitors: r.Visitors})
	}
	return out, nil
}

func decodeCampaigns(raw json.RawMessage) ([]Row, error) {
	var in []campaignRow
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := make([]Row, 0, len(in))
	for _, r := range in {
		// No Empty here, and that is the endpoint's shape rather than an
		// omission: BeaconCampaigns groups by the stored campaign query
		// and its SQL says WHERE query <> '', so untagged traffic is not
		// a flagged group - it is not returned at all.
		//
		// The consequence is real and belongs on the page rather than
		// here: unlike every other breakdown, these rows do not add up
		// to the site's total, so a campaigns table showing 2 of 11
		// pageviews is correct and looks broken. The section's help text
		// says so.
		out = append(out, Row{
			Key: r.Key, Params: r.Params,
			Count: r.Pageviews, Visitors: r.Visitors,
		})
	}
	return out, nil
}

func decodeEvents(raw json.RawMessage) ([]Row, error) {
	var in []eventRow
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := make([]Row, 0, len(in))
	for _, r := range in {
		out = append(out, Row{Key: r.Name, Empty: r.Name == "", Count: r.Count, Visitors: r.Visitors})
	}
	return out, nil
}

// FetchSite reads both summaries and every requested breakdown, all
// concurrently and all under one deadline.
//
// One call rather than a summary call followed by a breakdown call: a
// page that made two rounds would be bounded by twice PageTimeout while
// looking, in the code, like it was bounded by one.
func (c *Client) FetchSite(ctx context.Context, site string, from, to time.Time,
	want SiteRequest) Site {

	out := Site{Breakdowns: make(map[BreakdownKind]Breakdown, len(want.Breakdowns))}
	if !c.Configured() {
		// Only what was asked for is reported unavailable. A summary
		// nobody wanted is not a failure, and saying it failed would put
		// a page-wide warning on a page that is fine.
		if want.Traffic {
			out.TrafficErr = ErrUnavailable
		}
		if want.Beacon {
			out.BeaconErr = ErrUnavailable
		}
		for _, req := range want.Breakdowns {
			out.Breakdowns[req.Kind] = Breakdown{
				Kind: req.Kind, Limit: req.Limit, Offset: req.Offset, Err: ErrUnavailable,
			}
		}
		return out
	}

	ctx, cancel := context.WithTimeout(ctx, PageTimeout)
	defer cancel()

	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	if want.Traffic {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out.Traffic, out.TrafficErr = c.summary(ctx, site, from, to)
		}()
	}
	if want.Beacon {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out.Beacon, out.BeaconErr = c.beaconSummary(ctx, site, from, to)
		}()
	}
	for _, req := range want.Breakdowns {
		wg.Add(1)
		go func(req BreakdownRequest) {
			defer wg.Done()
			b := c.breakdown(ctx, site, req, from, to)
			// The map is the only shared write; the two summaries above
			// touch distinct fields of a struct nobody else reads until
			// Wait returns.
			mu.Lock()
			out.Breakdowns[req.Kind] = b
			mu.Unlock()
		}(req)
	}
	wg.Wait()
	return out
}

// breakdown fetches one grouped list.
func (c *Client) breakdown(ctx context.Context, site string, req BreakdownRequest,
	from, to time.Time) Breakdown {

	out := Breakdown{Kind: req.Kind, Limit: req.Limit, Offset: req.Offset}
	spec, ok := breakdowns[req.Kind]
	if !ok {
		// Not ErrUnavailable: nothing was unreachable. A kind that is not
		// in the registry is a programming error in the caller, and
		// reporting it as a transport failure would send somebody to
		// check a service that is fine.
		out.Err = fmt.Errorf("analytics: no such breakdown %q", req.Kind)
		return out
	}

	q := url.Values{}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	if req.Offset > 0 {
		q.Set("offset", strconv.Itoa(req.Offset))
	}
	// bots is deliberately not sent, on this call or on the summary. Both
	// take the API's default, which is what keeps a share and its
	// denominator counting the same population - see breakdownQuery.

	var env map[string]json.RawMessage
	err := c.get(ctx, "/api/v1/sites/"+url.PathEscape(site)+"/beacon/"+spec.path, from, to, q, &env)
	if err != nil {
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
		// A 200 with no rows field is something other than this endpoint
		// answering. Reported as unavailable because from the reader's
		// side that is what it is: no numbers.
		out.Err = fmt.Errorf("%w: the response has no %q field", ErrUnavailable, spec.key)
		return out
	}
	rows, err := spec.decode(raw)
	if err != nil {
		out.Err = fmt.Errorf("%w: %q is not the expected shape: %v", ErrUnavailable, spec.key, err)
		return out
	}
	out.Rows = rows
	return out
}

// breakdownQuery documents the one query parameter this package does not
// send, because leaving it out is a decision rather than an omission.
//
// The API's `bots` filter defaults to "exclude" on the beacon summary
// and on every breakdown alike. The panel sends it on neither, so both
// take that default and a row's share of the summary counts the same
// population as the summary does.
//
// Sending it on one and not the other - or letting one page set it -
// would produce a table whose percentages do not add to a hundred, drawn
// perfectly, with nothing on the page wrong except the numbers. A test
// holds the two defaults together for exactly that reason.
const breakdownQuery = "bots"
