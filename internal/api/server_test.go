package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeStore records what it was asked and returns canned data, so the
// HTTP/auth/parameter layer can be tested without a live database.
type fakeStore struct {
	sites []string
	err   error

	// Captured arguments from the last call, for asserting that query
	// parameters actually reach the store.
	gotSite        string
	gotFrom, gotTo time.Time
	gotInterval    string
	gotLimit       int
	gotBotScoreMin int
}

func (f *fakeStore) Sites(ctx context.Context) ([]string, error) {
	return f.sites, f.err
}

func (f *fakeStore) Summary(ctx context.Context, siteID string, from, to time.Time, botScoreMin int) (Summary, error) {
	f.gotSite, f.gotFrom, f.gotTo, f.gotBotScoreMin = siteID, from, to, botScoreMin
	if f.err != nil {
		return Summary{}, f.err
	}
	return Summary{SiteID: siteID, From: from, To: to, UniqueIPs: 3, BotIPs: 1, HumanIPs: 2, BotScoreMin: botScoreMin}, nil
}

func (f *fakeStore) Timeseries(ctx context.Context, siteID string, from, to time.Time, interval string, botScoreMin int) ([]Bucket, error) {
	f.gotSite, f.gotFrom, f.gotTo, f.gotInterval, f.gotBotScoreMin = siteID, from, to, interval, botScoreMin
	if f.err != nil {
		return nil, f.err
	}
	return []Bucket{{Time: from, UniqueIPs: 2, BotIPs: 1}}, nil
}

func (f *fakeStore) TopIPs(ctx context.Context, siteID string, from, to time.Time, limit int) ([]IPStat, error) {
	f.gotSite, f.gotFrom, f.gotTo, f.gotLimit = siteID, from, to, limit
	if f.err != nil {
		return nil, f.err
	}
	return []IPStat{{IP: "203.0.113.1", PeakScore: 90}}, nil
}

func (f *fakeStore) Countries(ctx context.Context, siteID string, from, to time.Time, limit, botScoreMin int) ([]GroupStat, error) {
	f.gotSite, f.gotFrom, f.gotTo, f.gotLimit, f.gotBotScoreMin = siteID, from, to, limit, botScoreMin
	if f.err != nil {
		return nil, f.err
	}
	return []GroupStat{{Key: "US", UniqueIPs: 2, BotIPs: 1}}, nil
}

func (f *fakeStore) ASNs(ctx context.Context, siteID string, from, to time.Time, limit, botScoreMin int) ([]GroupStat, error) {
	f.gotSite, f.gotFrom, f.gotTo, f.gotLimit, f.gotBotScoreMin = siteID, from, to, limit, botScoreMin
	if f.err != nil {
		return nil, f.err
	}
	return []GroupStat{{Key: "15169", Label: "GOOGLE", UniqueIPs: 2}}, nil
}

// fixedNow is the clock every test server uses, so default time ranges
// are deterministic.
var fixedNow = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// newTestServer wires a Server over the given store with two tokens:
// "panel" (wildcard) and "ahmet" (only site-a).
func newTestServer(t *testing.T, store Querier) http.Handler {
	t.Helper()
	auth, err := NewAuthenticator([]Token{
		testToken("panel", "panel-secret", WildcardSite),
		testToken("ahmet", "ahmet-secret", "site-a"),
	})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	srv := &Server{Store: store, Auth: auth, Now: func() time.Time { return fixedNow }}
	return srv.Handler()
}

// do issues a request with an optional bearer token and returns the
// recorder.
func do(h http.Handler, method, target, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestServer_RequiresAuthOnEveryAPIRoute(t *testing.T) {
	h := newTestServer(t, &fakeStore{})

	for _, path := range []string{
		"/api/v1/sites",
		"/api/v1/sites/site-a/summary",
		"/api/v1/sites/site-a/timeseries",
		"/api/v1/sites/site-a/top-ips",
		"/api/v1/sites/site-a/countries",
		"/api/v1/sites/site-a/asns",
	} {
		t.Run(path, func(t *testing.T) {
			w := do(h, http.MethodGet, path, "")
			if w.Code != http.StatusUnauthorized {
				t.Errorf("no-token status = %d, want %d", w.Code, http.StatusUnauthorized)
			}
			if got := w.Header().Get("WWW-Authenticate"); got == "" {
				t.Error("401 response has no WWW-Authenticate header")
			}

			if w := do(h, http.MethodGet, path, "wrong-secret"); w.Code != http.StatusUnauthorized {
				t.Errorf("bad-token status = %d, want %d", w.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestServer_HealthzNeedsNoToken(t *testing.T) {
	h := newTestServer(t, &fakeStore{})
	w := do(h, http.MethodGet, "/healthz", "")
	if w.Code != http.StatusOK {
		t.Errorf("healthz status = %d, want 200 (it must be usable by an uptime check with no credential)", w.Code)
	}
}

func TestServer_TokenCannotReadAnotherSite(t *testing.T) {
	h := newTestServer(t, &fakeStore{})

	// ahmet's token is scoped to site-a only.
	if w := do(h, http.MethodGet, "/api/v1/sites/site-a/summary", "ahmet-secret"); w.Code != http.StatusOK {
		t.Errorf("ahmet reading its own site: status = %d, want 200", w.Code)
	}
	w := do(h, http.MethodGet, "/api/v1/sites/site-b/summary", "ahmet-secret")
	if w.Code != http.StatusForbidden {
		t.Errorf("ahmet reading someone else's site: status = %d, want %d", w.Code, http.StatusForbidden)
	}

	// The wildcard token may read both.
	for _, site := range []string{"site-a", "site-b"} {
		if w := do(h, http.MethodGet, "/api/v1/sites/"+site+"/summary", "panel-secret"); w.Code != http.StatusOK {
			t.Errorf("panel reading %s: status = %d, want 200", site, w.Code)
		}
	}
}

func TestServer_SiteAuthorizationCoversEveryPerSiteRoute(t *testing.T) {
	// Every per-site route must enforce the site grant - a route that
	// forgot the check would leak another customer's data, so this is
	// asserted per route rather than assumed from one sample.
	h := newTestServer(t, &fakeStore{})
	for _, suffix := range []string{"summary", "timeseries", "top-ips", "countries", "asns"} {
		t.Run(suffix, func(t *testing.T) {
			w := do(h, http.MethodGet, "/api/v1/sites/site-b/"+suffix, "ahmet-secret")
			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want %d for a site outside the token's grant", w.Code, http.StatusForbidden)
			}
		})
	}
}

func TestServer_SitesListsOnlyWhatTheTokenCanSee(t *testing.T) {
	store := &fakeStore{sites: []string{"site-a", "site-b", "site-c"}}
	h := newTestServer(t, store)

	// Wildcard token: everything the database holds.
	w := do(h, http.MethodGet, "/api/v1/sites", "panel-secret")
	var got struct {
		Sites []string `json:"sites"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got.Sites) != 3 {
		t.Errorf("panel sees %v, want all 3 sites", got.Sites)
	}

	// Scoped token: only its own, and crucially not the names of sites it
	// can't read.
	w = do(h, http.MethodGet, "/api/v1/sites", "ahmet-secret")
	got.Sites = nil
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if len(got.Sites) != 1 || got.Sites[0] != "site-a" {
		t.Errorf("ahmet sees %v, want only [site-a] - other sites' names must not leak", got.Sites)
	}
}

func TestServer_NonGETIsRejected(t *testing.T) {
	h := newTestServer(t, &fakeStore{})
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		t.Run(method, func(t *testing.T) {
			w := do(h, method, "/api/v1/sites/site-a/summary", "panel-secret")
			if w.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want %d - this API is read-only", w.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}

func TestServer_DefaultTimeRangeIsLast24Hours(t *testing.T) {
	store := &fakeStore{}
	h := newTestServer(t, store)

	if w := do(h, http.MethodGet, "/api/v1/sites/site-a/summary", "panel-secret"); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !store.gotTo.Equal(fixedNow) {
		t.Errorf("default to = %v, want %v", store.gotTo, fixedNow)
	}
	if want := fixedNow.Add(-24 * time.Hour); !store.gotFrom.Equal(want) {
		t.Errorf("default from = %v, want %v", store.gotFrom, want)
	}
}

func TestServer_QueryParametersReachTheStore(t *testing.T) {
	store := &fakeStore{}
	h := newTestServer(t, store)

	w := do(h, http.MethodGet,
		"/api/v1/sites/site-a/timeseries?from=2026-05-01T00:00:00Z&to=2026-05-02T00:00:00Z&interval=15+minutes&bot_score_min=70",
		"panel-secret")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if store.gotSite != "site-a" {
		t.Errorf("site = %q, want site-a", store.gotSite)
	}
	if want := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC); !store.gotFrom.Equal(want) {
		t.Errorf("from = %v, want %v", store.gotFrom, want)
	}
	if want := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC); !store.gotTo.Equal(want) {
		t.Errorf("to = %v, want %v", store.gotTo, want)
	}
	if store.gotInterval != "15 minutes" {
		t.Errorf("interval = %q, want \"15 minutes\"", store.gotInterval)
	}
	if store.gotBotScoreMin != 70 {
		t.Errorf("bot_score_min = %d, want 70", store.gotBotScoreMin)
	}
}

func TestServer_InvalidParametersAre400(t *testing.T) {
	h := newTestServer(t, &fakeStore{})

	tests := []struct {
		name   string
		target string
	}{
		{"bad from", "/api/v1/sites/site-a/summary?from=yesterday"},
		{"bad to", "/api/v1/sites/site-a/summary?to=nope"},
		{"inverted range", "/api/v1/sites/site-a/summary?from=2026-05-02T00:00:00Z&to=2026-05-01T00:00:00Z"},
		{"range too long", "/api/v1/sites/site-a/summary?from=2020-01-01T00:00:00Z&to=2026-01-01T00:00:00Z"},
		{"disallowed interval", "/api/v1/sites/site-a/timeseries?interval=1+second"},
		{"nonsense interval", "/api/v1/sites/site-a/timeseries?interval=DROP+TABLE"},
		{"bad limit", "/api/v1/sites/site-a/top-ips?limit=abc"},
		{"limit too large", "/api/v1/sites/site-a/top-ips?limit=100000"},
		{"limit zero", "/api/v1/sites/site-a/top-ips?limit=0"},
		{"bot score out of range", "/api/v1/sites/site-a/summary?bot_score_min=500"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := do(h, http.MethodGet, tt.target, "panel-secret")
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d (body: %s)", w.Code, http.StatusBadRequest, w.Body)
			}
		})
	}
}

func TestServer_StoreErrorIsGenericised(t *testing.T) {
	// A database error must never reach the client verbatim - it can
	// carry schema details, hostnames, or credentials.
	store := &fakeStore{err: errors.New("pq: password authentication failed for user \"secret\"")}
	h := newTestServer(t, store)

	w := do(h, http.MethodGet, "/api/v1/sites/site-a/summary", "panel-secret")
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", w.Code, http.StatusInternalServerError)
	}
	if !json.Valid(w.Body.Bytes()) {
		t.Errorf("error body is not valid JSON: %s", w.Body)
	}
	for _, leaked := range []string{"password", "secret", "pq:"} {
		if strings.Contains(w.Body.String(), leaked) {
			t.Errorf("error body %q leaked %q from the underlying error", w.Body, leaked)
		}
	}
}

func TestServer_ResponsesAreJSON(t *testing.T) {
	h := newTestServer(t, &fakeStore{sites: []string{"site-a"}})

	for _, path := range []string{
		"/api/v1/sites",
		"/api/v1/sites/site-a/summary",
		"/api/v1/sites/site-a/timeseries",
		"/api/v1/sites/site-a/top-ips",
		"/api/v1/sites/site-a/countries",
		"/api/v1/sites/site-a/asns",
	} {
		t.Run(path, func(t *testing.T) {
			w := do(h, http.MethodGet, path, "panel-secret")
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
			}
			if ct := w.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if !json.Valid(w.Body.Bytes()) {
				t.Errorf("body is not valid JSON: %s", w.Body)
			}
		})
	}
}
