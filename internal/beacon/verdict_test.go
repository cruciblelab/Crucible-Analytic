package beacon

import (
	"bytes"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/privacy"
	"github.com/cruciblelab/crucible-analytic/internal/scoring"
	"github.com/cruciblelab/crucible-analytic/internal/verdict"
)

// The address post() sends from, and the time newTestServer's clock reads.
var (
	verdictPeer = netip.MustParseAddr("203.0.113.9")
	verdictNow  = time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC)
)

const pageview = `{"site":"acme","type":"pageview","url":"/"}`

func withVerdict(h string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(verdict.Header, h) }
}

// signedFor is a header the collector in front of this beacon would write.
func signedFor(s *Server, score int, at time.Time, ip netip.Addr) string {
	return verdict.Sign(verdict.Key(s.IPHashKey), score, at, ip)
}

// Each outcome, to the stored reason and to the heartbeat, through the
// handler a request really takes. The score cases sit either side of
// scoring.BotCutoff, asked of the constant.
func TestTheCollectorsVerdictDecidesTheEventsReason(t *testing.T) {
	otherKey := verdict.Key(bytes.Repeat([]byte("b"), privacy.MinHashKeyLen))
	for _, c := range []struct {
		name   string
		header func(s *Server) string
		reason string
		// counter is the heartbeat key that must read 1, all three
		// others reading 0; "" means all three read 0.
		counter string
	}{
		{"no header", func(*Server) string { return "" }, "", ""},
		{"a person, by the collector", func(s *Server) string {
			return signedFor(s, scoring.BotCutoff-1, verdictNow, verdictPeer)
		}, "", ""},
		{"automated, by the collector", func(s *Server) string {
			return signedFor(s, scoring.BotCutoff, verdictNow, verdictPeer)
		}, AutoReasonCollector, ""},
		{"written by the visitor", func(*Server) string {
			return "v1 0 1778051289 203.0.113.9 notamac"
		}, AutoReasonTampered, heartbeat.CounterVerdictTampered},
		{"signed with another key", func(*Server) string {
			return verdict.Sign(otherKey, 0, verdictNow, verdictPeer)
		}, AutoReasonTampered, heartbeat.CounterVerdictTampered},
		{"too old", func(s *Server) string {
			return signedFor(s, 0, verdictNow.Add(-verdict.MaxSkew-time.Second), verdictPeer)
		}, AutoReasonTampered, heartbeat.CounterVerdictTampered},
		{"copied from another visitor", func(s *Server) string {
			return signedFor(s, 0, verdictNow, netip.MustParseAddr("203.0.113.10"))
		}, AutoReasonTampered, heartbeat.CounterVerdictOtherAddress},
	} {
		t.Run(c.name, func(t *testing.T) {
			sink := &fakeSink{}
			s := newTestServer(t, sink)
			var mutate []func(*http.Request)
			if h := c.header(s); h != "" {
				mutate = append(mutate, withVerdict(h))
			}
			if w := post(t, s, pageview, mutate...); w.Code != http.StatusNoContent {
				t.Fatalf("status %d, want 204 - a verdict never refuses an event", w.Code)
			}
			if got := sink.only(t).AutoReason; got != c.reason {
				t.Errorf("auto_reason = %q, want %q", got, c.reason)
			}
			assertVerdictCounters(t, s, c.counter)
		})
	}
}

// A beacon without a key reads no verdict: the event is left to its
// User-Agent - never held against the visitor for a fault of the
// deployment's - and the operator is told the header went unread.
func TestWithoutAKeyAVerdictIsCountedAndNotRead(t *testing.T) {
	sink := &fakeSink{}
	s := newTestServer(t, sink)
	s.IPMode = privacy.IPMasked
	signed := signedFor(s, scoring.MaxScore, verdictNow, verdictPeer)
	s.IPHashKey = nil

	post(t, s, pageview, withVerdict(signed))
	if got := sink.only(t).AutoReason; got != "" {
		t.Errorf("auto_reason = %q with no key to check it, want none", got)
	}
	assertVerdictCounters(t, s, heartbeat.CounterVerdictUnchecked)

	// And nothing at all when no header came: unchecked counts headers,
	// not requests.
	sink2 := &fakeSink{}
	s2 := newTestServer(t, sink2)
	s2.IPHashKey = nil
	s2.IPMode = privacy.IPMasked
	post(t, s2, pageview)
	assertVerdictCounters(t, s2, "")
}

// The verdict is read against the address the server resolved, not the
// peer: behind a trusted proxy, the collector's header holds for the
// visitor named in X-Forwarded-For and not for the proxy.
func TestTheVerdictIsReadAgainstTheResolvedAddress(t *testing.T) {
	visitor := netip.MustParseAddr("198.51.100.77")
	trusting := func(r *http.Request) { r.Header.Set("X-Forwarded-For", visitor.String()) }
	for _, c := range []struct {
		name    string
		signed  netip.Addr
		reason  string
		counter string
	}{
		{"signed for the visitor", visitor, AutoReasonCollector, ""},
		{"signed for the proxy", verdictPeer, AutoReasonTampered, heartbeat.CounterVerdictOtherAddress},
	} {
		t.Run(c.name, func(t *testing.T) {
			sink := &fakeSink{}
			s := newTestServer(t, sink)
			s.ClientIP = ClientIPResolver{TrustedProxies: mustPrefixes(t, verdictPeer.String()+"/32")}
			post(t, s, pageview, trusting, withVerdict(signedFor(s, scoring.MaxScore, verdictNow, c.signed)))
			if got := sink.only(t).AutoReason; got != c.reason {
				t.Errorf("auto_reason = %q, want %q", got, c.reason)
			}
			assertVerdictCounters(t, s, c.counter)
		})
	}
}

func assertVerdictCounters(t *testing.T, s *Server, one string) {
	t.Helper()
	row := HeartbeatCounters(s, fixedWriter{})
	for _, key := range []string{
		heartbeat.CounterVerdictTampered,
		heartbeat.CounterVerdictOtherAddress,
		heartbeat.CounterVerdictUnchecked,
	} {
		want := int64(0)
		if key == one {
			want = 1
		}
		if row[key] != want {
			t.Errorf("%s = %d, want %d", key, row[key], want)
		}
	}
}
