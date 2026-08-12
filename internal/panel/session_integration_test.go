//go:build integration

package panel

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// testPanel is a real HTTP server over the real session manager and a
// real database, driven by a real cookie jar. Sessions are stateful
// across requests by nature, so a recorder-per-request harness would
// prove nothing about them.
type testPanel struct {
	store    *Store
	sessions *Sessions
	server   *httptest.Server
	client   *http.Client

	// httptest fixes its handler at construction, so routes a test needs
	// to add afterwards live here and are consulted first. Guarded by a
	// mutex because the server goroutine reads it while the test
	// goroutine writes.
	extraMu sync.Mutex
	extra   map[string]http.HandlerFunc
}

func newTestPanel(t *testing.T, ns string) *testPanel {
	t.Helper()
	store := newTestStore(t, ns)
	// secureCookies=false because httptest serves plain HTTP; a Secure
	// cookie would never be stored and every test would silently pass
	// by testing nothing.
	sessions := NewSessions(store, time.Hour, false)

	tp := &testPanel{store: store, sessions: sessions}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /whoami", func(w http.ResponseWriter, r *http.Request) {
		p, err := sessions.Principal(r.Context())
		if errors.Is(err, ErrNoSession) {
			http.Error(w, "anonymous", http.StatusUnauthorized)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write([]byte(string(p.Kind) + ":" + p.Label))
	})
	mux.HandleFunc("GET /csrf", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sessions.CSRFToken(r.Context())))
	})
	mux.HandleFunc("POST /write", func(w http.ResponseWriter, r *http.Request) {
		if !sessions.CheckCSRF(r) {
			http.Error(w, "bad csrf", http.StatusForbidden)
			return
		}
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("POST /logout", func(w http.ResponseWriter, r *http.Request) {
		if err := sessions.LogOut(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write([]byte("bye"))
	})

	root := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tp.extraMu.Lock()
		h, ok := tp.extra[r.URL.Path]
		tp.extraMu.Unlock()
		if ok {
			h(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

	tp.server = httptest.NewServer(sessions.Middleware(root))
	t.Cleanup(tp.server.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	tp.client = &http.Client{Jar: jar}
	return tp
}

// signIn drives a login through a real request, which is the only way
// the session manager will actually persist it.
func (tp *testPanel) signIn(t *testing.T, user User) {
	t.Helper()
	tp.route(t, "/signin-"+user.Email, func(w http.ResponseWriter, r *http.Request) {
		if err := tp.sessions.LogIn(r.Context(), user); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write([]byte("in"))
	})
	if body, code := tp.get(t, "/signin-"+user.Email); code != http.StatusOK {
		t.Fatalf("sign-in failed: %d %s", code, body)
	}
}

// route registers a handler after the server is running. httptest's
// handler is fixed at construction, so tests that need a bespoke route
// mount it through this indirection instead.
func (tp *testPanel) route(t *testing.T, path string, h http.HandlerFunc) {
	t.Helper()
	tp.extraMu.Lock()
	defer tp.extraMu.Unlock()
	if tp.extra == nil {
		tp.extra = map[string]http.HandlerFunc{}
	}
	tp.extra[path] = h
}

func (tp *testPanel) get(t *testing.T, path string) (string, int) {
	t.Helper()
	resp, err := tp.client.Get(tp.server.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(body)), resp.StatusCode
}

func (tp *testPanel) post(t *testing.T, path string, form url.Values) (string, int) {
	t.Helper()
	resp, err := tp.client.PostForm(tp.server.URL+path, form)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(body)), resp.StatusCode
}

// sessionCookie returns the current session cookie value, or "".
func (tp *testPanel) sessionCookie(t *testing.T) string {
	t.Helper()
	u, err := url.Parse(tp.server.URL)
	if err != nil {
		t.Fatalf("parsing server URL: %v", err)
	}
	for _, c := range tp.client.Jar.Cookies(u) {
		if c.Name == sessionCookieName {
			return c.Value
		}
	}
	return ""
}

// A code stays valid across three 30-second steps to tolerate clock
// drift, so without this a code observed over a shoulder or captured by
// a phishing proxy remains usable for up to ninety seconds - ample for
// an attacker who already has the password.
func TestStore_RealDB_TOTPCodeCannotBeUsedTwice(t *testing.T) {
	ns := "panel-totpreplay"
	s := newTestStore(t, ns)
	ctx := context.Background()

	user := mustUser(t, s, ns, "user", false)
	secret := newSecret(t)
	if err := s.SetTOTPSecret(ctx, user.ID, secret); err != nil {
		t.Fatalf("SetTOTPSecret: %v", err)
	}

	now := time.Now()
	code := codeAt(t, secret, now)

	if err := s.VerifyTOTP(ctx, user.ID, secret, code, now); err != nil {
		t.Fatalf("first use of a valid code failed: %v", err)
	}
	if err := s.VerifyTOTP(ctx, user.ID, secret, code, now); !errors.Is(err, ErrTOTPReplayed) {
		t.Errorf("second use gave %v, want ErrTOTPReplayed", err)
	}

	// An older code is refused too: accepting one would let an attacker
	// replay anything from the tolerated window.
	older := codeAt(t, secret, now.Add(-totpPeriod*time.Second))
	if err := s.VerifyTOTP(ctx, user.ID, secret, older, now); !errors.Is(err, ErrTOTPReplayed) {
		t.Errorf("an earlier step's code gave %v, want ErrTOTPReplayed", err)
	}

	// The next step's code is still fine - this locks out replays, not
	// the user.
	next := now.Add(totpPeriod * time.Second)
	if err := s.VerifyTOTP(ctx, user.ID, secret, codeAt(t, secret, next), next); err != nil {
		t.Errorf("the next code was refused: %v", err)
	}

	if err := s.VerifyTOTP(ctx, user.ID, secret, "000000", now); !errors.Is(err, ErrTOTPInvalid) {
		t.Errorf("a wrong code gave %v, want ErrTOTPInvalid", err)
	}
}

// Two submissions of the same code arriving together must not both
// pass; the check and the record are one statement precisely for this.
func TestStore_RealDB_ConcurrentTOTPUseAdmitsOne(t *testing.T) {
	ns := "panel-totprace"
	s := newTestStore(t, ns)
	ctx := context.Background()

	user := mustUser(t, s, ns, "user", false)
	secret := newSecret(t)
	if err := s.SetTOTPSecret(ctx, user.ID, secret); err != nil {
		t.Fatalf("SetTOTPSecret: %v", err)
	}

	now := time.Now()
	code := codeAt(t, secret, now)

	const attempts = 12
	var wg sync.WaitGroup
	results := make([]error, attempts)
	start := make(chan struct{})
	for i := range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = s.VerifyTOTP(context.Background(), user.ID, secret, code, now)
		}()
	}
	close(start)
	wg.Wait()

	accepted := 0
	for _, err := range results {
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrTOTPReplayed):
		default:
			t.Errorf("unexpected error: %v", err)
		}
	}
	if accepted != 1 {
		t.Errorf("%d of %d concurrent uses of one code succeeded, want exactly 1", accepted, attempts)
	}
}

func TestSessions_RealDB_SignInAndOut(t *testing.T) {
	ns := "panel-session"
	tp := newTestPanel(t, ns)

	if body, code := tp.get(t, "/whoami"); code != http.StatusUnauthorized {
		t.Fatalf("anonymous /whoami = %d %q, want 401", code, body)
	}

	user := mustUser(t, tp.store, ns, "ahmet", false)
	tp.signIn(t, user)

	body, code := tp.get(t, "/whoami")
	if code != http.StatusOK || body != "user:"+user.Email {
		t.Fatalf("/whoami after sign-in = %d %q", code, body)
	}

	if _, code := tp.post(t, "/logout", nil); code != http.StatusOK {
		t.Fatalf("logout = %d", code)
	}
	if body, code := tp.get(t, "/whoami"); code != http.StatusUnauthorized {
		t.Errorf("/whoami after logout = %d %q, want 401", code, body)
	}
}

// Session fixation: without RenewToken the identifier the browser
// carried before authenticating survives into the authenticated
// session, so anyone who fixed a known value there beforehand is now
// signed in as the victim. One line, invisible when missing.
func TestSessions_RealDB_SignInRotatesTheSessionIdentifier(t *testing.T) {
	ns := "panel-fixation"
	tp := newTestPanel(t, ns)

	// Touch the session so a pre-authentication cookie exists.
	tp.get(t, "/csrf")
	before := tp.sessionCookie(t)
	if before == "" {
		t.Fatal("no session cookie was issued before sign-in")
	}

	user := mustUser(t, tp.store, ns, "ahmet", false)
	tp.signIn(t, user)

	after := tp.sessionCookie(t)
	if after == "" {
		t.Fatal("no session cookie after sign-in")
	}
	if after == before {
		t.Error("the session identifier survived authentication; a fixed cookie would now be an authenticated one")
	}
}

// Disabling an account has to take hold on the next click, not whenever
// the session happens to expire - which is why the user row is reloaded
// per request rather than cached in the session.
func TestSessions_RealDB_DisablingAnAccountEndsItsSessionImmediately(t *testing.T) {
	ns := "panel-disable"
	tp := newTestPanel(t, ns)
	ctx := context.Background()

	user := mustUser(t, tp.store, ns, "ahmet", false)
	tp.signIn(t, user)
	if _, code := tp.get(t, "/whoami"); code != http.StatusOK {
		t.Fatal("sign-in did not take")
	}

	if err := tp.store.SetDisabled(ctx, user.ID, true); err != nil {
		t.Fatalf("SetDisabled: %v", err)
	}

	if body, code := tp.get(t, "/whoami"); code != http.StatusUnauthorized {
		t.Errorf("a disabled account still had a session: %d %q", code, body)
	}
}

// A session naming an account that no longer exists is not an error to
// show anybody; it is simply not a session.
func TestSessions_RealDB_DeletedAccountIsNotASession(t *testing.T) {
	ns := "panel-deleted"
	tp := newTestPanel(t, ns)
	ctx := context.Background()

	user := mustUser(t, tp.store, ns, "ahmet", false)
	tp.signIn(t, user)

	if _, err := tp.store.pool.Exec(ctx, `DELETE FROM panel_users WHERE id = $1`, user.ID); err != nil {
		t.Fatalf("deleting the account: %v", err)
	}
	if body, code := tp.get(t, "/whoami"); code != http.StatusUnauthorized {
		t.Errorf("a deleted account still had a session: %d %q", code, body)
	}
}

func TestSessions_RealDB_CSRF(t *testing.T) {
	ns := "panel-csrf"
	tp := newTestPanel(t, ns)

	token, code := tp.get(t, "/csrf")
	if code != http.StatusOK || token == "" {
		t.Fatalf("/csrf = %d %q", code, token)
	}
	// Stable within a session, or every open tab would invalidate the
	// others' forms.
	again, _ := tp.get(t, "/csrf")
	if again != token {
		t.Errorf("the token changed between requests: %q then %q", token, again)
	}

	if body, code := tp.post(t, "/write", url.Values{CSRFFieldName: {token}}); code != http.StatusOK {
		t.Errorf("a correct token was rejected: %d %q", code, body)
	}
	for name, form := range map[string]url.Values{
		"missing": nil,
		"empty":   {CSRFFieldName: {""}},
		"wrong":   {CSRFFieldName: {"not-the-token"}},
		"prefix":  {CSRFFieldName: {token[:len(token)-1]}},
	} {
		if _, code := tp.post(t, "/write", form); code != http.StatusForbidden {
			t.Errorf("%s token gave %d, want 403", name, code)
		}
	}
}

// A fresh session that has never been issued a token must fail closed
// rather than accept whatever the request carried.
func TestSessions_RealDB_CSRFFailsClosedWithoutAnIssuedToken(t *testing.T) {
	ns := "panel-csrfclosed"
	tp := newTestPanel(t, ns)

	if _, code := tp.post(t, "/write", url.Values{CSRFFieldName: {"anything"}}); code != http.StatusForbidden {
		t.Error("a session with no issued token accepted a write")
	}
}

// Approving developer access grants a visit, so the visit has to end on
// its own schedule rather than whenever the browser is closed.
func TestSessions_RealDB_DeveloperSessionExpiresWithItsGrant(t *testing.T) {
	ns := "panel-devsession"
	tp := newTestPanel(t, ns)

	tp.route(t, "/devin-live", func(w http.ResponseWriter, r *http.Request) {
		grant := DevAccessGrant{ID: 1, ExpiresAt: time.Now().Add(time.Hour)}
		if err := tp.sessions.LogInDeveloper(r.Context(), grant); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write([]byte("in"))
	})
	tp.route(t, "/devin-stale", func(w http.ResponseWriter, r *http.Request) {
		grant := DevAccessGrant{ID: 2, ExpiresAt: time.Now().Add(-time.Minute)}
		if err := tp.sessions.LogInDeveloper(r.Context(), grant); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write([]byte("in"))
	})

	tp.get(t, "/devin-live")
	body, code := tp.get(t, "/whoami")
	if code != http.StatusOK || body != "developer:"+DeveloperLabel {
		t.Fatalf("/whoami during a developer session = %d %q", code, body)
	}

	tp.get(t, "/devin-stale")
	if body, code := tp.get(t, "/whoami"); code != http.StatusUnauthorized {
		t.Errorf("an expired developer grant still had a session: %d %q", code, body)
	}
}
