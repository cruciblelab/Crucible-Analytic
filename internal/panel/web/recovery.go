package web

import (
	"errors"
	"net/http"
	"strings"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// RecoveryPath is where somebody who cannot get in goes.
//
// Under /giris/ rather than at its own root, because it is a way of
// signing in and belongs next to the form it rescues - and because the
// sign-in page has to link to it prominently. A recovery route nobody
// can find from the page where they are stuck is a route that does not
// exist.
const RecoveryPath = "/giris/kurtarma"

// recoveryPage is Data for the recovery form.
type recoveryPage struct {
	Email string
	// ClearSecondFactor is the checkbox state, kept across a failed
	// attempt so somebody who mistyped a code does not have to notice
	// they must tick it again.
	ClearSecondFactor bool
	Error             string
	// LoginPath is the way back. Passed in rather than written into the
	// template so the route and the link cannot drift apart.
	LoginPath string
}

// recoveryHandler resets a password against a recovery code.
//
// # Everything here answers the same way
//
// A wrong address, a wrong code, a used code, a code belonging to
// somebody else, and a disabled account all produce one message. Telling
// any of them apart would answer, to anybody on the internet, which
// addresses have accounts on this deployment - and this page is reachable
// without signing in, which is precisely what makes it worth asking.
//
// # It shares the sign-in throttle
//
// Deliberately, and by address rather than by page: an attacker who
// found the password form rate-limited would otherwise simply move here
// and guess codes instead. The two forms are two doors into one account,
// so they count against one budget.
func (s *Server) recoveryHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)
	if !s.haveStore(w, r, lang) {
		return
	}
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.renderRecovery(w, r, lang, http.StatusOK, recoveryPage{})
	case http.MethodPost:
		if !s.acceptPost(w, r, lang) {
			return
		}
		s.submitRecovery(w, r, lang)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
	}
}

func (s *Server) submitRecovery(w http.ResponseWriter, r *http.Request, lang *ui.Language) {
	ctx := r.Context()
	email := strings.ToLower(strings.TrimSpace(r.PostFormValue("eposta")))
	typed := r.PostFormValue("kod")
	password := r.PostFormValue("yeni_parola")
	repeat := r.PostFormValue("yeni_parola_tekrar")
	clear := r.PostFormValue("ikinci_faktor") == "1"
	addr := peerAddr(r)

	page := recoveryPage{Email: email, ClearSecondFactor: clear}
	refuse := func(status int, msg string) {
		page.Error = msg
		s.renderRecovery(w, r, lang, status, page)
	}

	// The two checks that are about the form rather than the credential
	// come first, and neither counts as a failed attempt: somebody who
	// mistyped their new password twice has not guessed at anything.
	if password != repeat {
		refuse(http.StatusBadRequest, lang.T("hesap.hata.parolalar_farkli"))
		return
	}
	if err := panel.ValidatePassword(password); err != nil {
		refuse(http.StatusBadRequest, passwordProblem(lang, err))
		return
	}

	throttle, err := s.Store.CheckLoginThrottle(ctx, email, addr)
	if err != nil {
		s.logger().Error("panel: recovery throttle", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}
	if throttle.Blocked {
		_ = s.Store.Record(ctx, panel.AuditEntry{
			Action: panel.ActionLoginThrottled, ActorLabel: email,
			Detail: map[string]any{"reason": throttle.Reason, "form": "recovery"},
		})
		refuse(http.StatusTooManyRequests, s.throttleMessage(ctx, lang, throttle))
		return
	}

	hash, err := panel.HashPassword(password)
	if err != nil {
		s.logger().Error("panel: hashing password", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	result, err := s.Store.UseRecoveryCode(ctx, email, typed, hash, clear, addr)
	if err != nil {
		if errors.Is(err, panel.ErrRecoveryInvalid) {
			if err := s.Store.RecordLoginAttempt(ctx, email, addr, false); err != nil {
				s.logger().Warn("panel: recording a failed recovery", "err", err)
			}
			refuse(http.StatusUnauthorized, lang.T("kurtarma.hata.gecersiz"))
			return
		}
		s.logger().Error("panel: using a recovery code", "err", err)
		s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
		return
	}

	// Recorded before the session exists, so the trail survives a
	// failure to sign them in. This is the one entrance that goes round
	// both the password and the second factor, so what it says matters
	// more than most: who, from where, whether the second factor went
	// with it, and how much of the escape route is left.
	id := result.User.ID
	_ = s.Store.Record(ctx, panel.AuditEntry{
		Action: panel.ActionRecoveryCodeUsed, ActorKind: panel.PrincipalUser,
		ActorID: &id, ActorLabel: result.User.Email,
		Detail: map[string]any{
			"second_factor_cleared": result.SecondFactorCleared,
			"codes_remaining":       result.Remaining,
		},
	})
	if err := s.Store.ClearLoginFailures(ctx, email); err != nil {
		s.logger().Warn("panel: clearing login failures", "err", err)
	}

	// Signed in from here rather than sent to the form. They have just
	// proved who they are with a single-use credential and set a
	// password four seconds ago; asking them to type it again reads as
	// the panel not having believed them.
	//
	// The second factor is the exception and is not skipped: an account
	// that still has one still has to satisfy it, because a recovery
	// code that silently bypassed a second factor the holder still has
	// would make the second factor optional for anybody who found one.
	if result.User.HasTOTP() {
		if err := s.Sessions.AwaitSecondFactor(ctx, result.User); err != nil {
			s.logger().Error("panel: starting the second factor", "err", err)
			s.Renderer.ErrorIn(w, r, http.StatusInternalServerError, lang)
			return
		}
		http.Redirect(w, r, SecondFactorPath, http.StatusSeeOther)
		return
	}
	s.completeLogin(w, r, lang, result.User, "")
}

func (s *Server) renderRecovery(w http.ResponseWriter, r *http.Request, lang *ui.Language,
	status int, data recoveryPage) {

	data.LoginPath = LoginPath
	s.Renderer.Render(w, r, status, "kurtarma", &ui.Page{
		L:       lang,
		Title:   lang.T("kurtarma.giris.baslik"),
		Heading: lang.T("kurtarma.giris.baslik"),
		F:       ui.NewFormatter(lang, s.zone(r.Context())),
		CSRF:    s.Sessions.CSRFToken(r.Context()),
		Data:    data,
	})
}
