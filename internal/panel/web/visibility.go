package web

import (
	"context"
	"net/http"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The wizard step that asks what the customer wants to look at.
//
// Every other step in this wizard is a technical question: is the schema
// applied, can the roles reach what they should, how long is data kept.
// This one is not. It is the installer sitting with the person who
// bought the website and asking which of these they actually want on
// their screen - and the honest default answer for most of them is "the
// four everybody recognises, not the two about bots".
//
// The blocks are offered in the customer's own words. The labels here
// are the same ones the dashboard draws, deliberately: an installer
// choosing from a list of internal ids would be choosing blind, and a
// second set of words for the same blocks is a second thing to keep in
// step.

// viewOption is one block offered for a site.
type viewOption struct {
	ID      string
	Label   string
	Help    string
	Checked bool
}

// siteVisibility is one site's row of choices.
type siteVisibility struct {
	SiteID string
	Name   string
	// Field names, so the template does not build them by string
	// concatenation and the handler does not have to guess what it did.
	CardField      string
	BreakdownField string
	Cards          []viewOption
	Breakdowns     []viewOption
}

// visibilityField names the form field carrying one site's choices.
//
// The site id is validated on the way in - letters, digits, underscore
// and dash, at most 64 characters - so it composes into a field name
// without escaping. Built in one place rather than at both ends, because
// a name that is written twice is a name that will differ once.
func visibilityField(prefix, siteID string) string { return prefix + ":" + siteID }

const (
	cardFieldPrefix      = "kart"
	breakdownFieldPrefix = "kirilim"
)

// visibilityRows builds the step's form.
//
// Pre-ticked with what the site shows *today*, which for an unconfigured
// deployment is the default set. That matters more than it looks: an
// installer who opens this step and saves without touching anything must
// get what was already there, not an empty page. Unticking everything is
// how you say "none", so a blank form saved by accident would be the
// worst possible misreading of a click.
func (s *Server) visibilityRows(ctx context.Context, lang *ui.Language) ([]siteVisibility, error) {
	value, err := s.Store.GetSetting(ctx, panel.KeyBeaconSites, "")
	if err != nil {
		return nil, err
	}
	sites := toStringList(value)

	rows := make([]siteVisibility, 0, len(sites))
	for _, siteID := range sites {
		shown := s.visible(ctx, siteID)

		row := siteVisibility{
			SiteID:         siteID,
			Name:           s.siteName(ctx, panel.Access{}, siteID),
			CardField:      visibilityField(cardFieldPrefix, siteID),
			BreakdownField: visibilityField(breakdownFieldPrefix, siteID),
		}
		for _, id := range defaultCardOrder() {
			row.Cards = append(row.Cards, viewOption{
				ID:      string(id),
				Label:   lang.T("pano.kart." + string(id) + ".baslik"),
				Help:    lang.T("pano.kart." + string(id) + ".aciklama"),
				Checked: containsCard(shown.Cards, id),
			})
		}
		for _, kind := range defaultBreakdowns {
			row.Breakdowns = append(row.Breakdowns, viewOption{
				ID:      string(kind),
				Label:   lang.T("pano.kirilim." + string(kind) + ".baslik"),
				Help:    lang.T("pano.kirilim." + string(kind) + ".aciklama"),
				Checked: containsKind(shown.Breakdowns, kind),
			})
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// defaultCardOrder is every card, in the order the default view uses.
//
// The offered list is every card this build has, not only the ones on by
// default - the point of the step is turning the others on.
func defaultCardOrder() []cardID {
	out := make([]cardID, 0, len(cards))
	out = append(out, defaultCards...)
	for id := range cards {
		if !containsCard(out, id) {
			out = append(out, id)
		}
	}
	return out
}

func containsCard(list []cardID, want cardID) bool {
	for _, id := range list {
		if id == want {
			return true
		}
	}
	return false
}

func containsKind(list []analytics.BreakdownKind, want analytics.BreakdownKind) bool {
	for _, kind := range list {
		if kind == want {
			return true
		}
	}
	return false
}

// saveVisible writes every site's choice.
func (s *Server) saveVisible(r *http.Request, lang *ui.Language, access panel.Access, data *setupPage) {
	rows, err := s.visibilityRows(r.Context(), lang)
	if err != nil {
		data.Message, data.Failed = s.settingProblem(lang, err), true
		return
	}

	for _, row := range rows {
		picked := map[panel.Key][]string{
			panel.KeyVisibleCards:      chosen(r, row.CardField, validCard),
			panel.KeyVisibleBreakdowns: chosen(r, row.BreakdownField, validBreakdown),
		}
		for key, value := range picked {
			if err := s.Store.SetSetting(r.Context(), key, row.SiteID, value, actorOf(access)); err != nil {
				// settingProblem rather than err.Error(): the same call
				// returns both the registry's installer-facing refusals
				// and wrapped database errors carrying query text.
				data.Message, data.Failed = s.settingProblem(lang, err), true
				return
			}
		}
	}
	// Re-read, so the form redraws what was actually stored rather than
	// what was posted. The two differ whenever a value was refused, and a
	// form that kept showing the rejected choice is how somebody comes to
	// believe a setting saved when it did not.
	if rows, err = s.visibilityRows(r.Context(), lang); err == nil {
		data.Visible = rows
	}
	data.Message = lang.T("kurulum.kaydedildi")
}

// chosen reads one checkbox group, keeping only ids the registry knows.
//
// Nothing ticked means ViewNone, and that is the whole reason "none" has
// a spelling: an empty list would be stored as unset, and unset means the
// default, so an installer who deliberately unticked everything would
// find all of it back on the next page load with nothing to explain why.
func chosen(r *http.Request, field string, valid func(string) bool) []string {
	var out []string
	for _, raw := range r.PostForm[field] {
		if !valid(raw) {
			// Not an error shown to the installer: the form only offers
			// real ids, so anything else was not typed by the person at
			// the screen. Dropped, and the stored set is what remains.
			continue
		}
		out = append(out, raw)
	}
	if len(out) == 0 {
		return []string{ViewNone}
	}
	return out
}

func validCard(raw string) bool {
	_, ok := cards[cardID(raw)]
	return ok
}

func validBreakdown(raw string) bool {
	_, ok := breakdownDefs[analytics.BreakdownKind(raw)]
	return ok
}
