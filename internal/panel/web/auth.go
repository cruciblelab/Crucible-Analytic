package web

import (
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The customer's door.
//
// Everything the security of this page rests on was written earlier and
// tested on its own: argon2id, the two throttling counters, TOTP with
// replay refusal, session-token renewal, the audit log. What is here is
// the part that decides whether any of it is actually reached, which is
// the part that historically goes wrong - a throttle that is checked
// after the password, a second factor that can be skipped by going
// straight to the next URL, a login form that answers differently for
// an account that exists.
//
// So this file is written around three rules:
//
//  1. **The throttle is checked before anything expensive or
//     revealing.** Not after the password fails - before it is verified
//     at all.
//  2. **Every failure produces the same page.** One sentence, one
//     status, the same timing. The password check runs even for an
//     address with no account, because skipping it answers "does this
//     address have an account here" in about eighty milliseconds.
//  3. **The pending second-factor state is not a session.** Principal
//     returns ErrNoSession for it, so every authenticated page already
//     refuses somebody who stopped halfway - without any handler
//     needing to remember.

// LoginPath is the sign-in form.
const LoginPath = "/giris"

// SecondFactorPath is where a code is asked for.
const SecondFactorPath = "/giris/dogrulama"

// LogoutPath ends a session.
const LogoutPath = "/cikis"

// loginPage is Data for both sign-in templates.
type loginPage struct {
	// Email is echoed back so a mistyped password does not cost the
	// address as well. Never echoed on the second-factor page - by then
	// the address is a thing the browser should stop carrying.
	Email string
	// Next is where to go after signing in, already validated. Empty
	// means the site list.
	Next string
	// Error is the one sentence shown on any failure.
	Error string
	// RememberedName greets the account awaiting a code. It is the
	// display name rather than the address: the person reading it just
	// proved they know the password, and the page is more useful if it
	// says which account they are finishing.
	RememberedName string
}

// loginHandler serves the sign-in form and processes it.
func (s *Server) loginHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)

	// Already signed in: send them on rather than showing a form that
	// would replace a working session with the same one.
	if p, err := s.Sessions.Principal(r.Context()); err == nil && p.Kind != "" {
		http.Redirect(w, r, s.safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.renderLogin(w, r, lang, http.StatusOK, loginPage{
			Next: s.rawNext(r.URL.Query().Get("next")),
		})
	case http.MethodPost:
		if !s.Sessions.CheckCSRF(r) {
			s.Renderer.ErrorIn(w, r, statusCSRFExpired, lang)
			return
		}
		s.submitLogin(w, r, lang)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
	}
}

func (s *Server) submitLogin(w http.ResponseWriter, r *http.Request, lang *ui.Language) {
	if err := r.ParseForm(); err != nil {
		s.Renderer.ErrorIn(w, r, http.StatusBadRequest, lang)
		return
	}
	ctx := r.Context()
	email := strings.TrimSpace(r.PostFormValue("eposta"))
	password := r.PostFormValue("parola")
	next := s.rawNext(r.PostFormValue("next"))
	addr := peerAddr(r)

	refuse := func() {
		s.renderLogin(w, r, lang, http.StatusUnauthorized, loginPage{
			Email: email, Next: next, Error: lang.T("giris.hata.gecersiz"),
		})
	}

	// Before the password, not after. A throttle consulted only once a
	// password has been checked is a throttle that still runs one
	// argon2id verification per guess - which is the cost the attacker
	// was going to pay anyway, and the answer they wanted.
	throttle, err := s.Store.CheckLoginThrottle(ctx, email, addr)
	if err != nil {
		s.logger().Error("panel: login throttle", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}
	if throttle.Blocked {
		_ = s.Store.Record(ctx, panel.AuditEntry{
			Action: panel.ActionLoginThrottled, ActorLabel: email,
			Detail: map[string]any{"reason": throttle.Reason},
		})
		// A separate sentence from "wrong password", because this one is
		// actionable: waiting fixes it. It names no account and no
		// address - which of the two counters fired would tell an
		// attacker whether the address is registered.
		s.renderLogin(w, r, lang, http.StatusTooManyRequests, loginPage{
			Email: email, Next: next,
			Error: s.throttleMessage(lang, throttle),
		})
		return
	}

	user, err := s.Store.UserByEmail(ctx, email)
	if err != nil {
		if !errors.Is(err, panel.ErrNotFound) {
			s.logger().Error("panel: login lookup", "err", err)
			s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
			return
		}
		// No such account. Hash anyway, against the same parameters, so
		// this branch costs what the real one costs. Without it the
		// response time is a membership oracle for the whole customer
		// list, readable from anywhere on the internet.
		panel.VerifyDummy(password)
		s.recordFailedLogin(r, email, addr)
		refuse()
		return
	}

	ok, needsRehash := panel.VerifyPassword(user.PasswordHash, password)
	if !ok {
		s.recordFailedLogin(r, email, addr)
		refuse()
		return
	}
	// A disabled account is checked after the password on purpose:
	// refusing earlier would make "disabled" distinguishable from "wrong
	// password" without knowing the password, which turns the login form
	// into a way to enumerate suspended accounts.
	if user.Disabled {
		s.recordFailedLogin(r, email, addr)
		refuse()
		return
	}

	// The password was right, so the failure counters for this account
	// have served their purpose.
	if err := s.Store.ClearLoginFailures(ctx, email); err != nil {
		s.logger().Warn("panel: clearing login failures", "err", err)
	}
	if err := s.Store.RecordLoginAttempt(ctx, email, addr, true); err != nil {
		s.logger().Warn("panel: recording login attempt", "err", err)
	}
	// Rehash when the stored parameters are behind the current ones.
	// Here rather than in a migration: this is the one moment the plain
	// password exists in memory, and it costs nothing extra.
	if needsRehash {
		if hash, err := panel.HashPassword(password); err == nil {
			if err := s.Store.SetPasswordHash(ctx, user.ID, hash); err != nil {
				s.logger().Warn("panel: rehashing password", "err", err)
			}
		}
	}

	if user.HasTOTP() {
		if err := s.Sessions.AwaitSecondFactor(ctx, user); err != nil {
			s.logger().Error("panel: awaiting second factor", "err", err)
			s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
			return
		}
		http.Redirect(w, r, withNext(SecondFactorPath, next), http.StatusSeeOther)
		return
	}

	s.completeLogin(w, r, lang, user, next)
}

// completeLogin establishes the session and records it.
func (s *Server) completeLogin(w http.ResponseWriter, r *http.Request, lang *ui.Language, user panel.User, next string) {
	ctx := r.Context()
	if err := s.Sessions.LogIn(ctx, user); err != nil {
		s.logger().Error("panel: establishing session", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}
	if err := s.Store.TouchLastLogin(ctx, user.ID); err != nil {
		s.logger().Warn("panel: touching last login", "err", err)
	}
	id := user.ID
	_ = s.Store.Record(ctx, panel.AuditEntry{
		Action: panel.ActionLoginSucceeded, ActorKind: panel.PrincipalUser,
		ActorID: &id, ActorLabel: user.Email,
		Detail: map[string]any{"two_factor": user.HasTOTP()},
	})
	http.Redirect(w, r, s.safeNext(next), http.StatusSeeOther)
}

func (s *Server) recordFailedLogin(r *http.Request, email string, addr netip.Addr) {
	ctx := r.Context()
	if err := s.Store.RecordLoginAttempt(ctx, email, addr, false); err != nil {
		s.logger().Warn("panel: recording login attempt", "err", err)
	}
	_ = s.Store.Record(ctx, panel.AuditEntry{
		Action: panel.ActionLoginFailed, ActorLabel: email,
	})
}

// secondFactorHandler asks for the code.
func (s *Server) secondFactorHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)
	ctx := r.Context()

	pending := s.Sessions.PendingUserID(ctx)
	if pending == 0 {
		// No half-finished login here. A redirect rather than an error:
		// arriving with an expired session is ordinary, and the form
		// they need is one hop away.
		http.Redirect(w, r, LoginPath, http.StatusSeeOther)
		return
	}
	user, err := s.Store.UserByID(ctx, pending)
	if err != nil || user.Disabled || !user.HasTOTP() {
		// The account was deleted, suspended or had its second factor
		// removed between the two steps. Nothing to finish.
		_ = s.Sessions.LogOut(ctx)
		http.Redirect(w, r, LoginPath, http.StatusSeeOther)
		return
	}

	next := s.rawNext(r.URL.Query().Get("next"))
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.renderSecondFactor(w, r, lang, http.StatusOK, loginPage{
			Next: next, RememberedName: user.Name(),
		})
	case http.MethodPost:
		if !s.Sessions.CheckCSRF(r) {
			s.Renderer.ErrorIn(w, r, statusCSRFExpired, lang)
			return
		}
		s.submitSecondFactor(w, r, lang, user)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
	}
}

func (s *Server) submitSecondFactor(w http.ResponseWriter, r *http.Request, lang *ui.Language, user panel.User) {
	if err := r.ParseForm(); err != nil {
		s.Renderer.ErrorIn(w, r, http.StatusBadRequest, lang)
		return
	}
	ctx := r.Context()
	next := s.rawNext(r.PostFormValue("next"))
	addr := peerAddr(r)

	// The same throttle as the password step, on the same account.
	//
	// Without it this page is a six-digit search against an account
	// whose password the attacker already has - a million codes, no
	// argon2id in the way, and three thirty-second windows valid at
	// once. The second factor would be decoration.
	throttle, err := s.Store.CheckLoginThrottle(ctx, user.Email, addr)
	if err != nil {
		s.logger().Error("panel: second factor throttle", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}
	if throttle.Blocked {
		_ = s.Store.Record(ctx, panel.AuditEntry{
			Action: panel.ActionLoginThrottled, ActorLabel: user.Email,
			Detail: map[string]any{"reason": throttle.Reason, "stage": "second_factor"},
		})
		s.renderSecondFactor(w, r, lang, http.StatusTooManyRequests, loginPage{
			Next: next, RememberedName: user.Name(),
			Error: s.throttleMessage(lang, throttle),
		})
		return
	}

	code := strings.TrimSpace(r.PostFormValue("kod"))
	err = s.Store.VerifyTOTP(ctx, user.ID, user.TOTPSecret, code, time.Now())
	switch {
	case err == nil:
		if err := s.Store.ClearLoginFailures(ctx, user.Email); err != nil {
			s.logger().Warn("panel: clearing login failures", "err", err)
		}
		s.completeLogin(w, r, lang, user, next)
		return

	case errors.Is(err, panel.ErrTOTPReplayed):
		// Its own sentence. "That code was already used" and "that code
		// is wrong" send a person to different places: one waits thirty
		// seconds, the other checks whether their clock is right or
		// whether they are reading the wrong account's row.
		s.recordFailedLogin(r, user.Email, addr)
		s.renderSecondFactor(w, r, lang, http.StatusUnauthorized, loginPage{
			Next: next, RememberedName: user.Name(),
			Error: lang.T("giris.hata.kod_kullanildi"),
		})
		return

	case errors.Is(err, panel.ErrTOTPInvalid):
		s.recordFailedLogin(r, user.Email, addr)
		s.renderSecondFactor(w, r, lang, http.StatusUnauthorized, loginPage{
			Next: next, RememberedName: user.Name(),
			Error: lang.T("giris.hata.kod_gecersiz"),
		})
		return

	default:
		s.logger().Error("panel: verifying totp", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
	}
}

// logoutHandler ends the session.
//
// POST only. A logout reachable by GET is a logout any page on the
// internet can perform with an <img> tag - harmless compared with most
// CSRF, and still a way to make a panel useless to whoever is being
// targeted.
func (s *Server) logoutHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
		return
	}
	if !s.Sessions.CheckCSRF(r) {
		s.Renderer.ErrorIn(w, r, statusCSRFExpired, lang)
		return
	}
	ctx := r.Context()
	if p, err := s.Sessions.Principal(ctx); err == nil {
		_ = s.Store.RecordFor(ctx, p, panel.AuditEntry{Action: panel.ActionLogout})
	}
	if err := s.Sessions.LogOut(ctx); err != nil {
		s.logger().Error("panel: logout", "err", err)
	}
	http.Redirect(w, r, LoginPath, http.StatusSeeOther)
}

// safeNext returns a destination that is certainly inside this panel.
//
// The whole reason a "next" parameter exists is that somebody clicked a
// link, got asked to sign in, and should land where they were going.
// The whole reason it is dangerous is that the value arrives from the
// URL, and a login form that will redirect anywhere is a phishing
// springboard wearing the customer's own domain: the address bar says
// this panel right up until the credentials are typed somewhere else.
//
// Rejected rather than sanitised. There is no repair for "//evil.test"
// that produces what the sender intended, so an unusable value becomes
// the site list, which is where signing in leads anyway.
func (s *Server) safeNext(next string) string {
	if clean := s.rawNext(next); clean != "" {
		return clean
	}
	return "/"
}

// rawNext is safeNext without the fallback, for putting back into a
// form's hidden field - where an empty value must stay empty rather
// than becoming a literal "/".
func (s *Server) rawNext(next string) string {
	// "/" is where signing in leads anyway, so carrying it is noise in
	// the address bar and one more value to validate for nothing.
	if next == "" || next == "/" {
		return ""
	}
	// A single leading slash, and nothing that could be read as an
	// authority. "//host" and "/\host" are both scheme-relative in
	// browsers, and the second is the one every hand-rolled check
	// forgets.
	if !strings.HasPrefix(next, "/") ||
		strings.HasPrefix(next, "//") ||
		strings.HasPrefix(next, "/\\") {
		return ""
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.Opaque != "" {
		return ""
	}
	// Sending somebody back to the login form after logging in is a
	// loop, and back to the developer wizard is a door their session
	// does not open.
	if u.Path == LoginPath || u.Path == SecondFactorPath ||
		strings.HasPrefix(u.Path, SetupPathPrefix) ||
		strings.HasPrefix(u.Path, DevAccessPathPrefix) {
		return ""
	}
	return u.String()
}

// withNext appends an already-validated destination.
func withNext(path, next string) string {
	if next == "" {
		return path
	}
	return path + "?next=" + url.QueryEscape(next)
}

// throttleMessage says how long to wait, in whole minutes.
//
// It names neither the account nor the address. Which of the two
// counters fired is in the audit log, where the operator can see it, and
// nowhere the person at the form can - "this address is blocked" versus
// "this account is blocked" confirms whether the account exists, which
// is the one thing the rest of this file works to keep quiet.
func (s *Server) throttleMessage(lang *ui.Language, t panel.Throttle) string {
	minutes := retryMinutes(t.RetryAfter)
	f := ui.NewFormatter(lang, s.Zone)
	return lang.Tn("giris.hata.kisitlandi", minutes, f.Number(int64(minutes)))
}

// retryMinutes rounds a wait up to whole minutes, never below one.
//
// Rounded because Throttle.RetryAfter is deliberately approximate: an
// exact countdown lets an attacker resume at precisely the right
// moment. "Try again in 3 minutes" is what a person needs, and it is
// all they get.
func retryMinutes(d time.Duration) int {
	minutes := int((d + time.Minute - 1) / time.Minute)
	if minutes < 1 {
		return 1
	}
	return minutes
}
