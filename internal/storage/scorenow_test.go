package storage

import (
	"net/netip"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/asnlookup"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// The score the full proxy signs onto a request (W2) and the score this
// flusher writes for the same address are one rule: with no rate, the same
// inputs give the same number. Each input is set to one that moves the
// score - a known-bot fingerprint and an ASN on the list that
// SetKnownBotASNs published after construction - so an IdentityScore that
// dropped either, or read the field instead of the live list, would
// disagree.
//
// And with a rate the flush's score grows and the identity score does not:
// that is the difference W2b made, held from both sides.
func TestIdentityScoreIsTheFlushsScoreWithoutTheRate(t *testing.T) {
	ip := netip.MustParseAddr("51.75.10.10")
	f := &Flusher{
		KnownBots:    map[string]string{"t13d1516h2_8daaf6152771_02713d6af862": "curl"},
		Resolver:     fakeResolver{result: asnlookup.Result{ASN: 16276}},
		KnownBotASNs: map[int]struct{}{},
	}
	f.SetKnownBotASNs(map[int]struct{}{16276: {}})
	written := func(ja4 string, rate float64) int {
		rows := BuildRows([]ratestore.Snapshot{{IP: ip, JA4: ja4, WindowStats: ratestore.WindowStats{EstimatedRate: rate}}},
			time.Now(), RowOptions{KnownBots: f.KnownBots, KnownBotASNs: f.knownBotASNs(), Resolver: f.Resolver})
		return int(rows[0].BotScore)
	}

	for _, c := range []struct {
		name string
		ja4  string
		want int
	}{
		{"nothing but the network", "", 20},
		{"a known fingerprint on that network", "t13d1516h2_8daaf6152771_02713d6af862", 70},
	} {
		got := f.IdentityScore(ip, c.ja4)
		if got != c.want {
			t.Errorf("%s: IdentityScore = %d, want %d", c.name, got, c.want)
		}
		if w := written(c.ja4, 0); got != w {
			t.Errorf("%s: IdentityScore = %d, a flush with no rate writes %d", c.name, got, w)
		}
		if w := written(c.ja4, 20); w <= got {
			t.Errorf("%s: a flush at 20 req/s writes %d, not above the identity score %d - the rate was never there to leave out", c.name, w, got)
		}
	}
}
