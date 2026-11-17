package web

import (
	"context"
	"slices"
	"strings"

	"github.com/cruciblelab/crucible-analytic/internal/configreport"
	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/panel/preflight"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// What the services say about themselves, for the setup wizard (PLAN §W4,
// items 5 and 6).
//
// The wizard is the one reader of heartbeat.ReadConfig: the values are a
// deployment's plumbing - one customer's collector backend, say - and the
// wizard is opened only by somebody who speaks for the whole deployment
// (developerAccess). internal/invariants holds every other page to
// heartbeat.Read, which does not select them.

// beaconSitesView is what one beacon process says it accepts events for.
type beaconSitesView struct {
	// Reporter is the heartbeat row's key - beacon_writer.
	Reporter string
	Sites    []string
	// FromFile is true when the list is beacon.toml's sites: the panel
	// had none saved when the beacon last read its settings.
	FromFile bool
	// PanelHasList is true when the panel has a list saved now. With
	// FromFile it means the beacon has not read that list yet - a
	// different sentence from "the panel has no list", which the first
	// version said in both cases (a mutation that dropped FromFile from
	// beaconDisagrees survived and showed it).
	PanelHasList bool
}

// SitesText is the list as the page writes it.
func (v beaconSitesView) SitesText() string { return strings.Join(v.Sites, ", ") }

// wizardReports reads what the running services said.
func (s *Server) wizardReports(ctx context.Context) ([]heartbeat.ConfigReport, error) {
	return heartbeat.ReadConfig(ctx, s.Store.Pool())
}

// beaconSites is what each reporting beacon accepts, from its report.
//
// A beacon that has not applied its settings yet - the first minute after
// it starts - or a build older than the report says nothing, and is left
// out: the page then says the beacon has not told the panel, which is
// true, rather than guessing from the file.
func beaconSites(reports []heartbeat.ConfigReport) []beaconSitesView {
	var out []beaconSitesView
	for _, r := range reports {
		if r.Report.Program != configreport.Beacon {
			continue
		}
		source, ok := r.Report.Values[configreport.KeySitesSource]
		if !ok {
			continue
		}
		view := beaconSitesView{Reporter: r.Key(), FromFile: source == configreport.SourceFile}
		for _, site := range strings.Split(r.Report.Values[configreport.KeyAcceptedSites], ",") {
			if site = strings.TrimSpace(site); site != "" {
				view.Sites = append(view.Sites, site)
			}
		}
		out = append(out, view)
	}
	return out
}

// beaconDisagrees is the subset of views worth a sentence beside the
// panel's list: every one when the panel has no list (the beacon is then
// on its file, or on nothing), and otherwise the ones accepting something
// other than what the panel saved - entries it could not take, or a list
// it has not read yet.
func beaconDisagrees(panelSites []string, views []beaconSitesView) []beaconSitesView {
	if len(panelSites) == 0 {
		return views
	}
	var out []beaconSitesView
	for _, v := range views {
		if v.FromFile || !slices.Equal(v.Sites, panelSites) {
			v.PanelHasList = true
			out = append(out, v)
		}
	}
	return out
}

// handoverBlockers is every reason the handover is closed, for the page
// that draws the step and for the POST that would mint the invitation -
// one function, so the two cannot disagree.
//
// The required checks, and one more: no site list saved in the panel. The
// account the handover creates becomes owner of every site on that list
// (welcome.go), so with none saved it owns nothing - on a deployment
// whose beacon may be accepting two sites from beacon.toml all the same.
// Measured on the live rig: with the panel's list removed the beacon kept
// accepting both of its file's sites, the wizard's site steps said there
// were none, and nothing stood between that state and the handover.
func (s *Server) handoverBlockers(ctx context.Context, lang *ui.Language, results []preflight.CheckResult) ([]string, error) {
	var names []string
	if ok, blocking := preflight.Complete(results); !ok {
		for _, check := range blocking {
			names = append(names, check.Label)
		}
	}
	sites, err := s.configuredSites(ctx)
	if err != nil {
		return nil, err
	}
	if len(sites) == 0 {
		names = append(names, lang.T("kurulum.devir.site_yok"))
	}
	return names, nil
}

// loadBeaconSites fills the two fields the site steps draw beside the
// panel's list.
func (s *Server) loadBeaconSites(ctx context.Context, data *setupPage) error {
	reports, err := s.wizardReports(ctx)
	if err != nil {
		return err
	}
	views := beaconSites(reports)
	data.BeaconSilent = len(views) == 0
	data.BeaconSites = beaconDisagrees(data.Sites, views)
	return nil
}
