package web

import (
	"net/http"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The pages that are only rendering: the site list, and the two sign-in
// forms. Everything with a decision in it lives beside the decision.

// siteRow is one entry in the signed-in landing page.
type siteRow struct {
	SiteID string
	Role   panel.Role
	// RoleLabel is the role in the reader's language. Rendered from the
	// catalog rather than the stored value, which is an identifier.
	RoleLabel string
	// ViaSuperadmin marks a site reachable by running the deployment
	// rather than by anybody granting a membership. Said out loud so an
	// operator cannot forget whose data is on screen.
	ViaSuperadmin bool
	// MembersURL is empty when this viewer may not manage members.
	MembersURL string
	// DashboardURL is where this site's numbers are. Never empty:
	// anybody who can see the row can see the numbers, which is what
	// having access to a site means.
	DashboardURL string
}

// sitesPage is Data for the landing page.
type sitesPage struct {
	Sites []siteRow
	// Empty means this account can reach nothing. Distinct from a
	// deployment with no sites at all, which is the operator's view of
	// the same emptiness and wants a different sentence.
	Empty      bool
	Superadmin bool
}

// sitesHandler is where signing in lands.
//
// It lists what this account can reach and nothing else. The analytics
// cards arrive with group D; until then this page says so rather than
// showing an empty frame that reads as a fault.
func (s *Server) sitesHandler(w http.ResponseWriter, r *http.Request, p panel.Principal) {
	lang := s.language(r)
	ctx := r.Context()

	switch r.Method {
	case http.MethodGet, http.MethodHead:
	default:
		w.Header().Set("Allow", "GET, HEAD")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
		return
	}

	// The known list is what the analytics side has seen collecting.
	// Nil for now: reading it means calling the read-only API, which
	// arrives with group D. A superadmin therefore sees sites somebody
	// has a membership on, and the wizard is still where a brand-new
	// site is introduced.
	sites, err := s.Store.Sites(ctx, p, nil)
	if err != nil {
		s.logger().Error("panel: listing sites", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	data := sitesPage{Superadmin: p.Superadmin, Empty: len(sites) == 0}
	for _, site := range sites {
		row := siteRow{
			SiteID:        site.SiteID,
			Role:          site.Role,
			ViaSuperadmin: site.ViaSuperadmin,
		}
		if site.Role != "" {
			row.RoleLabel = lang.T("rol." + string(site.Role))
		}
		// The link is drawn from the same capability the members handler
		// enforces, so the two cannot drift into showing a door that
		// does not open.
		if p.Superadmin || panel.RoleCan(site.Role, panel.CapManageMembers) {
			row.MembersURL = memberPath(site.SiteID)
		}
		row.DashboardURL = sitePath(site.SiteID)
		data.Sites = append(data.Sites, row)
	}

	page := s.page(r, lang, panel.Access{Principal: p}, "siteler", lang.T("siteler.baslik"))
	page.Data = data
	s.Renderer.Render(w, r, http.StatusOK, "siteler", page)
}

func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	status int, data loginPage) {

	s.Renderer.Render(w, r, status, "giris", &ui.Page{
		L:       lang,
		Title:   lang.T("giris.baslik"),
		Heading: lang.T("giris.baslik"),
		F:       ui.NewFormatter(lang, s.zone(r.Context())),
		CSRF:    s.Sessions.CSRFToken(r.Context()),
		Data:    data,
		// No navigation and no user block: there is nobody signed in to
		// name, and a header full of links nobody may follow is the
		// wrong first impression of a panel.
	})
}

func (s *Server) renderSecondFactor(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	status int, data loginPage) {

	s.Renderer.Render(w, r, status, "dogrulama", &ui.Page{
		L:       lang,
		Title:   lang.T("giris.dogrulama.baslik"),
		Heading: lang.T("giris.dogrulama.baslik"),
		F:       ui.NewFormatter(lang, s.zone(r.Context())),
		CSRF:    s.Sessions.CSRFToken(r.Context()),
		Data:    data,
	})
}
