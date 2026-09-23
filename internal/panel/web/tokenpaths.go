package web

import (
	"path"
	"strings"

	"github.com/cruciblelab/crucible-analytic/internal/logging"
)

// tokenPathPrefixes are the routes whose path is a credential.
//
// A developer link, an owner's claim link and a member invitation all
// work for whoever holds them - an invitation opened but not yet
// accepted still creates the invited account, with a password of the
// holder's choosing, for seven days. The database stores only their
// sha256 for that reason. The log did not: every request was written
// with its path, and measured on the real binary all three tokens were
// in access.log in plain text - and a request that ran into the
// deadline (an invitation page while a schema upgrade held its table)
// put one into panel_logs as well, at WARN, which always reaches it.
//
// internal/logging redacts by attribute name, which is the right
// backstop for a field called token and no help at all for a value
// inside a field called path.
//
// The list is held to the routes Handler registers with a {token...}
// wildcard, both ways, by internal/invariants - a new link route that
// is not here fails a test rather than quietly logging its secret.
var tokenPathPrefixes = []string{DevAccessPathPrefix, ClaimPathPrefix, JoinPathPrefix}

// loggedPath is a request path as a log line may carry it: the route,
// with any credential in it replaced.
//
// Matched on the cleaned path, because the access log sits outside the
// mux and sees the path as sent - "/./katil/<token>" is a request the mux
// redirects to the invitation, and its line would otherwise carry the
// token past a check written for "/katil/".
func loggedPath(p string) string {
	clean := path.Clean(p)
	for _, prefix := range tokenPathPrefixes {
		// path.Clean drops a trailing slash, so the bare "/katil/" is
		// "/katil" here and does not match: a cleaned path that carries
		// the prefix always carries something after it.
		if strings.HasPrefix(clean, prefix) {
			return prefix + logging.Redacted
		}
	}
	return p
}
