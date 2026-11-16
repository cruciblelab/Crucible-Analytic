package storage

import (
	"net/netip"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/asnlookup"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// The score the full proxy signs onto a request (W2) and the score this
// flusher writes for the same address are one rule: the same inputs give
// the same number. Each input is set to one that moves the score - a
// known-bot fingerprint, a rate, and an ASN on the list that SetKnownBotASNs
// published after construction - so a ScoreNow that dropped any of them,
// or read the field instead of the live list, would disagree.
func TestScoreNowIsTheScoreAFlushWrites(t *testing.T) {
	ip := netip.MustParseAddr("51.75.10.10")
	f := &Flusher{
		KnownBots:    map[string]string{"t13d1516h2_8daaf6152771_02713d6af862": "curl"},
		Resolver:     fakeResolver{result: asnlookup.Result{ASN: 16276}},
		KnownBotASNs: map[int]struct{}{},
	}
	f.SetKnownBotASNs(map[int]struct{}{16276: {}})

	for _, c := range []struct {
		name  string
		ja4   string
		stats ratestore.WindowStats
	}{
		{"nothing but the network", "", ratestore.WindowStats{}},
		{"a known fingerprint", "t13d1516h2_8daaf6152771_02713d6af862", ratestore.WindowStats{}},
		{"a rate", "", ratestore.WindowStats{EstimatedRate: 6}},
		{"all three", "t13d1516h2_8daaf6152771_02713d6af862", ratestore.WindowStats{EstimatedRate: 20}},
	} {
		rows := BuildRows([]ratestore.Snapshot{{IP: ip, JA4: c.ja4, WindowStats: c.stats}},
			time.Now(), RowOptions{
				KnownBots:    f.KnownBots,
				KnownBotASNs: f.knownBotASNs(),
				Resolver:     f.Resolver,
			})
		if got, want := f.ScoreNow(ip, c.stats, c.ja4), int(rows[0].BotScore); got != want {
			t.Errorf("%s: ScoreNow = %d, a flush writes %d", c.name, got, want)
		}
		if c.name == "nothing but the network" && f.ScoreNow(ip, c.stats, c.ja4) == 0 {
			t.Errorf("%s: scored 0 - the live ASN list was not read", c.name)
		}
	}
}
