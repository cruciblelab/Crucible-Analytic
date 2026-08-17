package scoring

import "testing"

func TestScore_ZeroActivity(t *testing.T) {
	r := Score(0, "unknown-ja4", testKnownBots, 0, nil)
	if r.Score != 0 || r.RateScore != 0 || r.JA4Score != 0 || r.IsKnownBotJA4 || r.ASNScore != 0 || r.IsKnownBotASN {
		t.Errorf("Score(0, unknown) = %+v, want all zero", r)
	}
}

func TestScore_RateSaturates(t *testing.T) {
	atSaturation := Score(rateScoreSaturationRPS, "unknown", nil, 0, nil)
	if atSaturation.RateScore != maxRateScore {
		t.Errorf("RateScore at saturation = %d, want %d", atSaturation.RateScore, maxRateScore)
	}

	wayAbove := Score(rateScoreSaturationRPS*10, "unknown", nil, 0, nil)
	if wayAbove.RateScore != maxRateScore {
		t.Errorf("RateScore above saturation = %d, want %d (capped)", wayAbove.RateScore, maxRateScore)
	}
}

func TestScore_RateScalesLinearly(t *testing.T) {
	half := Score(rateScoreSaturationRPS/2, "unknown", nil, 0, nil)
	if want := maxRateScore / 2; half.RateScore != want {
		t.Errorf("RateScore at half saturation = %d, want %d", half.RateScore, want)
	}
}

func TestScore_KnownBotJA4AddsFlatBonus(t *testing.T) {
	knownBots := map[string]string{"bad-ja4": "test-bot"}

	r := Score(0, "bad-ja4", knownBots, 0, nil)
	if !r.IsKnownBotJA4 {
		t.Error("IsKnownBotJA4 = false, want true")
	}
	if r.JA4Score != maxJA4Score {
		t.Errorf("JA4Score = %d, want %d", r.JA4Score, maxJA4Score)
	}
	if r.Score != maxJA4Score {
		t.Errorf("Score = %d, want %d", r.Score, maxJA4Score)
	}
}

func TestScore_UnknownJA4NoBonus(t *testing.T) {
	knownBots := map[string]string{"bad-ja4": "test-bot"}
	r := Score(0, "some-other-ja4", knownBots, 0, nil)
	if r.IsKnownBotJA4 || r.JA4Score != 0 {
		t.Errorf("Score(unknown ja4) = %+v, want no JA4 bonus", r)
	}
}

func TestScore_EmptyJA4NeverMatches(t *testing.T) {
	// "" is the sentinel for non-TLS/unparseable traffic; it must never
	// be treated as a known-bot match even against an attacker-controlled
	// knownBots map (defense in depth for the invariant KnownBotJA4 also
	// documents).
	knownBots := map[string]string{"": "should-not-match"}
	r := Score(0, "", knownBots, 0, nil)
	if r.IsKnownBotJA4 {
		t.Error("empty JA4 matched a known-bot entry; the \"\" sentinel must never match")
	}
}

func TestScore_KnownBotASNAddsFlatBonus(t *testing.T) {
	knownBotASNs := map[int]struct{}{64512: {}}

	r := Score(0, "", nil, 64512, knownBotASNs)
	if !r.IsKnownBotASN {
		t.Error("IsKnownBotASN = false, want true")
	}
	if r.ASNScore != maxASNScore {
		t.Errorf("ASNScore = %d, want %d", r.ASNScore, maxASNScore)
	}
	if r.Score != maxASNScore {
		t.Errorf("Score = %d, want %d", r.Score, maxASNScore)
	}
}

func TestScore_UnknownASNNoBonus(t *testing.T) {
	knownBotASNs := map[int]struct{}{64512: {}}
	r := Score(0, "", nil, 64513, knownBotASNs)
	if r.IsKnownBotASN || r.ASNScore != 0 {
		t.Errorf("Score(unknown asn) = %+v, want no ASN bonus", r)
	}
}

func TestScore_ZeroASNNeverMatches(t *testing.T) {
	// asn == 0 is asnlookup's own "not resolved" sentinel; it must never
	// be treated as a known-bot match even against an attacker-controlled
	// (or misconfigured) knownBotASNs map, mirroring TestScore_EmptyJA4NeverMatches.
	knownBotASNs := map[int]struct{}{0: {}}
	r := Score(0, "", nil, 0, knownBotASNs)
	if r.IsKnownBotASN {
		t.Error("ASN 0 matched a known-bot entry; the 0 sentinel must never match")
	}
}

func TestScore_NilKnownBotASNsNoBonus(t *testing.T) {
	// The shape asn_lookup.apply_to_scoring = false (the default) leaves
	// this in - a nil map must behave as a complete no-op, not panic on a
	// lookup into it.
	r := Score(0, "", nil, 64512, nil)
	if r.IsKnownBotASN || r.ASNScore != 0 {
		t.Errorf("Score(asn, nil knownBotASNs) = %+v, want no ASN bonus", r)
	}
}

func TestScore_RateJA4AndASNCombine(t *testing.T) {
	knownBots := map[string]string{"bad-ja4": "test-bot"}
	knownBotASNs := map[int]struct{}{64512: {}}

	r := Score(rateScoreSaturationRPS/2, "bad-ja4", knownBots, 64512, knownBotASNs)
	want := maxRateScore/2 + maxJA4Score + maxASNScore
	if r.Score != want {
		t.Errorf("Score = %d, want %d (RateScore %d + JA4Score %d + ASNScore %d)", r.Score, want, r.RateScore, r.JA4Score, r.ASNScore)
	}
}

func TestScore_NeverExceedsMaxScore(t *testing.T) {
	knownBots := map[string]string{"bad-ja4": "test-bot"}
	knownBotASNs := map[int]struct{}{64512: {}}
	r := Score(rateScoreSaturationRPS*1000, "bad-ja4", knownBots, 64512, knownBotASNs)
	if r.Score > MaxScore {
		t.Errorf("Score = %d, exceeds MaxScore %d", r.Score, MaxScore)
	}
	if r.Score != MaxScore {
		t.Errorf("Score = %d, want exactly %d for max rate + known bot JA4 + known bot ASN", r.Score, MaxScore)
	}
}

func TestScore_NegativeRateDoesNotGoNegative(t *testing.T) {
	r := Score(-5, "unknown", nil, 0, nil)
	if r.RateScore != 0 || r.Score != 0 {
		t.Errorf("Score(negative rate) = %+v, want zero", r)
	}
}

// testKnownBots stands in for a fetched set. The real one is not
// compiled into this repository - see internal/botdata for why - so the
// tests supply their own rather than depending on data that may not be
// on the machine running them.
var testKnownBots = KnownBots{
	"t13d1516h2_8daaf6152771_b186095e22b6": "curl",
}
