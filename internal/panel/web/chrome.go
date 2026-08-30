package web

import (
	"context"
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
	// Asked once and used twice: the navigation draws the approval page
	// only for somebody who may open it, and the banner is only for the
	// same person. Two calls would be two identical membership queries on
	// every page in the panel.
	decides := s.decidesDevAccess(r.Context(), access.Principal)
	p.Nav = s.navFor(lang, access, current, decides)
	if decides {
		if n := s.pendingDevAccess(r); n > 0 {
			p.Notices = append(p.Notices, devAccessNotice(lang, p.F, n))
		}
	}
	return p
}

// decidesDevAccess reports whether this reader is one of the people who
// decides developer access.
//
// Cosmetic, like everything else in this file: it decides what to draw.
// The authority is requireDecider, which asks the same two questions
// again on the page itself.
//
// Kind before ownership, here as there. A developer principal carries
// Superadmin - so ownsAnySite says yes - and drawing them a link to a
// page that will refuse them is the smaller of the two problems that
// ordering causes. The larger one is in requireDecider.
func (s *Server) decidesDevAccess(ctx context.Context, p panel.Principal) bool {
	if s.Store == nil || p.Kind != panel.PrincipalUser {
		return false
	}
	return s.ownsAnySite(ctx, p)
}

// pendingDevAccess counts requests awaiting a decision, for the banner.
//
// Called only for somebody who has already been found entitled to
// decide, so this is the second query rather than the first. Counting
// rather than listing: the banner needs a number, and the page that
// needs the rows is one click away.
//
// A failure here is logged and drawn as nothing. A banner is the panel
// volunteering information, and a deployment whose pages stop rendering
// because a count failed has traded something that matters for something
// that does not.
func (s *Server) pendingDevAccess(r *http.Request) int {
	n, err := s.Store.CountPendingDevAccess(r.Context())
	if err != nil {
		s.logger().Error("panel: counting pending developer access", "err", err)
		return 0
	}
	return n
}

// devAccessNotice is the banner an owner meets on every page while
// somebody is waiting.
//
// NoticeWarn rather than NoticeInfo: nothing has gone wrong, but
// somebody is asking to be let into this customer's data and the answer
// is theirs to give. It carries its own link, because a banner that
// announces a decision without offering the page to make it on is a
// notification, and this is a request.
func devAccessNotice(lang *ui.Language, f *ui.Formatter, n int) ui.Notice {
	return ui.Notice{
		Level:       ui.NoticeWarn,
		Title:       lang.T("erisim.afis.baslik"),
		Body:        lang.Tn("erisim.afis.govde", n, f.Number(int64(n))),
		ActionURL:   DevAccessRequestsPath,
		ActionLabel: lang.T("erisim.afis.eylem"),
	}
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
	// health limits the link to somebody entitled to read the health
	// page: an owner, or a developer. Broader than `decides` by exactly
	// one principal kind, which is why it is its own field rather than a
	// reuse.
	health bool
	// decides limits the link to somebody entitled to decide developer
	// access. Not a Capability, because a capability is held against a
	// site and this is a decision about the machine underneath all of
	// them: the people who make it are the ones who own something on it.
	// The answer arrives already resolved - see page.
	decides bool
}

func (s *Server) navFor(lang *ui.Language, access panel.Access, current string, decides bool) []ui.NavItem {
	siteID := access.SiteID
	candidates := []navItem{
		{id: "siteler", url: "/", label: lang.T("gezinme.siteler")},
		{id: "uyeler", url: memberPath(siteID), label: lang.T("gezinme.uyeler"),
			need: panel.CapManageMembers, site: true},
		// No capability, deliberately. A viewer reaches the settings
		// page and sees every value with a sentence saying why there is
		// no control - see settingsHandler. Hiding the link would mean
		// a customer cannot find out what their own deployment is set
		// to without asking somebody.
		{id: "ayarlar", url: settingsPath(siteID), label: lang.T("gezinme.ayarlar"),
			site: true},
		{id: "erisim", url: DevAccessRequestsPath, label: lang.T("gezinme.erisim"),
			decides: true},
		// Same gate as the access page and for a related reason: the
		// mail account belongs to the machine rather than to a site, and
		// the people who decide about the machine are the ones who own
		// something on it. `decides` already carries "a signed-in person
		// who owns a site", which is exactly the audience here.
		{id: "posta", url: MailPath, label: lang.T("gezinme.posta"),
			decides: true},
		// The health page is the one entry a developer sees too, so it
		// cannot ride on `decides` - that resolves to "a signed-in
		// person who owns a site", which a developer principal is not.
		// The link is drawn for both and requireHealthReader is what
		// actually decides; a link somebody is refused would be the
		// smaller mistake, and here nobody is.
		{id: "saglik", url: HealthPath, label: lang.T("gezinme.saglik"),
			health: true},
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
		if c.decides && !decides {
			continue
		}
		if c.health && !decides && access.Principal.Kind != panel.PrincipalDeveloper {
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
