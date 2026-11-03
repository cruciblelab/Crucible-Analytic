package web

import (
	"log/slog"
	"net/http"
	"net/netip"

	"github.com/cruciblelab/crucible-analytic/internal/clientip"
	"github.com/cruciblelab/crucible-analytic/internal/logging"
)

// clientAddr is the address a request is attributed to: what the sign-in
// limits count by, and what the audit records say a link, an invitation
// or a developer password was used from.
//
// The peer's own address, unless the peer is a proxy panel.toml names in
// trusted_proxies; then the rightmost X-Forwarded-For hop no trusted
// proxy vouches for (internal/clientip, the beacon's rule). Never a
// header on its own word: a value the person making the request can set
// is not an answer to "where was this from", and a sign-in limit keyed
// on one would be a limit the attacker chooses.
//
// # Why not just the peer (H12)
//
// It used to be the peer, always, and the comment said "if the panel
// sits behind a proxy this records the proxy, which is at least true".
// True, and for the audit record merely useless; for the sign-in limits
// it was a lock anybody could turn. KURULUM section 8 puts nginx in
// front, so every request came from 127.0.0.1, and maxFailuresPerIP is
// a budget for the whole internet: on the live rig thirty wrong
// passwords for addresses that do not exist, from one machine, and both
// owners - from Turkey and from Germany, with their right passwords -
// were told to wait fifteen minutes. Two requests a minute keep it shut.
func (s *Server) clientAddr(r *http.Request) netip.Addr {
	addr, ok := s.ClientIP.ClientIP(r)
	if !ok {
		return netip.Addr{}
	}
	s.warnUntrustedForward(r)
	return addr
}

// warnUntrustedForward says once, at WARN, that a forwarding header came
// from a peer trusted_proxies does not name.
//
// An install made before trusted_proxies existed, behind the proxy
// KURULUM recommends, is exactly that deployment, and nothing else would
// tell its operator: the panel works, the lock only shows when somebody
// leans on it. Once per process, because a directly exposed panel can be
// sent the header by anybody and a line per request would be theirs to
// write.
func (s *Server) warnUntrustedForward(r *http.Request) {
	claim := r.Header.Get("X-Forwarded-For")
	if claim == "" {
		claim = r.Header.Get("X-Real-IP")
	}
	if claim == "" {
		return
	}
	peer, ok := clientip.Peer(r.RemoteAddr)
	if !ok || s.ClientIP.Trusts(peer) {
		return
	}
	if !s.warnedForward.CompareAndSwap(false, true) {
		return
	}
	s.logger().Warn("panel: a forwarding header arrived from a peer trusted_proxies does not name; "+
		"sign-in limits count every request from it as one address",
		append(logging.Trust(logging.CategorySecurity, claim, logging.VerdictIgnored,
			"the immediate peer is not in panel.toml trusted_proxies"),
			slog.String(logging.KeyPeer, peer.String()),
			// The address stays under KeyPeer, which the diagnostic file
			// masks: on a directly exposed panel the peer is a visitor.
			slog.String("how", "if that peer is your reverse proxy, name it in panel.toml trusted_proxies"))...)
}
