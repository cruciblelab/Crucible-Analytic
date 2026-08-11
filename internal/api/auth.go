package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// WildcardSite in a Token's Sites list grants read access to every site
// in the database, rather than an enumerated few. Intended for the
// operator's own management panel, which needs to see everything on a
// server; a token handed to anyone else should always enumerate its
// sites explicitly instead.
const WildcardSite = "*"

// Token is one configured API credential. The token string itself is
// never stored - only its SHA-256 hash - so a leaked config file doesn't
// hand over working credentials, the same reasoning behind storing
// password hashes rather than passwords.
type Token struct {
	// Name is a human label used in logs to identify which credential was
	// used, without logging the credential. Not a secret and not checked
	// during authentication.
	Name string
	// SHA256 is the hex-encoded SHA-256 hash of the bearer token.
	SHA256 string
	// Sites lists the site IDs this token may read, or exactly
	// []string{WildcardSite} for all of them.
	Sites []string
}

// CanRead reports whether this token is allowed to read siteID.
func (t Token) CanRead(siteID string) bool {
	for _, s := range t.Sites {
		if s == WildcardSite || s == siteID {
			return true
		}
	}
	return false
}

// Authenticator validates bearer tokens against a fixed, configured set.
// Safe for concurrent use: it's read-only after construction.
type Authenticator struct {
	tokens []Token
}

// NewAuthenticator validates the configured tokens and returns an
// Authenticator over them. An empty token list is rejected rather than
// treated as "allow everything" - a config that forgot its tokens should
// fail loudly at startup, not silently serve every site to anyone.
func NewAuthenticator(tokens []Token) (*Authenticator, error) {
	if len(tokens) == 0 {
		return nil, fmt.Errorf("api: at least one token is required")
	}
	for i, t := range tokens {
		if t.Name == "" {
			return nil, fmt.Errorf("api: token %d has no name", i)
		}
		raw, err := hex.DecodeString(t.SHA256)
		if err != nil {
			return nil, fmt.Errorf("api: token %q has a non-hex sha256: %w", t.Name, err)
		}
		if len(raw) != sha256.Size {
			return nil, fmt.Errorf("api: token %q has a %d-byte sha256, want %d (is it really a SHA-256 hash?)", t.Name, len(raw), sha256.Size)
		}
		if len(t.Sites) == 0 {
			return nil, fmt.Errorf("api: token %q grants no sites (use [%q] for all)", t.Name, WildcardSite)
		}
	}

	// Copy so a caller mutating its slice afterwards can't change what
	// this Authenticator will accept.
	compiled := make([]Token, len(tokens))
	copy(compiled, tokens)
	for i := range compiled {
		compiled[i].SHA256 = strings.ToLower(compiled[i].SHA256)
	}
	return &Authenticator{tokens: compiled}, nil
}

// Authenticate extracts an `Authorization: Bearer <token>` credential
// from r and matches it against the configured set, returning the matched
// Token and true on success.
//
// The presented token is hashed and then compared against every
// configured hash with a constant-time comparison, rather than looked up
// in a map keyed by hash. Hashing first already defeats byte-by-byte
// guessing (a one-character change to the token changes the whole hash),
// so the constant-time compare is belt-and-braces - but the token count
// here is the number of sites on one server, so scanning all of them
// costs nothing measurable and removes the need to reason about map
// lookup timing at all.
func (a *Authenticator) Authenticate(r *http.Request) (Token, bool) {
	presented, ok := bearerToken(r)
	if !ok {
		return Token{}, false
	}

	sum := sha256.Sum256([]byte(presented))
	presentedHash := hex.EncodeToString(sum[:])

	var matched Token
	var found bool
	for _, t := range a.tokens {
		if subtle.ConstantTimeCompare([]byte(presentedHash), []byte(t.SHA256)) == 1 {
			matched, found = t, true
			// Deliberately no early break: returning as soon as a match is
			// found would make a request with a valid token measurably
			// faster than one with an invalid token in proportion to where
			// it sits in the list. Scanning the whole list every time keeps
			// the work constant regardless of which token was presented.
		}
	}
	return matched, found
}

// bearerToken pulls the credential out of an Authorization header,
// requiring the "Bearer " scheme. The scheme name is matched
// case-insensitively (RFC 7235 says it's case-insensitive) but the
// credential itself is taken verbatim.
func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	scheme, credential, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	credential = strings.TrimSpace(credential)
	if credential == "" {
		return "", false
	}
	return credential, true
}

// HashToken returns the hex-encoded SHA-256 of a raw token, the form
// Token.SHA256 expects. Exported so the operator-facing token-generation
// path (see cmd/analytics-api's -hash-token flag) and the authentication
// path above can't drift apart in how they hash.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}
