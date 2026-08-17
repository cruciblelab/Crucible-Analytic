package web

import (
	"bytes"
	"context"
	"errors"
	"image/png"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The account page: display name, password, developer mode, second
// factor.
//
// Two rules, and both are about the same thing - that a session is
// weaker evidence than a password.
//
// **Changing the password requires the current one.** A session cookie
// can be copied off a shared machine, lifted from a laptop somebody left
// open, or inherited from a browser nobody signed out of. Any of those
// gets an attacker the customer's numbers, which is bad; without this
// field, it also gets them the account permanently, which is worse.
// Removing the second factor asks for it too, for the same reason.
//
// **A half-finished second factor changes nothing.** The secret lives in
// the session until a code proves the authenticator actually has it.
// Writing it on the way out would produce the one unrecoverable state
// this panel can create by itself: an account demanding codes from an
// app that never finished scanning.

// AccountPath is the account settings page.
const AccountPath = "/hesap"

// TOTPQRPath serves the enrolment QR for the secret in this session.
const TOTPQRPath = "/hesap/iki-faktor/qr"

// accountPage is Data for the account template.
type accountPage struct {
	Email       string
	DisplayName string
	Superadmin  bool

	// CanUseDeveloperMode gates the toggle. The preference is stored on
	// the user, but only a role allowed to use it may set it - and the
	// POST handler checks again.
	CanUseDeveloperMode bool
	DeveloperMode       bool

	// TOTPEnabled is the committed state.
	TOTPEnabled bool
	// Enrolling reports that a secret is waiting in the session for a
	// code. While true the page shows the QR and the code field instead
	// of the "turn it on" button.
	Enrolling bool
	// ManualKey is the secret in the form an authenticator accepts typed
	// in, for anybody who cannot scan. Shown only while enrolling, and
	// only for the secret this session is holding.
	ManualKey string
	// QRURL is where the page fetches the enrolment image.
	QRURL string

	Message string
	Failed  bool
}

// accountHandler serves and processes the account page.
func (s *Server) accountHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	// The developer principal has no account row to edit: it is a visit,
	// not a person. Sending them here would render a form whose every
	// save fails.
	if p.Kind != panel.PrincipalUser {
		s.Renderer.ErrorIn(w, r, http.StatusForbidden, lang)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.renderAccount(w, r, lang, p, accountPage{})
	case http.MethodPost:
		if !s.Sessions.CheckCSRF(r) {
			s.Renderer.ErrorIn(w, r, statusCSRFExpired, lang)
			return
		}
		s.saveAccount(w, r, lang, p)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
	}
}

func (s *Server) saveAccount(w http.ResponseWriter, r *http.Request, lang *ui.Language, p panel.Principal) {
	if err := r.ParseForm(); err != nil {
		s.Renderer.ErrorIn(w, r, http.StatusBadRequest, lang)
		return
	}
	ctx := r.Context()
	user, err := s.Store.UserByID(ctx, p.UserID)
	if err != nil {
		s.logger().Error("panel: loading account", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	var data accountPage
	// A closed set of actions, matched exactly. The form's action field
	// reaches this from the browser like everything else, and a switch
	// with a default that does nothing is the difference between an
	// unknown value being ignored and it falling into whichever branch
	// happens to be first.
	switch r.PostFormValue("islem") {
	case "ad":
		data = s.saveDisplayName(ctx, lang, user, r.PostFormValue("ad"))
	case "parola":
		data = s.savePassword(ctx, lang, user, p,
			r.PostFormValue("mevcut_parola"),
			r.PostFormValue("yeni_parola"),
			r.PostFormValue("yeni_parola_tekrar"))
	case "gelistirici":
		data = s.saveDeveloperMode(ctx, lang, user, p, r.PostFormValue("acik") == "1")
	case "2fa-basla":
		data = s.beginTOTP(ctx, lang, user)
	case "2fa-onayla":
		data = s.confirmTOTP(ctx, lang, user, p, r.PostFormValue("kod"))
	case "2fa-iptal":
		s.Sessions.ClearPendingTOTP(ctx)
		data = accountPage{Message: lang.T("hesap.2fa.vazgecildi")}
	case "2fa-kapat":
		data = s.disableTOTP(ctx, lang, user, p, r.PostFormValue("mevcut_parola"))
	default:
		data = accountPage{Message: lang.T("hesap.hata.bilinmeyen"), Failed: true}
	}

	s.renderAccount(w, r, lang, p, data)
}

func (s *Server) saveDisplayName(ctx context.Context, lang *ui.Language, user panel.User, name string) accountPage {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > panel.MaxDisplayNameLength {
		return accountPage{Message: lang.T("hesap.hata.ad_uzun"), Failed: true}
	}
	if err := s.Store.SetDisplayName(ctx, user.ID, name); err != nil {
		s.logger().Error("panel: setting display name", "err", err)
		return accountPage{Message: lang.T("hesap.hata.kaydedilemedi"), Failed: true}
	}
	return accountPage{Message: lang.T("hesap.ad_kaydedildi")}
}

func (s *Server) savePassword(ctx context.Context, lang *ui.Language, user panel.User, p panel.Principal,
	current, next, repeat string) accountPage {

	// The current password, first and always. Everything else on this
	// page is a preference; this one is the account.
	if ok, _ := panel.VerifyPassword(user.PasswordHash, current); !ok {
		_ = s.Store.RecordFor(ctx, p, panel.AuditEntry{
			Action: panel.ActionLoginFailed,
			Detail: map[string]any{"stage": "password_change"},
		})
		return accountPage{Message: lang.T("hesap.hata.mevcut_parola"), Failed: true}
	}
	if next != repeat {
		return accountPage{Message: lang.T("hesap.hata.parolalar_farkli"), Failed: true}
	}
	if err := panel.ValidatePassword(next); err != nil {
		return accountPage{Message: passwordProblem(lang, err), Failed: true}
	}
	hash, err := panel.HashPassword(next)
	if err != nil {
		s.logger().Error("panel: hashing password", "err", err)
		return accountPage{Message: lang.T("hesap.hata.kaydedilemedi"), Failed: true}
	}
	if err := s.Store.SetPasswordHash(ctx, user.ID, hash); err != nil {
		s.logger().Error("panel: setting password", "err", err)
		return accountPage{Message: lang.T("hesap.hata.kaydedilemedi"), Failed: true}
	}
	_ = s.Store.RecordFor(ctx, p, panel.AuditEntry{Action: panel.ActionPasswordChanged})

	// Deliberately says what this does *not* do. Other sessions on other
	// machines keep working, because scs stores sessions without a user
	// column and finding them is a table scan today. Saying so beats
	// letting somebody who just changed a password because they felt
	// watched believe they have ended the watching.
	return accountPage{Message: lang.T("hesap.parola.kaydedildi")}
}

func (s *Server) saveDeveloperMode(ctx context.Context, lang *ui.Language, user panel.User,
	p panel.Principal, on bool) accountPage {

	// Checked here and not only when rendering the toggle. A viewer who
	// posts this form by hand must not acquire the technical views by
	// setting a preference the page never offered them.
	if !s.mayUseDeveloperMode(ctx, p) {
		return accountPage{Message: lang.T("hesap.hata.gelistirici_yetki"), Failed: true}
	}
	if err := s.Store.SetDeveloperMode(ctx, user.ID, on); err != nil {
		s.logger().Error("panel: setting developer mode", "err", err)
		return accountPage{Message: lang.T("hesap.hata.kaydedilemedi"), Failed: true}
	}
	action := panel.ActionDeveloperModeOff
	if on {
		action = panel.ActionDeveloperModeOn
	}
	_ = s.Store.RecordFor(ctx, p, panel.AuditEntry{Action: action})
	if on {
		return accountPage{Message: lang.T("hesap.gelistirici.acildi")}
	}
	return accountPage{Message: lang.T("hesap.gelistirici.kapandi")}
}

// mayUseDeveloperMode reports whether this principal holds the developer
// capability on any site they can reach.
//
// Any site rather than a particular one, because the preference itself
// is per-account: it decides whether the technical layers render at all.
// Whether they render for a *given* site is still Access.ShowsTechnical
// on that site, which asks the role again.
func (s *Server) mayUseDeveloperMode(ctx context.Context, p panel.Principal) bool {
	if p.Superadmin {
		return true
	}
	sites, err := s.Store.Sites(ctx, p, nil)
	if err != nil {
		s.logger().Error("panel: listing sites for developer mode", "err", err)
		return false
	}
	for _, site := range sites {
		if panel.RoleCan(site.Role, panel.CapUseDeveloperMode) {
			return true
		}
	}
	return false
}

// --- second factor ---

func (s *Server) beginTOTP(ctx context.Context, lang *ui.Language, user panel.User) accountPage {
	if user.HasTOTP() {
		return accountPage{Message: lang.T("hesap.hata.2fa_zaten"), Failed: true}
	}
	key, err := panel.NewTOTPSecret(user.Email)
	if err != nil {
		s.logger().Error("panel: generating totp secret", "err", err)
		return accountPage{Message: lang.T("hesap.hata.kaydedilemedi"), Failed: true}
	}
	// Into the session, not the user row. Until a code comes back, this
	// account has no second factor and can still be signed in to.
	s.Sessions.PutPendingTOTP(ctx, key.Secret())
	return accountPage{Message: lang.T("hesap.2fa.baslatildi")}
}

func (s *Server) confirmTOTP(ctx context.Context, lang *ui.Language, user panel.User,
	p panel.Principal, code string) accountPage {

	secret := s.Sessions.PendingTOTP(ctx)
	if secret == "" {
		return accountPage{Message: lang.T("hesap.hata.2fa_baslatilmadi"), Failed: true}
	}
	// Verified against the session's secret, before it is stored - so a
	// wrong code leaves the account exactly as it was.
	if err := s.Store.VerifyTOTP(ctx, user.ID, secret, strings.TrimSpace(code), time.Now()); err != nil {
		switch {
		case errors.Is(err, panel.ErrTOTPReplayed):
			return accountPage{Message: lang.T("giris.hata.kod_kullanildi"), Failed: true}
		case errors.Is(err, panel.ErrTOTPInvalid):
			return accountPage{Message: lang.T("giris.hata.kod_gecersiz"), Failed: true}
		default:
			s.logger().Error("panel: verifying totp during enrolment", "err", err)
			return accountPage{Message: lang.T("hesap.hata.kaydedilemedi"), Failed: true}
		}
	}
	if err := s.Store.SetTOTPSecret(ctx, user.ID, secret); err != nil {
		s.logger().Error("panel: storing totp secret", "err", err)
		return accountPage{Message: lang.T("hesap.hata.kaydedilemedi"), Failed: true}
	}
	s.Sessions.ClearPendingTOTP(ctx)
	_ = s.Store.RecordFor(ctx, p, panel.AuditEntry{Action: panel.ActionTOTPEnabled})
	return accountPage{Message: lang.T("hesap.2fa.acildi")}
}

func (s *Server) disableTOTP(ctx context.Context, lang *ui.Language, user panel.User,
	p panel.Principal, current string) accountPage {

	if !user.HasTOTP() {
		return accountPage{Message: lang.T("hesap.hata.2fa_yok"), Failed: true}
	}
	// Turning a second factor off is exactly the move somebody with a
	// stolen session wants to make first, and it is the only account
	// change that lowers a defence. It costs the password.
	if ok, _ := panel.VerifyPassword(user.PasswordHash, current); !ok {
		_ = s.Store.RecordFor(ctx, p, panel.AuditEntry{
			Action: panel.ActionLoginFailed,
			Detail: map[string]any{"stage": "totp_disable"},
		})
		return accountPage{Message: lang.T("hesap.hata.mevcut_parola"), Failed: true}
	}
	if err := s.Store.SetTOTPSecret(ctx, user.ID, ""); err != nil {
		s.logger().Error("panel: clearing totp secret", "err", err)
		return accountPage{Message: lang.T("hesap.hata.kaydedilemedi"), Failed: true}
	}
	_ = s.Store.RecordFor(ctx, p, panel.AuditEntry{Action: panel.ActionTOTPDisabled})
	return accountPage{Message: lang.T("hesap.2fa.kapandi")}
}

// totpQRHandler renders the enrolment QR for the secret in this session.
//
// A separate endpoint rather than a data: URI in the page, which is the
// obvious way to do this and the wrong one. An embedded secret is in the
// HTML: in view-source, in the browser's memory cache, in whatever a
// screenshot or a "save page as" produces, and in any copy of the
// markup a support conversation drags along. Here it is a same-origin
// image, scoped to the session that asked for it, and never stored.
//
// The response is no-store for the same reason.
func (s *Server) totpQRHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
		return
	}
	if p.Kind != panel.PrincipalUser {
		s.Renderer.ErrorIn(w, r, http.StatusForbidden, lang)
		return
	}
	secret := s.Sessions.PendingTOTP(r.Context())
	if secret == "" {
		// Nothing is being enrolled, so there is nothing to draw. 404
		// rather than an empty image: an image that renders blank looks
		// like a broken page.
		s.Renderer.ErrorIn(w, r, http.StatusNotFound, lang)
		return
	}
	user, err := s.Store.UserByID(r.Context(), p.UserID)
	if err != nil {
		s.logger().Error("panel: loading account for qr", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	key, err := panel.TOTPKeyFor(user.Email, secret)
	if err != nil {
		s.logger().Error("panel: rebuilding totp key", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}
	img, err := key.Image(qrSize, qrSize)
	if err != nil {
		s.logger().Error("panel: rendering totp qr", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	// Encoded into a buffer first, like every other response this panel
	// writes: a half-written image after a 200 has already gone out is
	// a broken picture with no explanation.
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		s.logger().Error("panel: encoding totp qr", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/png")
	h.Set("Cache-Control", "no-store, max-age=0")
	h.Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf.Bytes())
	}
}

// qrSize is the QR's pixel size. Large enough to scan from a laptop
// screen with a phone held at arm's length.
const qrSize = 256

func (s *Server) renderAccount(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	p panel.Principal, data accountPage) {

	ctx := r.Context()
	user, err := s.Store.UserByID(ctx, p.UserID)
	if err != nil {
		s.logger().Error("panel: loading account", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	pending := s.Sessions.PendingTOTP(ctx)
	data.Email = user.Email
	data.DisplayName = user.DisplayName
	data.Superadmin = user.IsSuperadmin
	data.CanUseDeveloperMode = s.mayUseDeveloperMode(ctx, p)
	data.DeveloperMode = user.DeveloperMode
	data.TOTPEnabled = user.HasTOTP()
	data.Enrolling = pending != "" && !user.HasTOTP()
	if data.Enrolling {
		data.ManualKey = pending
		data.QRURL = TOTPQRPath
	}

	// Built from the principal alone: this page belongs to an account,
	// not to a site, so there is no per-site Access to hand the chrome.
	page := s.page(r, lang, panel.Access{Principal: p}, "hesap", lang.T("hesap.baslik"))
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
	s.Renderer.Render(w, r, status, "hesap", page)
}

// passwordProblem turns a validation error into a sentence.
//
// Mapped rather than printed: ValidatePassword's errors are written for
// a developer reading a log, and the rules they describe are the ones a
// person needs stated in their own language.
func passwordProblem(lang *ui.Language, err error) string {
	switch {
	case errors.Is(err, panel.ErrPasswordTooShort):
		return lang.T("hesap.hata.parola_kisa")
	case errors.Is(err, panel.ErrPasswordTooLong):
		return lang.T("hesap.hata.parola_uzun")
	default:
		return lang.T("hesap.hata.parola_gecersiz")
	}
}
