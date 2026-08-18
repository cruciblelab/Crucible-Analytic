// Package analytics is how the panel reads traffic numbers.
//
// # Over HTTP, and that is the architecture rather than a preference
//
// The panel's database role has no access at all to the analytics
// tables. It is the process the customer's browser talks to, it holds
// accounts and sessions, and giving it read rights on every visitor
// record would make the widest-reachable component also the one with the
// broadest database access.
//
// So it asks the read-only API, over HTTP, with a bearer token - the
// same way an external dashboard would. That is not a workaround for the
// missing grant; the missing grant is the point, and this package is
// what makes living without it possible. A structural test refuses any
// import of the analytics store from the panel's tree.
//
// # The token reads every site, and the consequence is the panel's
//
// One token, granted every site the deployment has, because the panel
// serves every site. What keeps one customer's numbers away from another
// is entirely the panel's own authorisation - the same shape as its
// database pool: a broad credential, narrowed per request.
//
// Stated here because it is the kind of property that is obvious while
// it is being written and invisible a year later: a bug in the panel's
// site-access check does not merely show somebody the wrong page, it
// shows them another customer's traffic.
//
// # Failing usefully
//
// A dashboard whose data source is down should say the numbers have not
// arrived, not that something went wrong. The three states a caller has
// to be able to tell apart are unreachable, refused, and no such site,
// because each sends the reader somewhere different: wait, fix the
// token, check the site id.
package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Timeouts.
//
// A panel page is a person waiting at a screen. Better to draw the page
// with a sentence about the data source than to hold the request until
// a browser gives up - so the deadline here is short enough that a
// stuck API becomes a visible message rather than a blank tab.
const (
	// RequestTimeout bounds one call.
	RequestTimeout = 5 * time.Second
	// PageTimeout bounds a whole dashboard, however many calls it makes.
	// Not a multiple of RequestTimeout: the calls run concurrently, so a
	// page should not take longer than one slow call.
	PageTimeout = 8 * time.Second
	// maxResponseBytes caps what will be read from the API.
	//
	// The API is not hostile, but it is a separate process that can be
	// upgraded independently, misconfigured, or replaced by something
	// else listening on that port. A summary is a few hundred bytes;
	// anything approaching this is not a summary.
	maxResponseBytes = 4 << 20
)

// Errors a caller has to be able to tell apart.
var (
	// ErrUnavailable means the API could not be reached or did not
	// answer in time. The numbers exist; nothing fetched them.
	ErrUnavailable = errors.New("analytics: the read API could not be reached")
	// ErrRefused means the API answered and said no: a token that is
	// wrong, expired, or does not cover this site.
	//
	// Separate from ErrUnavailable because the reader's next step is
	// different in kind - one is "wait", the other is "somebody has to
	// fix the configuration" - and a page that showed the same sentence
	// for both would send an operator to watch a service that is fine.
	ErrRefused = errors.New("analytics: the read API refused this token")
	// ErrNoSite means the API has never heard of this site.
	ErrNoSite = errors.New("analytics: the read API has no such site")
)

// Client reads the analytics API.
//
// The zero value is not usable; see New. A nil *Client is, deliberately:
// every method answers ErrUnavailable, so a panel configured without an
// API address renders its "numbers unavailable" state rather than
// panicking. A deployment mid-installation is exactly that state.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// New builds a client for an API base address.
//
// An empty address returns a nil Client rather than an error. Group D
// arrived after group C, so there are deployments configured before
// analytics_api_url existed - and a panel that refuses to start because
// of a field nobody has filled in yet would make an upgrade an outage.
func New(base, token string) (*Client, error) {
	base = strings.TrimSpace(base)
	if base == "" {
		return nil, nil
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("analytics: %q is not a URL: %w", base, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("analytics: %q must start with http:// or https://", base)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("analytics: %q names no host", base)
	}
	return &Client{
		base:  u,
		token: strings.TrimSpace(token),
		http:  &http.Client{Timeout: RequestTimeout},
	}, nil
}

// Configured reports whether this client can reach anything. False for a
// nil client, which is what a panel with no API address gets.
func (c *Client) Configured() bool { return c != nil }

// Summary is the collector's headline numbers for one site.
//
// Only the fields the panel draws. Decoding the API's whole struct would
// tie this package to every field that service ever adds, and the two
// are deliberately separate processes that can be upgraded apart.
type Summary struct {
	SiteID string    `json:"site_id"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`

	UniqueIPs int `json:"unique_ips"`
	BotIPs    int `json:"bot_ips"`
	HumanIPs  int `json:"human_ips"`

	PeakRequestRate    float64 `json:"peak_request_rate"`
	PeakWindowRequests int     `json:"peak_window_requests"`

	// Snapshots is how many rows backed these numbers. Zero is the
	// signal the panel needs most: it distinguishes "this site has no
	// traffic in this range" from "nothing has ever been collected here".
	Snapshots int `json:"snapshots"`
}

// BeaconSummary is the client-side numbers for one site.
type BeaconSummary struct {
	SiteID string    `json:"site_id"`
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`

	Pageviews int `json:"pageviews"`
	Events    int `json:"events"`
	Visitors  int `json:"visitors"`
	Sessions  int `json:"sessions"`

	BounceRate        float64 `json:"bounce_rate"`
	PagesPerSession   float64 `json:"pages_per_session"`
	AvgSessionSeconds float64 `json:"avg_session_seconds"`
}

// Dashboard is both summaries for one site over one range.
//
// The two errors are kept rather than collapsed, because the two sources
// fail independently and mean different things when they do. A site with
// a collector and no beacon snippet is an ordinary, correct deployment;
// reporting that as a page-wide failure would tell a customer something
// is broken when nothing is.
type Dashboard struct {
	Traffic    Summary
	TrafficErr error
	Beacon     BeaconSummary
	BeaconErr  error
}

// FetchDashboard reads both summaries for a site, concurrently.
//
// Concurrent because a page should not cost the sum of its calls, and
// because the two are independent: neither answer changes what the other
// asks for. One deadline covers the pair, so a page has a bound whatever
// the API is doing.
func (c *Client) FetchDashboard(ctx context.Context, site string, from, to time.Time) Dashboard {
	var out Dashboard
	if !c.Configured() {
		out.TrafficErr, out.BeaconErr = ErrUnavailable, ErrUnavailable
		return out
	}

	ctx, cancel := context.WithTimeout(ctx, PageTimeout)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		out.Traffic, out.TrafficErr = c.summary(ctx, site, from, to)
	}()
	go func() {
		defer wg.Done()
		out.Beacon, out.BeaconErr = c.beaconSummary(ctx, site, from, to)
	}()
	wg.Wait()
	return out
}

// KnownSites lists the sites each source has ever written a row for.
//
// This is what separates the two emptinesses that look identical in a
// summary. Zero pageviews for a site the beacon has never heard of means
// the snippet was never embedded - a setup step nobody did, and fixable.
// Zero pageviews for a site it knows means nobody visited, which is a
// measurement. Showing the same sentence for both would present a
// missing installation as a result.
//
// Called only when a summary comes back empty. In the ordinary case -
// there are numbers - the answer would change nothing on the page, and a
// dashboard should not pay for a distinction it is not drawing.
func (c *Client) KnownSites(ctx context.Context) (traffic, beacon []string, err error) {
	if !c.Configured() {
		return nil, nil, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, PageTimeout)
	defer cancel()

	var (
		wg               sync.WaitGroup
		trafficErr, bErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		traffic, trafficErr = c.siteList(ctx, "/api/v1/sites")
	}()
	go func() {
		defer wg.Done()
		beacon, bErr = c.siteList(ctx, "/api/v1/beacon/sites")
	}()
	wg.Wait()

	// Either failing means the question cannot be answered, and
	// answering it wrongly is worse than not answering: the page would
	// tell somebody to install a snippet they already have.
	if trafficErr != nil {
		return nil, nil, trafficErr
	}
	if bErr != nil {
		return nil, nil, bErr
	}
	return traffic, beacon, nil
}

func (c *Client) siteList(ctx context.Context, path string) ([]string, error) {
	var body struct {
		Sites []string `json:"sites"`
	}
	// The site lists take no range; from and to are ignored by those
	// endpoints, and sending them keeps one code path rather than two.
	now := time.Now()
	if err := c.get(ctx, path, now.Add(-time.Hour), now, &body); err != nil {
		return nil, err
	}
	return body.Sites, nil
}

func (c *Client) summary(ctx context.Context, site string, from, to time.Time) (Summary, error) {
	var out Summary
	err := c.get(ctx, "/api/v1/sites/"+url.PathEscape(site)+"/summary", from, to, &out)
	return out, err
}

func (c *Client) beaconSummary(ctx context.Context, site string, from, to time.Time) (BeaconSummary, error) {
	var out BeaconSummary
	err := c.get(ctx, "/api/v1/sites/"+url.PathEscape(site)+"/beacon/summary", from, to, &out)
	return out, err
}

// get performs one call and decodes it.
func (c *Client) get(ctx context.Context, path string, from, to time.Time, into any) error {
	endpoint := *c.base
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + path
	endpoint.RawQuery = url.Values{
		"from": {from.UTC().Format(time.RFC3339)},
		"to":   {to.UTC().Format(time.RFC3339)},
	}.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// Every transport failure is one state as far as the reader is
		// concerned: nothing fetched the numbers. The underlying error
		// is wrapped for the log, not for the page - it names hosts and
		// ports, which is operator detail rather than customer detail.
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w (%s)", ErrRefused, resp.Status)
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w (%s)", ErrNoSite, resp.Status)
	default:
		return fmt.Errorf("%w: %s", ErrUnavailable, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%w: reading the response: %v", ErrUnavailable, err)
	}
	if len(body) == maxResponseBytes {
		return fmt.Errorf("%w: the response is larger than %d bytes, which is not a summary",
			ErrUnavailable, maxResponseBytes)
	}
	if err := json.Unmarshal(body, into); err != nil {
		// A 200 that does not decode is something other than this API
		// answering on that port. Reported as unavailable because from
		// the reader's side it is the same thing: no numbers.
		return fmt.Errorf("%w: the response is not the expected shape: %v", ErrUnavailable, err)
	}
	return nil
}
