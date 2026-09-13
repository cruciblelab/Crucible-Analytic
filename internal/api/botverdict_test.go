package api

import (
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/scoring"
)

// The weights in internal/scoring and the cutoff in this package are one
// rule written in two places, and until this file existed nothing held
// them together.
//
// What went wrong: scoring documented a known-bot JA4 match as "a
// high-confidence signal, since a JA4 match is a specific fingerprint
// match" and gave it 30 points, while DefaultBotScoreMin was 50. So the
// strongest signal the product has could not, on its own, produce the
// product's own default verdict - whereas the request rate, the weakest
// and most circumstantial of the three, could. Both constants were
// plausible on their own page. Only together were they wrong.
//
// Measured before the change, on a live deployment with the real dataset
// fetched: three non-browser clients each sent a current Chrome
// User-Agent and fetched two pages. Two matched the known-bot set, one
// of them under that dataset's own label "ua_spoof". The summary for
// that window reported bot_ips 0 and human_ips 4.
//
// This test asks each package for its own half rather than restating
// either, so lowering the weight or raising the default breaks it, and
// the failure says which side moved.

// ja4Probe is a fingerprint shaped like the ones the fetched dataset
// carries. Its value is irrelevant - what matters is that it is the only
// signal present when it is scored.
const ja4Probe = "t13d3112h2_e8f1e7e78f70_b26ce05bbdd6"

func TestAKnownBotFingerprintAloneReachesTheDefaultCutoff(t *testing.T) {
	// No rate, no ASN: a scraper that fetches politely, which is what a
	// scraper trying not to be noticed does.
	r := scoring.Score(0, ja4Probe, scoring.KnownBots{ja4Probe: "ua_spoof"}, 0, nil)

	if !r.IsKnownBotJA4 {
		t.Fatalf("the probe fingerprint did not match its own known-bot set: %+v", r)
	}
	if r.Score < DefaultBotScoreMin {
		t.Errorf("a confirmed known-bot fingerprint scores %d, below the default cutoff %d - "+
			"so the strongest signal this product has cannot on its own make it say \"bot\"",
			r.Score, DefaultBotScoreMin)
	}
}

func TestAKnownBotASNAloneStaysBelowTheDefaultCutoff(t *testing.T) {
	// The other half of the same rule, and it has to be here: a test that
	// only required JA4 to clear the cutoff would also pass with every
	// signal weighted at 100, which would make "circumstantial" and
	// "specific" mean the same thing. An ASN match is documented as a
	// nudge, so it must still be one.
	r := scoring.Score(0, "", nil, 64512, map[int]struct{}{64512: {}})

	if !r.IsKnownBotASN {
		t.Fatalf("the probe ASN did not match its own known-bot set: %+v", r)
	}
	if r.Score >= DefaultBotScoreMin {
		t.Errorf("an ASN match alone scores %d, at or above the default cutoff %d - "+
			"an ASN is circumstantial (cloud, VPN, CI) and must not decide by itself",
			r.Score, DefaultBotScoreMin)
	}
}
