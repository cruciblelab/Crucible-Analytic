package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testToken builds a valid Token for a raw secret, so tests never have to
// hand-write a hash (and can't accidentally test against a stale one).
func testToken(name, raw string, sites ...string) Token {
	return Token{Name: name, SHA256: HashToken(raw), Sites: sites}
}

func requestWithAuth(header string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/sites", nil)
	if header != "" {
		r.Header.Set("Authorization", header)
	}
	return r
}

func TestNewAuthenticator_RejectsEmptyTokenList(t *testing.T) {
	// An empty list must be an error, not "allow everything" - a config
	// that forgot its tokens should fail at startup, loudly.
	if _, err := NewAuthenticator(nil); err == nil {
		t.Error("NewAuthenticator(nil) error = nil, want an error")
	}
	if _, err := NewAuthenticator([]Token{}); err == nil {
		t.Error("NewAuthenticator(empty) error = nil, want an error")
	}
}

func TestNewAuthenticator_RejectsMalformedTokens(t *testing.T) {
	tests := []struct {
		name  string
		token Token
	}{
		{"no name", Token{SHA256: HashToken("secret"), Sites: []string{"a"}}},
		{"non-hex hash", Token{Name: "t", SHA256: "zzzz", Sites: []string{"a"}}},
		{"wrong-length hash", Token{Name: "t", SHA256: "abcd", Sites: []string{"a"}}},
		{"no sites", Token{Name: "t", SHA256: HashToken("secret")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewAuthenticator([]Token{tt.token}); err == nil {
				t.Errorf("NewAuthenticator(%+v) error = nil, want an error", tt.token)
			}
		})
	}
}

func TestNewAuthenticator_AcceptsUppercaseHash(t *testing.T) {
	// Hashes are often pasted from tools that emit uppercase hex; that
	// must authenticate identically rather than silently never matching.
	tok := Token{Name: "t", SHA256: strings.ToUpper(HashToken("secret")), Sites: []string{"site-a"}}
	auth, err := NewAuthenticator([]Token{tok})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	if _, ok := auth.Authenticate(requestWithAuth("Bearer secret")); !ok {
		t.Error("Authenticate with an uppercase-configured hash failed, want success")
	}
}

func TestAuthenticate_ValidToken(t *testing.T) {
	auth, err := NewAuthenticator([]Token{testToken("panel", "s3cret", "site-a")})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}

	tok, ok := auth.Authenticate(requestWithAuth("Bearer s3cret"))
	if !ok {
		t.Fatal("Authenticate() ok = false, want true")
	}
	if tok.Name != "panel" {
		t.Errorf("matched token Name = %q, want panel", tok.Name)
	}
}

func TestAuthenticate_SchemeIsCaseInsensitiveButCredentialIsNot(t *testing.T) {
	auth, err := NewAuthenticator([]Token{testToken("panel", "s3cret", "site-a")})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}

	if _, ok := auth.Authenticate(requestWithAuth("bearer s3cret")); !ok {
		t.Error("lowercase \"bearer\" scheme rejected, want accepted (RFC 7235 says the scheme is case-insensitive)")
	}
	if _, ok := auth.Authenticate(requestWithAuth("Bearer S3CRET")); ok {
		t.Error("uppercased credential accepted, want rejected (the token itself is case-sensitive)")
	}
}

func TestAuthenticate_RejectsBadCredentials(t *testing.T) {
	auth, err := NewAuthenticator([]Token{testToken("panel", "s3cret", "site-a")})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}

	tests := []struct {
		name   string
		header string
	}{
		{"no header at all", ""},
		{"wrong token", "Bearer wrong"},
		{"empty credential", "Bearer "},
		{"missing scheme", "s3cret"},
		{"wrong scheme", "Basic s3cret"},
		{"token as scheme", "s3cret Bearer"},
		// The stored value is a hash, so presenting the hash itself must
		// not authenticate - otherwise a leaked config would be as good as
		// a leaked token, defeating the point of hashing.
		{"presenting the hash instead of the token", "Bearer " + HashToken("s3cret")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := auth.Authenticate(requestWithAuth(tt.header)); ok {
				t.Errorf("Authenticate(%q) ok = true, want false", tt.header)
			}
		})
	}
}

func TestAuthenticate_PicksTheRightTokenAmongSeveral(t *testing.T) {
	auth, err := NewAuthenticator([]Token{
		testToken("first", "aaa", "site-a"),
		testToken("second", "bbb", "site-b"),
		testToken("third", "ccc", "site-c"),
	})
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}

	for _, tt := range []struct{ raw, wantName, wantSite string }{
		{"aaa", "first", "site-a"},
		{"bbb", "second", "site-b"},
		{"ccc", "third", "site-c"},
	} {
		tok, ok := auth.Authenticate(requestWithAuth("Bearer " + tt.raw))
		if !ok {
			t.Fatalf("Authenticate(%q) ok = false, want true", tt.raw)
		}
		if tok.Name != tt.wantName {
			t.Errorf("Authenticate(%q) matched %q, want %q", tt.raw, tok.Name, tt.wantName)
		}
		if !tok.CanRead(tt.wantSite) {
			t.Errorf("token %q cannot read %q, want it to", tok.Name, tt.wantSite)
		}
	}
}

func TestToken_CanRead(t *testing.T) {
	single := testToken("single", "x", "site-a")
	if !single.CanRead("site-a") {
		t.Error("CanRead(site-a) = false for a token granted site-a")
	}
	if single.CanRead("site-b") {
		t.Error("CanRead(site-b) = true for a token granted only site-a")
	}

	multi := testToken("multi", "x", "site-a", "site-b")
	if !multi.CanRead("site-a") || !multi.CanRead("site-b") {
		t.Error("a multi-site token cannot read one of its own sites")
	}
	if multi.CanRead("site-c") {
		t.Error("a multi-site token can read a site it wasn't granted")
	}

	wildcard := testToken("wildcard", "x", WildcardSite)
	if !wildcard.CanRead("anything-at-all") {
		t.Error("a wildcard token cannot read an arbitrary site, want it to")
	}

	// A site literally named "*" must not be readable by a token that was
	// only granted some other specific site - the wildcard is a grant, not
	// a matchable site name. (config rejects "*" as a site_id anyway, so
	// this is defense in depth on the grant side.)
	if single.CanRead(WildcardSite) {
		t.Error("a single-site token matched the wildcard as if it were a site name")
	}
}

func TestAuthenticator_ConfiguredTokensAreCopied(t *testing.T) {
	// Mutating the caller's slice after construction must not change what
	// the Authenticator accepts.
	tokens := []Token{testToken("panel", "s3cret", "site-a")}
	auth, err := NewAuthenticator(tokens)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}

	tokens[0].SHA256 = HashToken("different")
	tokens[0].Sites = []string{"site-z"}

	tok, ok := auth.Authenticate(requestWithAuth("Bearer s3cret"))
	if !ok {
		t.Fatal("Authenticate() failed after the caller mutated its own slice")
	}
	if !tok.CanRead("site-a") {
		t.Error("token's site grant changed after the caller mutated its own slice")
	}
}
