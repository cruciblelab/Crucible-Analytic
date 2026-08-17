package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// Who may see a site, and at what level.
//
// The rules this page enforces were all written and tested in
// internal/panel. What it adds is the part a form can get wrong:
//
// **Nobody may grant authority above their own.** Access.CanAssign
// already says so, and the select element below only offers roles that
// pass it. That element is a courtesy. The POST handler asks the same
// question again, because a form is a text field somebody can retype -
// and an admin who could promote themselves to owner by editing one
// makes the owner/admin distinction decorative.
//
// **A site cannot be left without an owner.** The store refuses it
// inside a transaction, which is the only place it can be refused
// correctly - two admins removing the last two owners at the same
// instant is exactly the race a check-then-act version loses. This
// page's job is to turn that refusal into a sentence rather than a 500.
//
// **Adding a member does not create an account.** That is an invitation
// email, and email is C7. Until then this page adds people who already
// have accounts and says plainly what to do about anyone who does not.

// MembersPathPrefix is where a site's member list lives.
const MembersPathPrefix = "/site/"

// membersPath is the suffix after the site id.
const membersPathSuffix = "/uyeler"

// memberPath builds the member page URL for a site.
func memberPath(siteID string) string {
	if siteID == "" {
		return ""
	}
	return MembersPathPrefix + url.PathEscape(siteID) + membersPathSuffix
}

// memberRow is one person as the page shows them.
type memberRow struct {
	panel.Member
	// Self marks the signed-in user's own row, so the page can explain
	// why it offers no "remove" button there.
	Self bool
	// AssignableRoles are the roles the viewer may move this person to.
	// Computed per row rather than per page: an owner may be re-roled by
	// another owner and not by an admin, and the list has to say so.
	AssignableRoles []roleChoice
	// Removable reports whether the remove button is drawn.
	Removable bool
}

// roleChoice is one option in a role select.
type roleChoice struct {
	Value    panel.Role
	Label    string
	Selected bool
}

// membersPage is Data for the member template.
type membersPage struct {
	SiteID  string
	Members []memberRow
	// AddRoles are the roles the viewer may grant to somebody new.
	AddRoles []roleChoice
	// CanManage is false for a page rendered read-only. Today the
	// handler refuses anybody who cannot manage, so this is always true;
	// it is here because the audit view arrives in a later phase and
	// will want the same table without the controls.
	CanManage bool

	Message string
	Failed  bool
}

// membersHandler serves and processes a site's member list.
func (s *Server) membersHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	siteID := r.PathValue("site")
	if siteID == "" {
		s.Renderer.ErrorIn(w, r, http.StatusNotFound, lang)
		return
	}
	access, ok := s.siteAccess(w, r, p, siteID)
	if !ok {
		return
	}
	// A viewer gets 403 here, not a hidden link. The nav never drew this
	// page for them; typing the URL still has to be refused.
	if !s.require(w, r, access, panel.CapManageMembers) {
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.renderMembers(w, r, lang, access, membersPage{})
	case http.MethodPost:
		if !s.Sessions.CheckCSRF(r) {
			s.Renderer.ErrorIn(w, r, statusCSRFExpired, lang)
			return
		}
		s.saveMembers(w, r, lang, access)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
	}
}

func (s *Server) saveMembers(w http.ResponseWriter, r *http.Request, lang *ui.Language, access panel.Access) {
	if err := r.ParseForm(); err != nil {
		s.Renderer.ErrorIn(w, r, http.StatusBadRequest, lang)
		return
	}
	ctx := r.Context()

	var data membersPage
	switch r.PostFormValue("islem") {
	case "ekle":
		data = s.addMember(ctx, lang, access,
			r.PostFormValue("eposta"), panel.Role(r.PostFormValue("rol")))
	case "rol":
		data = s.changeRole(ctx, lang, access,
			r.PostFormValue("kullanici"), panel.Role(r.PostFormValue("rol")))
	case "cikar":
		data = s.removeMember(ctx, lang, access, r.PostFormValue("kullanici"))
	default:
		data = membersPage{Message: lang.T("uyeler.hata.bilinmeyen"), Failed: true}
	}
	s.renderMembers(w, r, lang, access, data)
}

func (s *Server) addMember(ctx context.Context, lang *ui.Language, access panel.Access,
	email string, role panel.Role) membersPage {

	// The same question the select answered when the page was drawn,
	// asked again against the value that actually arrived.
	if !access.CanAssign(role) {
		return membersPage{Message: lang.T("uyeler.hata.rol_yetki"), Failed: true}
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return membersPage{Message: lang.T("uyeler.hata.eposta_bos"), Failed: true}
	}

	user, err := s.Store.UserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, panel.ErrNotFound) {
			// Says what is missing and what to do, because the obvious
			// reading of a bare failure here is "I typed it wrong" -
			// and the actual state is "this person has no account yet",
			// which nothing on this page can fix.
			return membersPage{Message: lang.T("uyeler.hata.hesap_yok"), Failed: true}
		}
		s.logger().Error("panel: looking up member", "err", err)
		return membersPage{Message: lang.T("uyeler.hata.kaydedilemedi"), Failed: true}
	}

	by := access.Principal.UserID
	var grantedBy *int64
	if by != 0 {
		grantedBy = &by
	}
	if err := s.Store.AddMember(ctx, access.SiteID, user.ID, role, grantedBy); err != nil {
		s.logger().Error("panel: adding member", "err", err)
		return membersPage{Message: lang.T("uyeler.hata.kaydedilemedi"), Failed: true}
	}
	_ = s.Store.RecordFor(ctx, access.Principal, panel.AuditEntry{
		Action: panel.ActionMemberAdded, SiteID: access.SiteID,
		Detail: map[string]any{"user": user.Email, "role": string(role)},
	})
	return membersPage{Message: lang.Tf("uyeler.eklendi", user.Email)}
}

func (s *Server) changeRole(ctx context.Context, lang *ui.Language, access panel.Access,
	rawUserID string, role panel.Role) membersPage {

	if !access.CanAssign(role) {
		return membersPage{Message: lang.T("uyeler.hata.rol_yetki"), Failed: true}
	}
	userID, ok := parseUserID(rawUserID)
	if !ok {
		return membersPage{Message: lang.T("uyeler.hata.kullanici_gecersiz"), Failed: true}
	}
	// Demoting the person whose role currently permits the demotion is
	// how a site loses its last owner without anybody removing anyone.
	// The store's transaction refuses it; this is only the message.
	if err := s.Store.SetMemberRole(ctx, access.SiteID, userID, role); err != nil {
		return s.memberWriteFailed(lang, err, "panel: changing member role")
	}
	_ = s.Store.RecordFor(ctx, access.Principal, panel.AuditEntry{
		Action: panel.ActionMemberRerole, SiteID: access.SiteID,
		Detail: map[string]any{"user_id": userID, "role": string(role)},
	})
	return membersPage{Message: lang.T("uyeler.rol_degisti")}
}

func (s *Server) removeMember(ctx context.Context, lang *ui.Language, access panel.Access,
	rawUserID string) membersPage {

	userID, ok := parseUserID(rawUserID)
	if !ok {
		return membersPage{Message: lang.T("uyeler.hata.kullanici_gecersiz"), Failed: true}
	}
	if err := s.Store.RemoveMember(ctx, access.SiteID, userID); err != nil {
		return s.memberWriteFailed(lang, err, "panel: removing member")
	}
	_ = s.Store.RecordFor(ctx, access.Principal, panel.AuditEntry{
		Action: panel.ActionMemberRemoved, SiteID: access.SiteID,
		Detail: map[string]any{"user_id": userID},
	})
	return membersPage{Message: lang.T("uyeler.cikarildi")}
}

// memberWriteFailed turns a store error into a page message.
//
// The last-owner refusal is the whole reason this exists. It is not a
// server fault and it is not the caller's mistake in any interesting
// sense - it is the rule working - so it gets its own sentence, and only
// everything else gets logged and called a failure.
func (s *Server) memberWriteFailed(lang *ui.Language, err error, what string) membersPage {
	if errors.Is(err, panel.ErrLastOwner) {
		return membersPage{Message: lang.T("uyeler.hata.son_sahip"), Failed: true}
	}
	if errors.Is(err, panel.ErrNotFound) {
		return membersPage{Message: lang.T("uyeler.hata.uye_yok"), Failed: true}
	}
	s.logger().Error(what, "err", err)
	return membersPage{Message: lang.T("uyeler.hata.kaydedilemedi"), Failed: true}
}

// parseUserID reads a user id out of a form value.
//
// A closed conversion rather than passing the string on: this value goes
// into a query, and the type system is a better guarantee than a
// promise that the driver parameterises everything.
func parseUserID(raw string) (int64, bool) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func (s *Server) renderMembers(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	access panel.Access, data membersPage) {

	ctx := r.Context()
	members, err := s.Store.Members(ctx, access.SiteID)
	if err != nil {
		s.logger().Error("panel: listing members", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	data.SiteID = access.SiteID
	data.CanManage = access.Can(panel.CapManageMembers)
	data.AddRoles = assignableRoles(lang, access, "")
	for _, m := range members {
		self := m.UserID == access.Principal.UserID
		data.Members = append(data.Members, memberRow{
			Member:          m,
			Self:            self,
			AssignableRoles: assignableRoles(lang, access, m.Role),
			// Removing yourself is not offered. It is not forbidden by
			// the model - an owner leaving a site they no longer work on
			// is legitimate - but it is not what the button next to your
			// own name is for, and a mis-click that locks somebody out
			// of the page they are standing on is worth one extra step
			// elsewhere.
			Removable: data.CanManage && !self,
		})
	}

	page := s.page(r, lang, access, "uyeler", lang.T("uyeler.baslik"))
	page.Site = ui.SiteView{ID: access.SiteID, Name: access.SiteID}
	page.Data = data
	if data.Message != "" {
		level := ui.NoticeInfo
		if data.Failed {
			level = ui.NoticeError
		}
		page.Notices = append(page.Notices, ui.Notice{Level: level, Body: data.Message})
	}
	status := http.StatusOK
	if data.Failed {
		status = http.StatusBadRequest
	}
	s.Renderer.Render(w, r, status, "uyeler", page)
}

// assignableRoles lists the roles this viewer may set, marking current.
//
// Built from panel.ValidRoles and filtered by CanAssign, so a role added
// to the model appears here without anybody remembering to add it - and
// one nobody may grant never appears at all.
func assignableRoles(lang *ui.Language, access panel.Access, current panel.Role) []roleChoice {
	choices := make([]roleChoice, 0, len(panel.ValidRoles))
	for _, role := range panel.ValidRoles {
		if !access.CanAssign(role) {
			continue
		}
		choices = append(choices, roleChoice{
			Value:    role,
			Label:    lang.T("rol." + string(role)),
			Selected: role == current,
		})
	}
	return choices
}
