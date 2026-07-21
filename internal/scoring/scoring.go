// Package scoring computes a simple, cheap bot-likelihood score for the
// MVP: a request-rate component derived from the RateStore's sliding
// window, plus a flat bonus if the connection's JA4 fingerprint matches a
// known-bot signature. Anything more elaborate (header consistency,
// path-scanning, weighted multi-signal correlation) is explicitly out of
// scope for this phase.
package scoring

const (
	// MaxScore is the ceiling of the combined score.
	MaxScore = 100

	// maxRateScore is the most points the request-rate component can
	// contribute.
	maxRateScore = 70
	// maxJA4Score is the flat bonus applied when the JA4 fingerprint
	// matches a known-bot signature.
	maxJA4Score = 30

	// rateScoreSaturationRPS is the estimated requests/second (from the
	// sliding window) at or above which the rate component alone hits
	// maxRateScore; below it, the score scales down linearly to 0. This is
	// a heuristic starting point, not a tuned threshold - revisit once
	// real traffic data is available.
	rateScoreSaturationRPS = 15.0
)

// KnownBotJA4 is a small, hardcoded starter list mapping known JA4
// fingerprints to a human-readable label. It exists to demonstrate the
// scoring mechanism end-to-end - the entries below are illustrative
// placeholders, NOT a verified threat-intel feed. A later phase should
// replace/extend this with real, maintained fingerprint data (see
// https://github.com/FoxIO-LLC/ja4 and https://ja4db.com for public
// references on associating JA4 values with known clients/tools).
//
// Must never contain "" as a key: that's the sentinel RecordRequest/Score
// callers use for "no JA4 available" (non-TLS or unparseable traffic), and
// an empty-string entry here would flag all of that traffic as a known bot.
var KnownBotJA4 = map[string]string{
	"t13i000200_aaaaaaaaaaaa_aaaaaaaaaaaa": "example-placeholder-scanner",
	"t11i000100_bbbbbbbbbbbb_bbbbbbbbbbbb": "example-placeholder-legacy-bot",
}

// Result is the outcome of scoring one IP's current activity.
type Result struct {
	Score         int
	RateScore     int
	JA4Score      int
	IsKnownBotJA4 bool
}

// Score combines the estimated request rate (requests/second, from the
// sliding window) and JA4 fingerprint into a single 0-100 bot-likelihood
// score. ja4 may be empty (non-TLS or unparseable traffic), in which case
// the JA4 component simply contributes nothing.
func Score(estimatedRPS float64, ja4 string, knownBots map[string]string) Result {
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

	total := rateScore + ja4Score
	if total > MaxScore {
		total = MaxScore
	}

	return Result{
		Score:         total,
		RateScore:     rateScore,
		JA4Score:      ja4Score,
		IsKnownBotJA4: isKnownBot,
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
