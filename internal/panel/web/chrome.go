package web

import (
	"errors"
	"net/http"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The signed-in shell: who is looking, what they may reach, and the
// helpers every page behind the login form is built from.
//
// One rule runs through all of it, and it is the reason this file is
// separate from the pages it serves:
//
// **Hiding a link is not authorisation.** Everything here decides what
// to *draw*. Not one function in this file is allowed to be the reason a
// request succeeds. Every handler asks for its own capability again,
// against the same Access, and refuses on its own - so a member who
// types a URL, replays a form, or reads the HTML for hrefs meets the
// same answer as one who clicked.

// requireUser resolves the signed-in principal or sends them to the
// login form.
//
// The redirect carries where they were going, so signing in lands them
// there rather than on the site list - the small courtesy that makes
// bookmarks and shared links work. See safeNext for why that parameter
// is the dangerous one on this page.
func (s *Server) requireUser(w http.ResponseWriter, r *http.Request) (panel.Principal, bool) {
	p, err := s.Sessions.Principal(r.Context())
	if err == nil {
		return p, true
	}
	if !errors.Is(err, panel.ErrNoSession) {
		s.logger().Error("panel: resolving principal", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, s.language(r))
		return panel.Principal{}, false
	}
	// Only ever the path, never the query string: it is where a
	// password-reset token or a search term would be, and this value
	// comes straight back out in a URL and an access log.
	http.Redirect(w, r, withNext(LoginPath, s.rawNext(r.URL.Path)), http.StatusSeeOther)
	return panel.Principal{}, false
}

// siteAccess resolves what this principal may do on a site, and refuses
// anybody with no business there.
//
// The refusal for "site you cannot see" is 404, not 403. A 403 confirms
// the site exists, which turns the URL into a way to enumerate a
// deployment's customers from any account on it.
func (s *Server) siteAccess(w http.ResponseWriter, r *http.Request, p panel.Principal, siteID string) (panel.Access, bool) {
	lang := s.language(r)
	access, err := s.Store.AccessFor(r.Context(), p, siteID)
	if err != nil {
		s.logger().Error("panel: resolving site access", "err", err, "site", siteID)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return panel.Access{}, false
	}
	if !access.Can(panel.CapViewAnalytics) {
		s.Renderer.ErrorIn(w, r, http.StatusNotFound, lang)
		return panel.Access{}, false
	}
	return access, true
}

// require refuses a principal who lacks a capability on this site.
//
// Separate from siteAccess because the two answers differ: not being
// able to see a site at all is a 404, and being a viewer on a page only
// admins may open is a 403. The second is honest - the reader knows the
// site exists, they are looking at it - and telling them "this needs a
// role you do not have" beats a page that pretends not to exist.
func (s *Server) require(w http.ResponseWriter, r *http.Request, access panel.Access, c panel.Capability) bool {
	if access.Can(c) {
		return true
	}
	s.Renderer.ErrorIn(w, r, http.StatusForbidden, s.language(r))
	return false
}

// page builds the chrome every signed-in page shares.
//
// The navigation is filtered by capability here, and that filtering is
// cosmetic on purpose: it exists so nobody is shown a door that will not
// open, not so that the door is locked. The lock is in each handler.
func (s *Server) page(r *http.Request, lang *ui.Language, access panel.Access, current, title string) *ui.Page {
	p := &ui.Page{
		L:       lang,
		Title:   title,
		Heading: title,
		F:       ui.NewFormatter(lang, s.zone(r.Context())),
		CSRF:    s.Sessions.CSRFToken(r.Context()),
		User: ui.UserView{
			Label:    access.Principal.Label,
			Operator: access.Principal.Superadmin && !access.Member,
			// Said once, in the chrome, so that every control missing
			// from the page below has an explanation the reader has
			// already met.
			ReadOnly:      !access.Can(panel.CapManageSettings) && !access.Can(panel.CapManageMembers),
			DeveloperMode: access.ShowsTechnical(),
		},
	}
	p.Nav = s.navFor(lang, access, current)
	return p
}

// navItem is one candidate link with the capability it needs.
type navItem struct {
	id    string
	url   string
	label string
	// need is the capability required to reach it. Empty means every
	// signed-in principal may.
	need panel.Capability
	// site reports whether the URL needs a site to be selected.
	site bool
}

func (s *Server) navFor(lang *ui.Language, access panel.Access, current string) []ui.NavItem {
	siteID := access.SiteID
	candidates := []navItem{
		{id: "siteler", url: "/", label: lang.T("gezinme.siteler")},
		{id: "uyeler", url: memberPath(siteID), label: lang.T("gezinme.uyeler"),
			need: panel.CapManageMembers, site: true},
		{id: "hesap", url: AccountPath, label: lang.T("gezinme.hesap")},
	}

	items := make([]ui.NavItem, 0, len(candidates))
	for _, c := range candidates {
		if c.need != "" && !access.Can(c.need) {
			continue
		}
		if c.site && siteID == "" {
			continue
		}
		items = append(items, ui.NavItem{Label: c.label, URL: c.url, Current: c.id == current})
	}
	return items
}

// viewerNotice explains an empty-looking page to somebody who cannot
// change anything on it.
//
// A viewer sees the numbers and no controls. Without a sentence saying
// why, the honest reading of that page is "this panel is broken" - and
// the support call that follows is about a feature working exactly as
// designed. NoticeLock rather than a warning: nothing has gone wrong,
// the reader is being shown where the boundary is.
func viewerNotice(lang *ui.Language, access panel.Access) []ui.Notice {
	if access.Can(panel.CapManageSettings) || access.Can(panel.CapManageMembers) {
		return nil
	}
	if access.Role != panel.RoleViewer {
		return nil
	}
	return []ui.Notice{{
		Level: ui.NoticeLock,
		Title: lang.T("viewer.baslik"),
		Body:  lang.T("viewer.govde"),
	}}
}
