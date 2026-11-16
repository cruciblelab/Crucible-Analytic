package beacon

import (
	"net/http"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/scoring"
	"github.com/cruciblelab/crucible-analytic/internal/verdict"
)

// Why an event is counted as automated, beyond its User-Agent. Written to
// beacon_events.auto_reason; NULL when nothing said so (W2).
//
// The column is a reason rather than a flag so the dashboard, and anyone
// reading the table, can tell a verdict the collector signed from a header
// that failed to check out. Both are excluded from the counts; only one of
// them is the collector's opinion of the visitor.
const (
	// AutoReasonCollector: the collector in front of the site gave this
	// request's fingerprint and network an identity score at or above
	// scoring.BotCutoff and signed that.
	AutoReasonCollector = "collector"
	// AutoReasonTampered: a verdict header was present and did not hold -
	// malformed, signed with another key, too old, or signed for another
	// address. Nobody but the collector writes a header that holds, so a
	// request carrying one that doesn't is not a browser's.
	AutoReasonTampered = "tampered"
	// AutoReasonRate: the address sent events faster than the mark
	// thresholds of Server.EventRate (W2b).
	AutoReasonRate = "rate"
)

// storedAutoReason is the auto_reason column's value: NULL for none.
func (r Row) storedAutoReason() any {
	if r.AutoReason == "" {
		return nil
	}
	return r.AutoReason
}

// verdictCounts are the checks that went wrong, by kind, for the
// heartbeat. A verdict that held is not counted here: how many visitors
// the collector judged automated is a number about visitors, and the
// Health page carries none.
type verdictCounts struct {
	// tampered: Malformed, BadSignature or Stale.
	tampered atomic.Uint64
	// otherAddress: the MAC held for an address other than the one this
	// server resolved. Kept apart because its likeliest cause is not an
	// attacker but this beacon's trusted_proxies resolving visitors'
	// addresses differently from the collector - a deployment fault the
	// operator can fix once named.
	otherAddress atomic.Uint64
	// unchecked: a header arrived and this beacon has no key to check it
	// with. The collector has one (full mode requires it), so the two
	// services are configured with different privacy.ip_hash_key values,
	// or this one with none.
	unchecked atomic.Uint64
}

// verdictKey is the key the collector signs with, derived once.
func (s *Server) verdictKey() []byte {
	s.verdictKeyOnce.Do(func() { s.verdictKeyBytes = verdict.Key(s.IPHashKey) })
	return s.verdictKeyBytes
}

// judgeVerdict reads the collector's header on r, for a request from ip,
// and returns the auto_reason it implies.
//
// No key, no verdict: a header this server cannot check says nothing
// either way, so the event is left to the User-Agent and is counted under
// unchecked for the operator. That is the one case where a present header
// is not held against the request - the fault is the deployment's, and
// excluding every visitor for it would empty the dashboard.
func (s *Server) judgeVerdict(r *http.Request, ip netip.Addr, now time.Time) string {
	header := r.Header.Get(verdict.Header)
	key := s.verdictKey()
	if key == nil {
		if header != "" {
			s.verdicts.unchecked.Add(1)
		}
		return ""
	}
	score, outcome := verdict.Check(key, header, ip, now)
	switch outcome {
	case verdict.Absent:
		return ""
	case verdict.Verified:
		if score >= scoring.BotCutoff {
			return AutoReasonCollector
		}
		return ""
	case verdict.OtherAddress:
		s.verdicts.otherAddress.Add(1)
		return AutoReasonTampered
	default: // Malformed, BadSignature, Stale
		s.verdicts.tampered.Add(1)
		return AutoReasonTampered
	}
}
