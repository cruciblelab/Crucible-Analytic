package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

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

// durationChoice is one option in the access-length select.
type durationChoice struct {
	// Days is what the form posts; 0 is "no end".
	Days  int
	Label string
}

// membersPage is Data for the member template.
type membersPage struct {
	SiteID  string
	Members []memberRow
	// Expired are memberships whose end date has passed.
	//
	// Their own list, below the table, for the same reason the
	// invitations have one: the table answers "who can see this site",
	// and somebody whose access ran out is not an answer to it. Kept
	// visible rather than swept away because a row nothing can display
	// is a row nobody can explain, and "why did Ali lose access" is a
	// question the panel should be able to answer.
	Expired []memberRow
	// AddRoles are the roles the viewer may grant to somebody new.
	AddRoles []roleChoice
	// AddDurations are the access lengths the add form offers, derived
	// from accessDurations so the template cannot offer one the handler
	// would refuse.
	AddDurations []durationChoice
	// CanManage is false for a page rendered read-only. Today the
	// handler refuses anybody who cannot manage, so this is always true;
	// it is here because the audit view arrives in a later phase and
	// will want the same table without the controls.
	CanManage bool

	// Invites are the invitations nobody has accepted yet.
	//
	// Their own list rather than rows in the table above, because the
	// table above answers "who can see this site" and an invitation is
	// not an answer to that. Somebody invited is somebody offered.
	Invites []inviteRow
	// InviteURL is the link a mint just produced, shown once.
	//
	// Once is not a limitation to work around: only the hash is stored,
	// so this is the only moment it exists. Inviting the same address
	// again replaces the invitation and produces a new link, which is
	// what "I lost it" needs and why there is no separate button for it.
	InviteURL   string
	InviteEmail string
	InviteRole  string
	Delivery    *mailDelivery

	Message string
	Failed  bool
}

// inviteRow is one open invitation as the page shows it.
type inviteRow struct {
	ID        int64
	Email     string
	Role      string
	RoleLabel string
	Expires   time.Time
	InvitedBy string
	// GrantDays is how long the access will last once accepted, zero for
	// no end. Shown as a length rather than a date because there is no
	// date until somebody clicks - the honest cost of counting from
	// acceptance instead of from minting.
	GrantDays int
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
		if !s.acceptPost(w, r, lang) {
			return
		}
		s.saveMembers(w, r, lang, access)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
	}
}

func (s *Server) saveMembers(w http.ResponseWriter, r *http.Request, lang *ui.Language, access panel.Access) {
	ctx := r.Context()

	var data membersPage
	switch r.PostFormValue("islem") {
	case "ekle":
		data = s.addMember(ctx, lang, r, access,
			r.PostFormValue("eposta"), panel.Role(r.PostFormValue("rol")),
			r.PostFormValue("sure"))
	case "rol":
		data = s.changeRole(ctx, lang, access,
			r.PostFormValue("kullanici"), panel.Role(r.PostFormValue("rol")))
	case "cikar":
		data = s.removeMember(ctx, lang, access, r.PostFormValue("kullanici"))
	case "davet-geri-al":
		data = s.withdrawInvite(ctx, lang, access, r.PostFormValue("davet"))
	default:
		data = membersPage{Message: lang.T("uyeler.hata.bilinmeyen"), Failed: true}
	}
	s.renderMembers(w, r, lang, access, data)
}

func (s *Server) addMember(ctx context.Context, lang *ui.Language, r *http.Request,
	access panel.Access, email string, role panel.Role, rawDays string) membersPage {

	// The same question the select answered when the page was drawn,
	// asked again against the value that actually arrived.
	if !access.CanAssign(role) {
		return membersPage{Message: lang.T("uyeler.hata.rol_yetki"), Failed: true}
	}
	days, ok := parseAccessDays(rawDays)
	if !ok {
		return membersPage{Message: lang.T("uyeler.hata.sure_gecersiz"), Failed: true}
	}
	if days > 0 && role == panel.RoleOwner {
		// The database refuses this too. The message exists because a
		// constraint violation reaching the page as "could not be saved"
		// would be the panel declining to say what it declined.
		return membersPage{Message: lang.T("uyeler.hata.sahip_sureli"), Failed: true}
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return membersPage{Message: lang.T("uyeler.hata.eposta_bos"), Failed: true}
	}

	user, err := s.Store.UserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, panel.ErrNotFound) {
			// No account: an invitation rather than a refusal.
			//
			// One form for both, deliberately. Whether this address has
			// an account is not something the person filling the form
			// knows, and it is not their business - they are doing one
			// thing, giving somebody access to this site. A second form
			// beside this one would ask them to guess, and tell them
			// they guessed wrong.
			return s.inviteMember(ctx, lang, r, access, email, role, days)
		}
		s.logger().Error("panel: looking up member", "err", err)
		return membersPage{Message: lang.T("uyeler.hata.kaydedilemedi"), Failed: true}
	}

	grant := panel.Grant{By: actorID(access)}
	if days > 0 {
		until := time.Now().Add(time.Duration(days) * 24 * time.Hour)
		grant.Until = &until
	}
	if err := s.Store.AddMember(ctx, access.SiteID, user.ID, role, grant); err != nil {
		return s.memberWriteFailed(lang, err, "panel: adding member")
	}
	detail := map[string]any{"user": user.Email, "role": string(role)}
	if grant.Until != nil {
		// Written when the access is given, not when it runs out. There
		// is nothing that watches for the second moment, deliberately -
		// the expiry is enforced by the reads - so the record of it has
		// to be made here or not at all.
		detail["until"] = grant.Until.UTC().Format(time.RFC3339)
	}
	s.auditFor(ctx, access.Principal, panel.AuditEntry{
		Action: panel.ActionMemberAdded, SiteID: access.SiteID, Detail: detail,
	})
	// "Can now see this site" rather than "was added": the same form also
	// updates somebody who was already a member, and telling them they
	// were added would be telling them something that did not happen.
	return membersPage{Message: lang.Tf("uyeler.erisebiliyor", user.Email)}
}

// accessDurations is the closed set of access lengths the page offers, in
// days. Zero is "no end", and it is first because it is the answer for
// almost everybody.
//
// A closed set rather than a free number field, for the reason
// parsePositiveID gives: this value reaches a query, and a list the
// template draws from is also the list the handler validates against, so
// the two cannot drift.
var accessDurations = []int{0, 1, 7, 30, 90}

// parseAccessDays turns the form's value into one of accessDurations.
//
// An empty field means "no end" so that a form posted without the select
// - an older page still open, a script - keeps the old behaviour rather
// than being refused.
func parseAccessDays(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, true
	}
	days, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	for _, d := range accessDurations {
		if d == days {
			return days, true
		}
	}
	return 0, false
}

func (s *Server) changeRole(ctx context.Context, lang *ui.Language, access panel.Access,
	rawUserID string, role panel.Role) membersPage {

	if !access.CanAssign(role) {
		return membersPage{Message: lang.T("uyeler.hata.rol_yetki"), Failed: true}
	}
	userID, ok := parsePositiveID(rawUserID)
	if !ok {
		return membersPage{Message: lang.T("uyeler.hata.kullanici_gecersiz"), Failed: true}
	}
	// Demoting the person whose role currently permits the demotion is
	// how a site loses its last owner without anybody removing anyone.
	// The store's transaction refuses it; this is only the message.
	if err := s.Store.SetMemberRole(ctx, access.SiteID, userID, role, actorID(access)); err != nil {
		return s.memberWriteFailed(lang, err, "panel: changing member role")
	}
	s.auditFor(ctx, access.Principal, panel.AuditEntry{
		Action: panel.ActionMemberRerole, SiteID: access.SiteID,
		Detail: map[string]any{"user_id": userID, "role": string(role)},
	})
	return membersPage{Message: lang.T("uyeler.rol_degisti")}
}

func (s *Server) removeMember(ctx context.Context, lang *ui.Language, access panel.Access,
	rawUserID string) membersPage {

	userID, ok := parsePositiveID(rawUserID)
	if !ok {
		return membersPage{Message: lang.T("uyeler.hata.kullanici_gecersiz"), Failed: true}
	}
	if err := s.Store.RemoveMember(ctx, access.SiteID, userID, actorID(access)); err != nil {
		return s.memberWriteFailed(lang, err, "panel: removing member")
	}
	s.auditFor(ctx, access.Principal, panel.AuditEntry{
		Action: panel.ActionMemberRemoved, SiteID: access.SiteID,
		Detail: map[string]any{"user_id": userID},
	})
	return membersPage{Message: lang.T("uyeler.cikarildi")}
}

// actorID is who the store should check this write against.
//
// A principal with no account is a developer session, which the store
// treats as the deployment acting rather than a person. That is the same
// answer its own rules would give, because such a session carries
// superadmin authority - asserted in
// TestDeveloperPrincipal_IsLabelledAndPrivileged, which is what keeps
// this shortcut honest if that ever changes.
func actorID(access panel.Access) *int64 {
	if access.Principal.UserID == 0 {
		return nil
	}
	id := access.Principal.UserID
	return &id
}

// inviteMember mints an invitation for an address with no account.
//
// Reached only from addMember, which is the point: the caller asked for
// one thing and gets one thing, whichever half of the branch runs.
func (s *Server) inviteMember(ctx context.Context, lang *ui.Language, r *http.Request,
	access panel.Access, email string, role panel.Role, days int) membersPage {

	token, invite, err := s.Store.CreateMemberInvite(ctx, access.SiteID, email, role,
		access.Principal, 0, days)
	if err != nil {
		if errors.Is(err, panel.ErrTooManyInvites) {
			return membersPage{Message: lang.Tf("uyeler.hata.davet_cok",
				panel.MaxOpenInvitesPerSite), Failed: true}
		}
		s.logger().Error("panel: creating member invitation", "err", err)
		return membersPage{Message: lang.T("uyeler.hata.kaydedilemedi"), Failed: true}
	}

	detail := map[string]any{"invited": invite.Email, "role": string(invite.Role),
		"invite_id": invite.ID}
	if invite.GrantDays > 0 {
		// Days, not a date, because there is no date yet: the clock
		// starts when somebody accepts. The audit entry says exactly
		// what was decided here and nothing it cannot know.
		detail["days"] = invite.GrantDays
	}
	s.auditFor(ctx, access.Principal, panel.AuditEntry{
		Action: panel.ActionMemberInvited, SiteID: access.SiteID, Detail: detail,
	})

	data := membersPage{
		InviteURL:   s.absoluteURL(r, JoinPathPrefix+token),
		InviteEmail: invite.Email,
		InviteRole:  lang.T("rol." + string(invite.Role)),
		Message:     lang.Tf("uyeler.davet.olusturuldu", invite.Email),
	}
	// Emailed as well, if this deployment can. The link above is set
	// first and unconditionally: mail is a second copy of something the
	// inviter is already looking at, and a send that fails changes what
	// the page says beside the link rather than whether there is one.
	delivery := s.deliverLink(ctx, lang, invite.Email,
		"posta.uye_daveti.konu", "posta.uye_daveti.govde", data.InviteURL)
	data.Delivery = &delivery
	return data
}

// withdrawInvite ends one open invitation.
func (s *Server) withdrawInvite(ctx context.Context, lang *ui.Language, access panel.Access,
	rawID string) membersPage {

	if !access.Can(panel.CapManageMembers) {
		return membersPage{Message: lang.T("uyeler.hata.rol_yetki"), Failed: true}
	}
	id, ok := parsePositiveID(rawID)
	if !ok {
		return membersPage{Message: lang.T("uyeler.hata.kullanici_gecersiz"), Failed: true}
	}
	// The site comes from the authorised access rather than from the
	// form, so an id belonging to another site's invitation matches
	// nothing here instead of being withdrawn by somebody with no
	// authority over it.
	if err := s.Store.RevokeMemberInvite(ctx, id, access.SiteID); err != nil {
		if errors.Is(err, panel.ErrNotFound) {
			return membersPage{Message: lang.T("uyeler.hata.davet_yok"), Failed: true}
		}
		s.logger().Error("panel: withdrawing invitation", "err", err)
		return membersPage{Message: lang.T("uyeler.hata.kaydedilemedi"), Failed: true}
	}
	s.auditFor(ctx, access.Principal, panel.AuditEntry{
		Action: panel.ActionMemberInviteWithdrawn, SiteID: access.SiteID,
		Detail: map[string]any{"invite_id": id},
	})
	return membersPage{Message: lang.T("uyeler.davet.geri_alindi")}
}

// memberWriteFailed turns a store error into a page message.
//
// The last-owner refusal is the whole reason this exists. It is not a
// server fault and it is not the caller's mistake in any interesting
// sense - it is the rule working - so it gets its own sentence, and only
// everything else gets logged and called a failure. The same is now true
// of the authority refusal beside it.
func (s *Server) memberWriteFailed(lang *ui.Language, err error, what string) membersPage {
	if errors.Is(err, panel.ErrLastOwner) {
		return membersPage{Message: lang.T("uyeler.hata.son_sahip"), Failed: true}
	}
	if errors.Is(err, panel.ErrNotPermitted) {
		return membersPage{Message: lang.T("uyeler.hata.uye_yetki"), Failed: true}
	}
	if errors.Is(err, panel.ErrNotFound) {
		return membersPage{Message: lang.T("uyeler.hata.uye_yok"), Failed: true}
	}
	s.logger().Error(what, "err", err)
	return membersPage{Message: lang.T("uyeler.hata.kaydedilemedi"), Failed: true}
}

// parsePositiveID reads a database row id out of a form value.
//
// A closed conversion rather than passing the string on: this value goes
// into a query, and the type system is a better guarantee than a
// promise that the driver parameterises everything.
//
// Shared by the member forms and the developer-access decisions rather
// than copied, because the property being asserted - "a form field
// becomes an integer here or it becomes nothing" - is the same one, and
// two copies is two chances for one of them to grow a special case.
func parsePositiveID(raw string) (int64, bool) {
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
	data.AddDurations = accessDurationChoices(lang)
	for _, m := range members {
		self := m.UserID == access.Principal.UserID
		row := memberRow{
			Member:          m,
			Self:            self,
			AssignableRoles: assignableRoles(lang, access, m.Role),
			// Removing yourself is not offered. It is not forbidden by
			// the model - an owner leaving a site they no longer work on
			// is legitimate - but it is not what the button next to your
			// own name is for, and a mis-click that locks somebody out
			// of the page they are standing on is worth one extra step
			// elsewhere.
			//
			// Nor is removing somebody whose role stands above your own.
			// The store refuses it either way; this is what stops the
			// page from offering a button that always fails.
			Removable: data.CanManage && !self && access.CanManageMember(m.Role),
		}
		if m.Expired {
			// No role select on a membership that grants nothing: moving
			// an expired viewer to "admin" would leave them expired, and
			// a control whose effect is invisible is worse than none.
			// Removing it is still offered, and re-granting is the add
			// form, which resets the end date.
			row.AssignableRoles = nil
			data.Expired = append(data.Expired, row)
			continue
		}
		data.Members = append(data.Members, row)
	}

	// The open invitations, read after the members so the two lists come
	// from one page load rather than from two moments.
	//
	// A failure here is logged and not fatal: it costs a section, and
	// taking the member list down over it would be the worse trade.
	if invites, err := s.Store.OpenMemberInvites(ctx, access.SiteID); err != nil {
		s.logger().Warn("panel: listing invitations", "err", err)
	} else {
		for _, in := range invites {
			data.Invites = append(data.Invites, inviteRow{
				ID:        in.ID,
				Email:     in.Email,
				Role:      string(in.Role),
				RoleLabel: lang.T("rol." + string(in.Role)),
				Expires:   in.ExpiresAt,
				InvitedBy: in.CreatedLabel,
				GrantDays: in.GrantDays,
			})
		}
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

// accessDurationChoices renders accessDurations for the select.
//
// Derived from the same slice the handler validates against, so the page
// cannot offer a length that would be refused, and adding one is a single
// edit rather than two that have to agree.
func accessDurationChoices(lang *ui.Language) []durationChoice {
	choices := make([]durationChoice, 0, len(accessDurations))
	for _, days := range accessDurations {
		label := lang.T("uyeler.sure.suresiz")
		if days > 0 {
			label = lang.Tf("uyeler.sure.gun", days)
		}
		choices = append(choices, durationChoice{Days: days, Label: label})
	}
	return choices
}

// assignableRoles lists the roles this viewer may set, marking current.
//
// Built from panel.ValidRoles and filtered by CanAssign, so a role added
// to the model appears here without anybody remembering to add it - and
// one nobody may grant never appears at all.
//
// Filtered by CanManageMember first, which is a different question: the
// list above asks what may be handed out, this asks whether this row's
// occupant may be touched at all. An administrator looking at an owner
// gets no select, which is what the page has always said happens.
func assignableRoles(lang *ui.Language, access panel.Access, current panel.Role) []roleChoice {
	if current != "" && !access.CanManageMember(current) {
		return nil
	}
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
