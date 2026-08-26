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

	// The three below come from the collector rather than the beacon, and
	// count *addresses* rather than pageviews. They are what the panel
	// shows in developer mode; see the web package's registry.

	// BreakdownFingerprints groups by JA4 TLS fingerprint: which client
	// software connected, whether or not it ran any JavaScript.
	BreakdownFingerprints BreakdownKind = "parmak-izi"
	// BreakdownASNs groups by the network an address belongs to, which is
	// how a datacentre's traffic separates from a phone network's.
	BreakdownASNs BreakdownKind = "asn"
	// BreakdownServerCountries groups the *collector's* countries: every
	// address that reached the server, including everything that never
	// ran the beacon.
	//
	// Deliberately a different kind from BreakdownCountries rather than a
	// mode of it. They answer different questions - addresses that
	// arrived, versus people who opened a page - and the API's own route
	// comment warns that offering both under one name invites a panel to
	// compare them. Two kinds means two sections, two headings, and no
	// way to draw one while labelling it the other.
	BreakdownServerCountries BreakdownKind = "sunucu-ulke"
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

	// Bots is set only for the collector-side breakdowns: how many of
	// Count's addresses scored as bots.
	//
	// A separate field rather than a reuse of Visitors, even though both
	// are "the second number in the row". Visitors means distinct people
	// behind Count; this means addresses that scored above the bot
	// threshold. Putting one in the other's field would be a lie that
	// renders perfectly - the table would draw, the column would have a
	// heading, and the number under it would answer a different question
	// than the heading asks.
	//
	// The same reasoning is why Count is not renamed: for a collector
	// breakdown it is unique addresses, for a beacon one it is pageviews,
	// and what it is called on screen comes from the panel's registry.
	Bots int
	// Label is a human-readable name for Key where the API resolved one:
	// an ASN's organisation, or the bot behind a known fingerprint.
	// Empty when there is none, which is not the same as unknown - see
	// Empty.
	Label string
	// KnownBot marks a fingerprint the scoring package recognises. Kept
	// apart from Label being non-empty because an ASN also has a label
	// and is not a bot.
	KnownBot bool
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
	// path is the segment after /api/v1/sites/{site}/, including the
	// "beacon/" prefix where there is one.
	//
	// It used to be the segment after a hardcoded "/beacon/", which was
	// right while every breakdown came from the beacon. The collector's
	// own breakdowns (D3) sit directly under the site, so the prefix
	// moved into the table where it is visible per row rather than
	// implied for all of them.
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
	BreakdownPages:     {path: "beacon/pages", key: "pages", decode: decodeGroups},
	BreakdownReferrers: {path: "beacon/referrers", key: "referrers", decode: decodeGroups},
	BreakdownDevices:   {path: "beacon/devices", key: "devices", decode: decodeGroups},
	BreakdownCountries: {path: "beacon/countries", key: "countries", decode: decodeGroups},
	BreakdownCampaigns: {path: "beacon/campaigns", key: "campaigns", decode: decodeCampaigns},
	BreakdownEvents:    {path: "beacon/events", key: "events", decode: decodeEvents},

	// The collector's own breakdowns. Note BreakdownServerCountries and
	// BreakdownCountries answer under the same envelope key from
	// different paths - which is exactly why path and key are separate
	// fields rather than one.
	BreakdownFingerprints:    {path: "ja4", key: "ja4", decode: decodeJA4s},
	BreakdownASNs:            {path: "asns", key: "asns", decode: decodeASNGroups},
	BreakdownServerCountries: {path: "countries", key: "countries", decode: decodeAddressGroups},
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

// addressGroupRow is the collector's GroupStat: one country's or one
// ASN's share of a site's *addresses*.
type addressGroupRow struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	UniqueIPs int    `json:"unique_ips"`
	BotIPs    int    `json:"bot_ips"`
}

// ja4Row is the collector's JA4Stat.
type ja4Row struct {
	JA4           string `json:"ja4"`
	Label         string `json:"label"`
	IsKnownBotJA4 bool   `json:"is_known_bot_ja4"`
	UniqueIPs     int    `json:"unique_ips"`
	BotIPs        int    `json:"bot_ips"`
	Empty         bool   `json:"empty"`
}

// decodeASNGroups reads the ASN rows.
//
// Separate from decodeAddressGroups for one character: the unresolved
// group arrives as "0" here and as "" there, because the API selects
// asn::text out of an INTEGER column that defaults to 0, while country is
// TEXT defaulting to ”. Both mean "never determined" and neither is a
// real group.
//
// Discovered by a test rather than by reading: sharing the decoder drew
// the unresolved ASNs as a group literally named 0, which looks like a
// network number and is not one. Writing it as two functions rather than
// a flag keeps the reason next to the code that needs it.
func decodeASNGroups(raw json.RawMessage) ([]Row, error) {
	rows, err := decodeAddressGroups(raw)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Key == "0" {
			rows[i].Empty = true
		}
	}
	return rows, nil
}

// decodeAddressGroups reads the collector's country and ASN rows.
//
// Count is unique addresses, not pageviews, and Visitors stays zero -
// the collector cannot know how many people are behind an address, and
// filling that field with the address count would put a plausible number
// under a heading that asks a different question.
//
// Empty is derived from an absent key rather than read from a flag: the
// collector's GroupStat has no Empty field, and its doc says addresses
// whose country never resolved are grouped under an empty key rather
// than dropped. So the flag is what an empty key means, and deriving it
// here keeps that knowledge in one place instead of in every caller that
// draws a row.
func decodeAddressGroups(raw json.RawMessage) ([]Row, error) {
	var in []addressGroupRow
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := make([]Row, 0, len(in))
	for _, r := range in {
		out = append(out, Row{
			Key: r.Key, Label: r.Label, Empty: r.Key == "",
			Count: r.UniqueIPs, Bots: r.BotIPs,
		})
	}
	return out, nil
}

// decodeJA4s reads the fingerprint rows. Unlike the address groups this
// endpoint does carry an explicit Empty flag, for traffic that had no
// usable fingerprint at all - plaintext HTTP, or a ClientHello that could
// not be parsed in time.
func decodeJA4s(raw json.RawMessage) ([]Row, error) {
	var in []ja4Row
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	out := make([]Row, 0, len(in))
	for _, r := range in {
		out = append(out, Row{
			Key: r.JA4, Label: r.Label, Empty: r.Empty, KnownBot: r.IsKnownBotJA4,
			Count: r.UniqueIPs, Bots: r.BotIPs,
		})
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
	err := c.get(ctx, "/api/v1/sites/"+url.PathEscape(site)+"/"+spec.path, from, to, q, &env)
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
