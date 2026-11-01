package web

import (
	"context"
	"errors"
	"sync"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The beacon, site by site (B3g, catalog #29's other half).
//
// "JS verisi hiç gelmiyor" is the question this answers without a shell:
// for each site the person may see, when the snippet last reported. B3b
// put the beacon's refusals on this page, deployment-wide; this is the
// per-site half, so "the snippet on this site stopped yesterday at
// 14:02" reads off one row.
//
// # A time, never a count
//
// The catalog (#29) also asked for the last hour's count, and the API
// returns it. This page does not show it: the Health page's promise is
// that nothing on it is a number about a visitor
// (TestTheHealthPageCarriesNoVisitorNumbers), and an hourly event count
// per site is a traffic figure, unlike the per-process "written" counter
// beside it. The first version drew it; the check did not see it,
// because the check named five types by hand and these lived in a new
// file. The check now walks every type the page can reach. The time of
// the last event is kept: it says whether the snippet works, not how
// much traffic a site has - the same kind of fact as a service's last
// heartbeat. For the same reason the diagnostic file, which leaves the
// machine, carries none of this section (its omitted list says so).
//
// # Which sites
//
// The rule diagnosticLogs follows, for the same reason: an owner sees
// the sites they own and no other, the operator (a developer session
// carries superadmin authority) sees every site anybody has a membership
// for. A list that could not be read is not widened to "all" for want
// of an answer - the section says it could not decide, and shows none.
//
// # Never a stopped snippet for a failure
//
// A row whose read failed says so and carries no time. "No event yet"
// printed over an unreachable API would be the one sentence this section
// must not say, because it is also what a snippet nobody embedded looks
// like (outage_test.go's rule for every analytics page).

// maxActivitySites is how many sites the section asks about.
//
// Each is one request to the read API, 15-21 ms on 2M events (B3g's
// measurement), four at a time: fifty is under a quarter second, and a
// deployment with more sites than that is told how many were left out
// rather than kept waiting for them.
const maxActivitySites = 50

// activityWorkers is how many sites are asked about at once.
const activityWorkers = 4

// activityState is what one site's row says.
type activityState string

const (
	// activityReported: the site has sent events, and the row says when.
	activityReported activityState = "var"
	// activityNever: the API answered, and this site has never sent one.
	activityNever activityState = "hic"
	// activityUnreadable: the read failed, so the row carries no number.
	activityUnreadable activityState = "okunamadi"
)

// siteActivity is one site's answer, before any of it is worded.
type siteActivity struct {
	Site     string
	State    activityState
	Activity analytics.BeaconActivity
	Err      error
}

// beaconActivities asks the read API about each site this person may
// see. more is how many sites past the cap were not asked about.
func (s *Server) beaconActivities(ctx context.Context, p panel.Principal) (rows []siteActivity, more int, err error) {
	if !s.Analytics.Configured() {
		return nil, 0, analytics.ErrUnavailable
	}
	sites, err := s.activitySites(ctx, p)
	if err != nil {
		return nil, 0, err
	}
	if len(sites) > maxActivitySites {
		more = len(sites) - maxActivitySites
		sites = sites[:maxActivitySites]
	}

	rows = make([]siteActivity, len(sites))
	next := make(chan int)
	var wg sync.WaitGroup
	for range activityWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				rows[i] = s.oneSiteActivity(ctx, sites[i])
			}
		}()
	}
	for i := range sites {
		next <- i
	}
	close(next)
	wg.Wait()
	return rows, more, nil
}

// oneSiteActivity is one site's read, classified.
func (s *Server) oneSiteActivity(ctx context.Context, site string) siteActivity {
	act, err := s.Analytics.BeaconActivity(ctx, site)
	switch {
	case err != nil:
		s.logger().Warn("panel: reading a site's beacon activity", "site", site, "err", err)
		return siteActivity{Site: site, State: activityUnreadable, Err: err}
	case act.LastEvent == nil:
		return siteActivity{Site: site, State: activityNever, Activity: act}
	default:
		return siteActivity{Site: site, State: activityReported, Activity: act}
	}
}

// activitySites is the sites this person may see, in order.
func (s *Server) activitySites(ctx context.Context, p panel.Principal) ([]string, error) {
	if !p.Superadmin {
		return s.ownedSites(ctx, p)
	}
	all, err := s.Store.Sites(ctx, p, nil)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(all))
	for _, site := range all {
		out = append(out, site.SiteID)
	}
	return out, nil
}

// beaconActivitySection is the Health page's section.
type beaconActivitySection struct {
	Configured bool
	Rows       []beaconActivityRow
	// More is how many sites were not asked about (maxActivitySites).
	More int
	// Error is set when the list of sites itself could not be read.
	Error string
}

// beaconActivityRow is one site, worded.
type beaconActivityRow struct {
	Site  string
	State activityState
	// LastEvent is when the newest event came, in words measured on the
	// database's clock; empty unless State is activityReported.
	LastEvent string
}

// beaconActivityFor builds the section for one viewer.
func (s *Server) beaconActivityFor(ctx context.Context, lang *ui.Language, p panel.Principal) beaconActivitySection {
	out := beaconActivitySection{Configured: s.Analytics.Configured()}
	if !out.Configured {
		return out
	}
	rows, more, err := s.beaconActivities(ctx, p)
	if err != nil {
		s.logger().Warn("panel: listing the sites for the beacon section", "err", err)
		out.Error = lang.T("saglik.beacon_site.liste_yok")
		if errors.Is(err, analytics.ErrUnavailable) {
			out.Error = lang.T("saglik.api.ulasilamadi")
		}
		return out
	}
	f := ui.NewFormatter(lang, s.zone(ctx))
	out.More = more
	for _, r := range rows {
		out.Rows = append(out.Rows, activityRowFrom(f, r))
	}
	return out
}

// activityRowFrom words one site's answer.
//
// "How long ago" against the database's clock, the one that stamped the
// event, never the panel's (the B3e lesson; see BeaconActivity.AsOf).
func activityRowFrom(f *ui.Formatter, r siteActivity) beaconActivityRow {
	row := beaconActivityRow{Site: r.Site, State: r.State}
	if r.State == activityReported {
		row.LastEvent = f.SinceAt(*r.Activity.LastEvent, r.Activity.AsOf)
	}
	return row
}
