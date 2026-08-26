package web

import (
	"context"
	"slices"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
)

// What this deployment's customer actually sees.
//
// The person who buys a website does not know what a TLS fingerprint is
// and has no reason to. Showing every customer the same twelve blocks
// means showing most of them several they cannot read - and a number
// nobody can interpret is worse than no number, because it invites a
// wrong conclusion instead of none.
//
// So the visible set is a per-site setting. The installer asks what this
// customer wants, turns those on, and the rest stay off until somebody
// asks. Three rules run through it:
//
// **Unset means the default, never a blank page.** Every deployment that
// existed before this setting is unset, and a page that emptied itself
// on upgrade would be the worst possible reading of "not configured".
//
// **"None" therefore has to be said out loud.** The first draft of this
// left it at unset-means-default and called the missing case a stated
// limit. A live run showed why that was wrong: a deployment running only
// the collector cannot turn the beacon sections off, so its customer
// gets six tables that all say "the snippet was never installed" - six
// blocks of noise, on the page whose whole purpose is not confusing
// somebody who does not know what a snippet is. The stored list holds
// ViewNone for that, which is a value a form can send and a database can
// keep, unlike an absence.
//
// **An id nobody knows is dropped and logged, never rendered.** These
// values come out of a database, and a page that trusted them would put
// a stored string into a catalog lookup and an API path.
//
// **A block that is off costs nothing.** Not merely hidden: its call is
// never made. A saving that stops at the template is not a saving, and
// this is the axis the whole setting exists for.

// ViewNone is the stored id meaning "show none of these".
//
// A reserved word rather than an empty list, because an empty list is
// what every deployment that predates this setting already has and those
// must keep drawing the default. The two states have to be different on
// disk, and this is the smaller change of the two ways to make them so.
//
// It is refused as a card or breakdown id by the registries themselves -
// neither has an entry called this - so a stored set that mixes it with
// real ids is not ambiguous: none wins, because a person who wrote
// "none" meant none.
const ViewNone = "yok"

// visibleSets is what one site's page draws.
type visibleSets struct {
	Cards      []cardID
	Breakdowns []analytics.BreakdownKind
}

// Empty reports whether this page has nothing at all to draw.
//
// A configuration nobody sensible picks, and it still has to be handled:
// the page says so in a sentence and points at the setting, rather than
// rendering a heading over blank space that reads as a fault.
func (v visibleSets) Empty() bool { return len(v.Cards) == 0 && len(v.Breakdowns) == 0 }

// visible resolves both sets for a site.
//
// Read together rather than one at a time because they answer one
// question - what does this page contain - and because the fetch below
// needs both before it can decide which summaries to ask for.
func (s *Server) visible(ctx context.Context, siteID string) visibleSets {
	return visibleSets{
		Cards:      s.visibleCards(ctx, siteID),
		Breakdowns: s.visibleBreakdowns(ctx, siteID),
	}
}

func (s *Server) visibleCards(ctx context.Context, siteID string) []cardID {
	return resolveCards(s.storedList(ctx, panel.KeyVisibleCards, siteID), func(name string) {
		// Stored and unknown: a card removed from the build, or a value
		// written by a version that had one this one does not. Dropped
		// rather than rendered, and logged rather than dropped silently -
		// a page quietly missing a block the customer chose is a support
		// call nobody can answer.
		s.logger().Warn("panel: unknown card in the visible set", "site", siteID, "card", name)
	})
}

func (s *Server) visibleBreakdowns(ctx context.Context, siteID string) []analytics.BreakdownKind {
	return resolveBreakdowns(s.storedList(ctx, panel.KeyVisibleBreakdowns, siteID), func(name string) {
		s.logger().Warn("panel: unknown breakdown in the visible set",
			"site", siteID, "breakdown", name)
	})
}

// resolveCards and resolveBreakdowns turn a stored list into a drawable
// one.
//
// Split from the reads above so the rules that matter - unset means the
// default, an unknown id is dropped, an all-unknown list still draws a
// page - are testable without a database standing behind them.
func resolveCards(raw []string, unknown func(string)) []cardID {
	if slices.Contains(raw, ViewNone) {
		return nil
	}
	out := make([]cardID, 0, len(raw))
	for _, name := range raw {
		id := cardID(name)
		if _, ok := cards[id]; !ok {
			unknown(name)
			continue
		}
		if slices.Contains(out, id) {
			continue
		}
		out = append(out, id)
	}
	if len(out) == 0 {
		// Unset, or every stored id unknown. The default is a better
		// answer than an empty page; the warnings say why it happened.
		return defaultCards
	}
	return out
}

func resolveBreakdowns(raw []string, unknown func(string)) []analytics.BreakdownKind {
	if slices.Contains(raw, ViewNone) {
		return nil
	}
	out := make([]analytics.BreakdownKind, 0, len(raw))
	for _, name := range raw {
		kind := analytics.BreakdownKind(name)
		if _, ok := breakdownDefs[kind]; !ok {
			unknown(name)
			continue
		}
		if slices.Contains(out, kind) {
			continue
		}
		out = append(out, kind)
	}
	if len(out) == 0 {
		return defaultBreakdowns
	}
	return out
}

// storedList reads a site-scoped string list, treating any failure as
// unset.
//
// Unset rather than an error, because the alternative is a dashboard
// that refuses to draw when one preference cannot be read. The customer
// wants their numbers; which blocks they picked is the less important
// half of the page.
func (s *Server) storedList(ctx context.Context, key panel.Key, siteID string) []string {
	if s.Store == nil {
		return nil
	}
	value, err := s.Store.GetSetting(ctx, key, siteID)
	if err != nil {
		s.logger().Error("panel: reading the visible set", "err", err, "key", key, "site", siteID)
		return nil
	}
	list, ok := value.([]string)
	if !ok {
		return nil
	}
	return list
}

// request turns the visible sets into the calls the page will make.
//
// This is where "a block that is off costs nothing" becomes true. A
// breakdown that is not shown is not requested, and a summary is
// requested only when something on the page reads it.
//
// The beacon summary has two readers and both have to be counted: the
// beacon cards, and *every* breakdown, because a row's share is a
// percentage of that summary. Skipping it while drawing a breakdown
// would leave every share as a dash - not a wrong number, but a column
// that silently emptied, which is its own kind of wrong.
func (v visibleSets) request(limit int) analytics.SiteRequest {
	req := analytics.SiteRequest{
		Breakdowns: make([]analytics.BreakdownRequest, 0, len(v.Breakdowns)),
	}
	for _, kind := range v.Breakdowns {
		req.Breakdowns = append(req.Breakdowns, analytics.BreakdownRequest{Kind: kind, Limit: limit})
	}
	for _, id := range v.Cards {
		switch cards[id].Source {
		case sourceTraffic:
			req.Traffic = true
		case sourceBeacon:
			req.Beacon = true
		}
	}
	// Each breakdown turns on the summary it divides by, rather than all
	// of them turning on the beacon's. See summaryFlags.
	//
	// This was `if len(v.Breakdowns) > 0 { req.Beacon = true }` while every
	// breakdown was beacon-sourced, with a note saying it had to read the
	// breakdown's own source once that stopped being true. D3 made it stop
	// being true, and the test that said so is
	// TestEveryBreakdownAsksForTheSummaryItDividesBy.
	//
	// Getting this wrong does not fail loudly. A collector breakdown whose
	// traffic summary was never fetched still draws every row; only the
	// share column empties to dashes, because the denominator came back a
	// legitimate zero.
	traffic, beacon := summaryFlags(v.Breakdowns...)
	req.Traffic = req.Traffic || traffic
	req.Beacon = req.Beacon || beacon
	return req
}

// summaryFlags says which summaries a set of breakdowns divides by.
//
// One function with two callers - request() for the site page and
// detailData for a breakdown's own page - because they had drifted
// already. detailData hardcoded the beacon summary, which was right for
// all six D2 breakdowns and silently wrong for D3's three: every row and
// count drew fine, and only the share column emptied to dashes, because a
// summary nobody asked for comes back a legitimate zero rather than an
// error.
//
// An unknown metric turns nothing on, deliberately. The alternative is
// guessing a denominator, and a share computed against the wrong total is
// worse than no share at all: the dash says "not known", a number says
// something false.
func summaryFlags(kinds ...analytics.BreakdownKind) (traffic, beacon bool) {
	for _, kind := range kinds {
		switch breakdownDefs[kind].Metric {
		case metricAddresses:
			traffic = true
		case metricPageviews, metricEvents:
			beacon = true
		}
	}
	return traffic, beacon
}
