package logging

import "log/slog"

// The standing rule this file exists to make cheap to follow:
//
//	Never trust the client. Only the server's own conclusion counts.
//	It does not matter that the request says 1+1=2 - what matters is
//	what the server worked out. Every request, and every user behind
//	one, is a potential attacker.
//
// The code already works this way: the beacon checks a payload's site
// claim against its configured allowlist rather than believing it,
// X-Forwarded-For is read only when the immediate peer is a configured
// trusted proxy, an API token is compared by hash, and a panel session
// re-reads the user row on every request instead of trusting what the
// cookie said when it was issued.
//
// What was missing is the *record*. When a decision goes wrong - a
// customer behind Cloudflare whose visitors all share one IP, a site id
// nobody can explain, a login refused for a reason the customer
// disputes - the question is always the same: what did the client claim,
// and what did the server conclude? A log line that carries only the
// conclusion cannot answer it, and one that carries only the claim is
// worse than useless because it reads as if the claim were believed.
//
// So trust decisions are logged as both halves, in one place, in their
// own file.

// Trust attribute keys. Constants rather than inline strings so every
// service spells them the same way and the panel can filter on them.
const (
	// KeyClaim is what the client asserted.
	KeyClaim = "claimed"
	// KeyVerdict is what the server concluded.
	KeyVerdict = "verdict"
	// KeyReason explains the verdict.
	KeyReason = "reason"
	// KeySource names where the claim arrived, e.g. "X-Forwarded-For".
	KeySource = "source"
	// KeyPeer is the immediate network peer, which unlike anything in
	// the payload cannot be forged by the client.
	KeyPeer = "peer"
)

// Verdicts. A closed set, so "how many rejections yesterday" is a count
// rather than a text search.
const (
	// VerdictAccepted means the claim was checked and held.
	VerdictAccepted = "accepted"
	// VerdictRejected means the claim was checked and failed.
	VerdictRejected = "rejected"
	// VerdictIgnored means the claim was not believed and a
	// server-derived value was used instead - an untrusted peer's
	// forwarding header, for example.
	VerdictIgnored = "ignored"
	// VerdictThrottled means the request was refused for rate rather
	// than for content.
	VerdictThrottled = "throttled"
)

// Trust builds the attributes for one trust decision.
//
// Usage keeps both halves together by construction, which is the point:
//
//	logger.Warn("site claim refused",
//	    logging.Trust(logging.CategorySecurity, claimedSite,
//	        logging.VerdictRejected, "not in the configured allowlist")...)
//
// The claim is sanitized like any other untrusted string - it is chosen
// by whoever sent the request, and a newline inside it would forge a
// second log line.
func Trust(category Category, claim, verdict, reason string) []any {
	return []any{
		In(category),
		slog.String(KeyClaim, SanitizeValue(claim)),
		slog.String(KeyVerdict, verdict),
		slog.String(KeyReason, SanitizeValue(reason)),
	}
}

// Attempt builds the attributes for one authentication attempt.
//
// Every attempt is logged, successful or not. A file that records only
// failures cannot show that the successful login at 03:00 came from an
// address that had failed forty times an hour earlier, which is the
// shape an actual compromise has.
//
// The identity is whatever the client typed, so it is a claim and is
// labelled as one. No password, hash or token is ever passed here; the
// handler's redaction would catch one, but the rule is not to pass it.
func Attempt(identity, verdict, reason string, peer string) []any {
	return []any{
		In(CategoryAuth),
		slog.String(KeyClaim, SanitizeValue(identity)),
		slog.String(KeyVerdict, verdict),
		slog.String(KeyReason, SanitizeValue(reason)),
		slog.String(KeyPeer, SanitizeValue(peer)),
	}
}

// Rejected builds the attributes for refused input.
//
// Its own category because "the customer says data is missing" is
// answered here and nowhere else: what arrived, and the server's reason
// for not keeping it.
func Rejected(what, reason string) []any {
	return []any{
		In(CategoryRejected),
		slog.String(KeyClaim, SanitizeValue(what)),
		slog.String(KeyVerdict, VerdictRejected),
		slog.String(KeyReason, SanitizeValue(reason)),
	}
}
