package panel

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"time"

	"github.com/alexedwards/scs/pgxstore"
	"github.com/alexedwards/scs/v2"
)

// Session defaults.
const (
	// DefaultSessionLifetime is how long a signed-in session lasts
	// without activity. Long enough to work through an afternoon,
	// short enough that a forgotten browser on a shared machine is not
	// a standing invitation.
	DefaultSessionLifetime = 12 * time.Hour
	// sessionCookieName carries no product name: a cookie called
	// "crucible_panel" tells anyone reading a request what software is
	// running here, which is free reconnaissance.
	sessionCookieName = "cap_session"
)

// Session keys. Unexported constants rather than inline strings so a
// typo cannot silently create a second, always-empty key.
const (
	keyUserID     = "user_id"
	keyDevGrantID = "dev_grant_id"
	// keyDevExpires holds Unix seconds rather than a time.Time.
	//
	// scs encodes the session with gob, and gob cannot encode a
	// time.Time held in an interface{} unless the concrete type has been
	// registered globally. It fails quietly rather than loudly: the
	// value comes back as the zero time, which this package reads as
	// "expired" - so a developer session would appear to work and then
	// be rejected on the very next request. An integer sidesteps the
	// whole question and needs no package-level gob.Register side
	// effect.
	keyDevExpires = "dev_expires_unix"
	// keyPendingUserID holds the account that passed a password check
	// and still owes a second factor. Deliberately separate from
	// keyUserID: a half-finished login must not be a login, and keeping
	// them in one field would make that one forgotten branch away.
	keyPendingUserID = "pending_user_id"
	keyCSRF          = "csrf"
	// keyPendingTOTP holds a second-factor secret that has been shown to
	// the user but not yet proved. It is in the session and not on the
	// user row deliberately: enrolment that is abandoned halfway must
	// leave the account exactly as it was, because the alternative -
	// writing the secret first - creates the one state this panel
	// cannot repair on its own, an account demanding codes from an
	// authenticator that never finished scanning.
	//
	// It dies with the session, which is the right lifetime: a secret
	// nobody confirmed is worth nothing, and one that outlived the tab
	// it was created in would be a credential lying about.
	keyPendingTOTP = "pending_totp"
)

// Sessions manages signed-in state.
type Sessions struct {
	mgr   *scs.SessionManager
	store *Store
}

// NewSessions wires a session manager over the panel's own database.
//
// Sessions live in Postgres rather than in a signed cookie, which costs
// a query per request and buys the thing a stateless token cannot give:
// revocation. Disabling an account or signing out ends the session
// immediately, everywhere, rather than whenever the token happens to
// expire.
func NewSessions(store *Store, lifetime time.Duration, secureCookies bool) *Sessions {
	if lifetime <= 0 {
		lifetime = DefaultSessionLifetime
	}

	mgr := scs.New()
	mgr.Store = pgxstore.NewWithConfig(store.Pool(), pgxstore.Config{TableName: "panel_sessions"})
	mgr.Lifetime = lifetime
	mgr.IdleTimeout = lifetime
	mgr.Cookie.Name = sessionCookieName
	mgr.Cookie.HttpOnly = true
	mgr.Cookie.Path = "/"
	// Lax rather than Strict: Strict would sign a user out whenever they
	// arrived from any other site, including their own bookmarks bar in
	// some browsers, and the cross-site POST that Strict additionally
	// blocks is already refused by the synchronizer token below.
	mgr.Cookie.SameSite = http.SameSiteLaxMode
	// Secure is a deployment fact, not a preference: over plain HTTP the
	// cookie travels in clear text. It is configurable only so the panel
	// can be exercised on http://localhost during development, and the
	// config file says so.
	mgr.Cookie.Secure = secureCookies
	mgr.Cookie.Persist = true

	return &Sessions{mgr: mgr, store: store}
}

// Middleware loads and saves the session around each request.
func (s *Sessions) Middleware(next http.Handler) http.Handler {
	return s.mgr.LoadAndSave(next)
}

// LogIn establishes a signed-in session for a user.
//
// RenewToken first, always. Without it the session identifier from
// before authentication survives into the authenticated session, so
// anyone who managed to fix a known identifier in the victim's browser
// beforehand - session fixation - is now signed in as them. It is one
// line, it is invisible when missing, and it is the single most
// commonly omitted step in hand-rolled login code.
func (s *Sessions) LogIn(ctx context.Context, user User) error {
	if err := s.mgr.RenewToken(ctx); err != nil {
		return err
	}
	s.mgr.Remove(ctx, keyPendingUserID)
	s.mgr.Remove(ctx, keyDevGrantID)
	s.mgr.Remove(ctx, keyDevExpires)
	s.mgr.Put(ctx, keyUserID, user.ID)
	return nil
}

// LogInDeveloper establishes a session from a redeemed developer grant.
func (s *Sessions) LogInDeveloper(ctx context.Context, grant DevAccessGrant) error {
	if err := s.mgr.RenewToken(ctx); err != nil {
		return err
	}
	s.mgr.Remove(ctx, keyPendingUserID)
	s.mgr.Remove(ctx, keyUserID)
	s.mgr.Put(ctx, keyDevGrantID, grant.ID)
	s.mgr.Put(ctx, keyDevExpires, grant.ExpiresAt.Unix())
	return nil
}

// AwaitSecondFactor records that a password check passed and a code is
// still owed. Renews the token here too: the identifier the browser
// carried before it proved anything about itself should not be the one
// that eventually becomes authenticated.
func (s *Sessions) AwaitSecondFactor(ctx context.Context, user User) error {
	if err := s.mgr.RenewToken(ctx); err != nil {
		return err
	}
	s.mgr.Remove(ctx, keyUserID)
	s.mgr.Put(ctx, keyPendingUserID, user.ID)
	return nil
}

// PendingUserID returns the account awaiting a second factor, or 0.
func (s *Sessions) PendingUserID(ctx context.Context) int64 {
	if s == nil || s.mgr == nil {
		return 0
	}
	return s.mgr.GetInt64(ctx, keyPendingUserID)
}

// LogOut destroys the session server-side, so the cookie is worthless
// even if it has already been copied somewhere.
func (s *Sessions) LogOut(ctx context.Context) error {
	return s.mgr.Destroy(ctx)
}

// ErrNoSession means nobody is signed in.
var ErrNoSession = errors.New("panel: no session")

// Principal resolves who is making this request.
//
// The user row is reloaded on every request rather than cached in the
// session. That costs a query and buys immediate effect: disabling an
// account, revoking developer mode or changing a role takes hold on the
// next click rather than whenever the session happens to expire.
func (s *Sessions) Principal(ctx context.Context) (Principal, error) {
	// A nil manager is "nobody is signed in", not a crash.
	//
	// Server.Handler already treats a nil Sessions as a server with no
	// session middleware, so it is a state this package has to answer
	// for rather than assume away - and the answer that keeps a panel
	// safe is the one where nobody is authenticated. A deployment that
	// reaches this is refused at startup instead; see
	// web.Server.ListenAndServe.
	if s == nil || s.mgr == nil {
		return Principal{}, ErrNoSession
	}
	if grantID := s.mgr.GetInt64(ctx, keyDevGrantID); grantID != 0 {
		expiresUnix := s.mgr.GetInt64(ctx, keyDevExpires)
		if expiresUnix == 0 || time.Now().After(time.Unix(expiresUnix, 0)) {
			// The grant's own lifetime, separate from the session
			// cookie's: approving developer access grants a visit, and
			// the visit ending must not depend on the browser being
			// closed.
			_ = s.mgr.Destroy(ctx)
			return Principal{}, ErrNoSession
		}
		return developerPrincipal(), nil
	}

	userID := s.mgr.GetInt64(ctx, keyUserID)
	if userID == 0 {
		return Principal{}, ErrNoSession
	}

	user, err := s.store.UserByID(ctx, userID)
	if err != nil {
		// A session naming an account that no longer exists is not an
		// error to show anybody; it is simply not a session.
		if errors.Is(err, ErrNotFound) {
			_ = s.mgr.Destroy(ctx)
			return Principal{}, ErrNoSession
		}
		return Principal{}, err
	}
	if user.Disabled {
		_ = s.mgr.Destroy(ctx)
		return Principal{}, ErrNoSession
	}

	return Principal{
		Kind:          PrincipalUser,
		UserID:        user.ID,
		Label:         user.Email,
		Superadmin:    user.IsSuperadmin,
		DeveloperMode: user.DeveloperMode,
	}, nil
}

// CSRFToken returns this session's synchronizer token, creating one on
// first use.
//
// SameSite=Lax already blocks the cross-site form POST this defends
// against in every browser that honours it. The token is here anyway
// because "in every browser that honours it" is doing real work in that
// sentence, and because a single misconfigured proxy stripping SameSite
// should not be the only thing between a customer and an attacker's
// form.
func (s *Sessions) CSRFToken(ctx context.Context) string {
	// No manager, no token - and CheckCSRF below then refuses every
	// write, which is the direction a missing defence must fail in.
	if s == nil || s.mgr == nil {
		return ""
	}
	if token := s.mgr.GetString(ctx, keyCSRF); token != "" {
		return token
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		// Without randomness there is no safe token to issue. Returning
		// "" means every check below fails closed, which blocks writes
		// rather than accepting unverified ones.
		return ""
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	s.mgr.Put(ctx, keyCSRF, token)
	return token
}

// CSRFFieldName is the form field the token travels in.
const CSRFFieldName = "csrf_token"

// CheckCSRF verifies the token on an unsafe request.
func (s *Sessions) CheckCSRF(r *http.Request) bool {
	if s == nil || s.mgr == nil {
		return false
	}
	want := s.mgr.GetString(r.Context(), keyCSRF)
	if want == "" {
		// No token was ever issued for this session, so nothing can
		// legitimately match one. Fail closed.
		return false
	}
	got := r.PostFormValue(CSRFFieldName)
	if got == "" {
		// Also accept a header, so a fetch() from the panel's own
		// scripts does not have to build a form body.
		got = r.Header.Get("X-CSRF-Token")
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// safeMethod reports whether a method is defined as read-only and
// therefore needs no CSRF token.
func safeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

// PutPendingTOTP stores an unconfirmed second-factor secret.
func (s *Sessions) PutPendingTOTP(ctx context.Context, secret string) {
	s.mgr.Put(ctx, keyPendingTOTP, secret)
}

// PendingTOTP returns the unconfirmed secret, or "".
func (s *Sessions) PendingTOTP(ctx context.Context) string {
	if s == nil || s.mgr == nil {
		return ""
	}
	return s.mgr.GetString(ctx, keyPendingTOTP)
}

// ClearPendingTOTP forgets an unconfirmed secret.
//
// Called both when enrolment succeeds and when it is abandoned. After
// success the secret is on the user row and a second copy in the session
// is one more place it can leak from; after abandonment it is simply
// worthless.
func (s *Sessions) ClearPendingTOTP(ctx context.Context) {
	s.mgr.Remove(ctx, keyPendingTOTP)
}
