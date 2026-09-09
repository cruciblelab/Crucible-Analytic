package web

import (
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// Accepting an invitation to a site.
//
// The counterpart of ClaimPathPrefix, and deliberately a different page
// rather than a mode of that one. An owner claim hands over the whole
// deployment; this hands over one site at one role, and the two say
// different things to the person reading them. A single template with an
// if in it would be a page that has to explain both.
//
// # What this page does not offer
//
// A role, a site, or an address. All three are on the invitation row and
// are read from it inside the transaction that grants the membership -
// see panel.RedeemMemberInvite. Nothing this form sends is consulted for
// any of them, which is why a hand-edited form is not a way in.

// JoinPathPrefix is where an invitation to a site is opened.
const JoinPathPrefix = "/katil/"

// joinPage is Data for the join template.
type joinPage struct {
	Invite panel.MemberInvite
	// RoleLabel is the invitation's role in the reader's language. On
	// the page because "viewer" is a word the invitee has to understand
	// before they accept, not after.
	RoleLabel string
	Token     string
	// Invalid renders the "this link cannot be used" page instead of the
	// form.
	Invalid bool
	Error   string
}

// joinHandler serves and processes an invitation to a site.
func (s *Server) joinHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)
	// Open to anybody holding a link, so it is reached before any of the
	// checks the authenticated pages rely on. See haveStore.
	if !s.haveStore(w, r, lang) {
		return
	}
	token := strings.Trim(strings.TrimPrefix(r.URL.Path, JoinPathPrefix), "/")

	invite, err := s.Store.LookupMemberInvite(r.Context(), token)
	if err != nil {
		if errors.Is(err, panel.ErrInviteInvalid) {
			// One page for unknown, expired, withdrawn and already used.
			// Telling them apart would confirm to anybody guessing that
			// a guess had once been real, and a person holding a genuine
			// link that no longer works does the same thing in every
			// case: ask whoever invited them.
			s.renderJoin(w, r, lang, http.StatusNotFound, joinPage{Invalid: true})
			return
		}
		s.logger().Error("panel: looking up member invitation", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	page := joinPage{Invite: invite, RoleLabel: lang.T("rol." + string(invite.Role)), Token: token}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.renderJoin(w, r, lang, http.StatusOK, page)
	case http.MethodPost:
		if !s.acceptPost(w, r, lang) {
			return
		}
		s.submitJoin(w, r, lang, page)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
	}
}

func (s *Server) submitJoin(w http.ResponseWriter, r *http.Request, lang *ui.Language, page joinPage) {
	ctx := r.Context()
	password := r.PostFormValue("parola")
	repeat := r.PostFormValue("parola_tekrar")
	name := strings.TrimSpace(r.PostFormValue("ad"))

	refuse := func(msg string) {
		page.Error = msg
		s.renderJoin(w, r, lang, http.StatusBadRequest, page)
	}
	if password != repeat {
		refuse(lang.T("hesap.hata.parolalar_farkli"))
		return
	}
	if err := panel.ValidatePassword(password); err != nil {
		refuse(passwordProblem(lang, err))
		return
	}
	if utf8.RuneCountInString(name) > panel.MaxDisplayNameLength {
		refuse(lang.T("hesap.hata.ad_uzun"))
		return
	}
	hash, err := panel.HashPassword(password)
	if err != nil {
		s.logger().Error("panel: hashing password", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	got, err := s.Store.RedeemMemberInvite(ctx, page.Token, name, hash, peerAddr(r))
	if err != nil {
		if errors.Is(err, panel.ErrInviteInvalid) {
			// It was open when the page was drawn and is not now: it
			// expired while the form sat there, another tab took it, or
			// whoever invited them lost the authority to have done so.
			refuse(lang.T("katil.hata.artik_gecersiz"))
			return
		}
		s.logger().Error("panel: redeeming member invitation", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	id := got.User.ID
	if got.Created {
		s.audit(ctx, panel.AuditEntry{
			Action: panel.ActionUserCreated, ActorKind: panel.PrincipalUser,
			ActorID: &id, ActorLabel: got.User.Email,
			Detail: map[string]any{"via": "member_invite", "site": got.Invite.SiteID},
		})
	}
	s.audit(ctx, panel.AuditEntry{
		Action: panel.ActionMemberInviteUsed, ActorKind: panel.PrincipalUser,
		ActorID: &id, ActorLabel: got.User.Email, SiteID: got.Invite.SiteID,
		Detail: map[string]any{"role": string(got.Invite.Role), "invite_id": got.Invite.ID},
	})

	// Signed in immediately when the account is new, for the reason the
	// owner claim gives: asking somebody to type a password they set four
	// seconds ago reads as the panel not having believed them.
	//
	// Not when it is not new. The password on the form was never applied
	// to an account that already had one, so signing them in on the
	// strength of it would be signing somebody in without checking a
	// password - and this page is reachable by anybody holding a link.
	if !got.Created {
		page.Error = ""
		s.renderJoinDone(w, r, lang, got)
		return
	}
	if err := s.Sessions.LogIn(ctx, got.User); err != nil {
		s.logger().Error("panel: signing in new member", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}
	http.Redirect(w, r, sitePath(got.Invite.SiteID), http.StatusSeeOther)
}

// renderJoinDone is the page an invitee sees when the address already had
// an account: the site is theirs now, and they sign in as they always do.
func (s *Server) renderJoinDone(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	got panel.Redemption) {

	page := &ui.Page{
		L:       lang,
		Title:   lang.T("katil.baslik"),
		Heading: lang.T("katil.baslik"),
		F:       ui.NewFormatter(lang, s.zone(r.Context())),
		Data: joinPage{
			Invite:    got.Invite,
			RoleLabel: lang.T("rol." + string(got.Invite.Role)),
		},
	}
	page.Notices = append(page.Notices, ui.Notice{
		Level: ui.NoticeInfo,
		Body:  lang.Tf("katil.hesap_vardi", got.Invite.SiteID),
	})
	s.Renderer.Render(w, r, http.StatusOK, "katil", page)
}

func (s *Server) renderJoin(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	status int, data joinPage) {

	title := lang.T("katil.baslik")
	if data.Invalid {
		title = lang.T("katil.gecersiz.baslik")
	}
	page := &ui.Page{
		L:       lang,
		Title:   title,
		Heading: title,
		F:       ui.NewFormatter(lang, s.zone(r.Context())),
		CSRF:    s.Sessions.CSRFToken(r.Context()),
		Data:    data,
	}
	if data.Error != "" {
		page.Notices = append(page.Notices, ui.Notice{Level: ui.NoticeError, Body: data.Error})
	}
	s.Renderer.Render(w, r, status, "katil", page)
}
