package scoring

import "testing"

func TestScore_ZeroActivity(t *testing.T) {
	r := Score(0, "unknown-ja4", KnownBotJA4)
	if r.Score != 0 || r.RateScore != 0 || r.JA4Score != 0 || r.IsKnownBotJA4 {
		t.Errorf("Score(0, unknown) = %+v, want all zero", r)
	}
}

func TestScore_RateSaturates(t *testing.T) {
	atSaturation := Score(rateScoreSaturationRPS, "unknown", nil)
	if atSaturation.RateScore != maxRateScore {
		t.Errorf("RateScore at saturation = %d, want %d", atSaturation.RateScore, maxRateScore)
	}

	wayAbove := Score(rateScoreSaturationRPS*10, "unknown", nil)
	if wayAbove.RateScore != maxRateScore {
		t.Errorf("RateScore above saturation = %d, want %d (capped)", wayAbove.RateScore, maxRateScore)
	}
}

func TestScore_RateScalesLinearly(t *testing.T) {
	half := Score(rateScoreSaturationRPS/2, "unknown", nil)
	if want := maxRateScore / 2; half.RateScore != want {
		t.Errorf("RateScore at half saturation = %d, want %d", half.RateScore, want)
	}
}

func TestScore_KnownBotJA4AddsFlatBonus(t *testing.T) {
	knownBots := map[string]string{"bad-ja4": "test-bot"}

	r := Score(0, "bad-ja4", knownBots)
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
	r := Score(0, "some-other-ja4", knownBots)
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
	r := Score(0, "", knownBots)
	if r.IsKnownBotJA4 {
		t.Error("empty JA4 matched a known-bot entry; the \"\" sentinel must never match")
	}
}

func TestScore_NeverExceedsMaxScore(t *testing.T) {
	knownBots := map[string]string{"bad-ja4": "test-bot"}
	r := Score(rateScoreSaturationRPS*1000, "bad-ja4", knownBots)
	if r.Score > MaxScore {
		t.Errorf("Score = %d, exceeds MaxScore %d", r.Score, MaxScore)
	}
	if r.Score != MaxScore {
		t.Errorf("Score = %d, want exactly %d for max rate + known bot", r.Score, MaxScore)
	}
}

func TestScore_NegativeRateDoesNotGoNegative(t *testing.T) {
	r := Score(-5, "unknown", nil)
	if r.RateScore != 0 || r.Score != 0 {
		t.Errorf("Score(negative rate) = %+v, want zero", r)
	}
}

func TestKnownBotJA4_NoEmptyKey(t *testing.T) {
	if _, ok := KnownBotJA4[""]; ok {
		t.Error("KnownBotJA4 must not contain an empty-string key (would match all non-TLS/unparsed traffic)")
	}
}
