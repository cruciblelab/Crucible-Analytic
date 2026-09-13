// Package scoring computes a simple, cheap bot-likelihood score for the
// MVP: a request-rate component derived from the RateStore's sliding
// window, a flat bonus if the connection's JA4 fingerprint matches a
// known-bot signature, and an optional flat bonus if its ASN matches an
// operator-configured list (asn_lookup.known_bot_asns, off unless
// asn_lookup.apply_to_scoring = true - see the README). Anything more
// elaborate (header consistency, path-scanning, weighted multi-signal
// correlation) is explicitly out of scope for this phase.
package scoring

const (
	// MaxScore is the ceiling of the combined score.
	MaxScore = 100

	// maxRateScore is the most points the request-rate component can
	// contribute.
	maxRateScore = 70
	// maxJA4Score is the flat bonus applied when the JA4 fingerprint
	// matches a known-bot signature - a high-confidence signal, since a
	// JA4 match is a specific fingerprint match.
	//
	// # Why it is exactly api.DefaultBotScoreMin and not less
	//
	// It was 30 against a default cutoff of 50, which meant the strongest
	// signal this package has could never, on its own, produce the
	// verdict the product's own default draws. Measured on a live
	// deployment rather than argued: three non-browser clients, all
	// sending a current Chrome User-Agent, fetched two pages each. Two of
	// the three matched the fetched known-bot set - one of them under the
	// dataset's own label "ua_spoof" - and the API's summary for that
	// window reported bot_ips 0, human_ips 4. The product had caught them
	// and then counted them as people, because 30 < 50.
	//
	// A rate burst alone could cross the cutoff; a confirmed fingerprint
	// alone could not. That inversion is what changed, and nothing else:
	// the score is still only ever read (nothing in this project blocks
	// on it), so the effect is on what the panel counts, never on who is
	// served.
	//
	// The relationship is the point, not the number. It is held by
	// api.TestAKnownBotFingerprintAloneReachesTheDefaultCutoff, which
	// asks this package for a score and internal/api for its cutoff
	// rather than restating either.
	maxJA4Score = 50
	// maxASNScore is the flat bonus applied when the ASN matches
	// known_bot_asns. Weighted below maxJA4Score deliberately: an ASN
	// match is a weaker, more circumstantial signal than a JA4 fingerprint
	// match - plenty of legitimate traffic also originates from cloud/
	// hosting ASNs (corporate VPNs, CI runners, etc.), so this nudges the
	// score rather than asserting a specific match the way JA4 does.
	//
	// "Nudges rather than asserts" is a claim about a number, so it is
	// tested as one: the same test that requires a JA4 match to reach the
	// default cutoff requires an ASN match on its own to fall short of
	// it. One half of that rule without the other would let both weights
	// drift to the same meaning.
	maxASNScore = 20

	// rateScoreSaturationRPS is the estimated requests/second (from the
	// sliding window) at or above which the rate component alone hits
	// maxRateScore; below it, the score scales down linearly to 0. This is
	// a heuristic starting point, not a tuned threshold - revisit once
	// real traffic data is available.
	rateScoreSaturationRPS = 15.0
)

// Result is the outcome of scoring one IP's current activity.
type Result struct {
	Score         int
	RateScore     int
	JA4Score      int
	IsKnownBotJA4 bool
	ASNScore      int
	IsKnownBotASN bool
}

// Score combines the estimated request rate (requests/second, from the
// sliding window), JA4 fingerprint, and ASN into a single 0-100
// bot-likelihood score (RateScore + JA4Score + ASNScore, capped at
// MaxScore). ja4 may be empty (non-TLS or unparseable traffic) and asn may
// be 0 (asn_lookup disabled, or the IP wasn't found in the ASN dataset) -
// both simply contribute nothing in that case. knownBotASNs is nil
// whenever asn_lookup.apply_to_scoring = false (the default), which - like
// asn == 0 - means the ASN component always contributes 0: this function
// doesn't need its own on/off switch, since an empty/nil knownBotASNs
// already behaves as a complete no-op.
func Score(estimatedRPS float64, ja4 string, knownBots map[string]string, asn int, knownBotASNs map[int]struct{}) Result {
	rateScore := rateComponent(estimatedRPS)

	// ja4 == "" is the sentinel for non-TLS/unparseable traffic. Guard it
	// explicitly rather than trusting knownBots to never contain an
	// empty-string key - that map may later be loaded from an external
	// file, and a bad entry there would otherwise flag all such traffic
	// as a known bot.
	var isKnownBot bool
	if ja4 != "" {
		_, isKnownBot = knownBots[ja4]
	}
	ja4Score := 0
	if isKnownBot {
		ja4Score = maxJA4Score
	}

	// asn == 0 is asnlookup's own "not resolved" sentinel (see
	// asnlookup.Result) - guarded the same way ja4 == "" is above, so a
	// knownBotASNs entry of 0 (which config validation already rejects,
	// but defense in depth costs nothing here) could never match every
	// unresolved lookup.
	var isKnownBotASN bool
	if asn != 0 {
		_, isKnownBotASN = knownBotASNs[asn]
	}
	asnScore := 0
	if isKnownBotASN {
		asnScore = maxASNScore
	}

	total := rateScore + ja4Score + asnScore
	if total > MaxScore {
		total = MaxScore
	}

	return Result{
		Score:         total,
		RateScore:     rateScore,
		JA4Score:      ja4Score,
		IsKnownBotJA4: isKnownBot,
		ASNScore:      asnScore,
		IsKnownBotASN: isKnownBotASN,
	}
}

func rateComponent(rps float64) int {
	if rps <= 0 {
		return 0
	}
	score := int((rps / rateScoreSaturationRPS) * maxRateScore)
	if score > maxRateScore {
		score = maxRateScore
	}
	return score
}
