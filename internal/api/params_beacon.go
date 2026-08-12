package api

import (
	"fmt"
	"net/url"
	"time"
)

// BotFilter selects which population a beacon query counts.
//
// It exists because there is no honest single answer. A customer asking
// "how many pageviews" means human ones, and a number that quietly
// included crawlers would be wrong for them. But this project's whole
// argument is that automated traffic should be visible rather than
// silently discarded, so filtering it away by default without saying so
// would be exactly the behavior it criticizes in conventional tools.
//
// The resolution is an explicit, named parameter with a documented
// default: nothing is hidden, and the default still matches what a panel
// would want to show without passing anything.
type BotFilter string

const (
	// BotsExclude drops events whose user agent self-identifies as a
	// bot. The default.
	BotsExclude BotFilter = "exclude"
	// BotsInclude counts everything.
	BotsInclude BotFilter = "include"
	// BotsOnly counts *only* self-identified bots - clients that ran
	// JavaScript and admitted to being automated.
	BotsOnly BotFilter = "only"
)

// DefaultBotFilter is what a request that doesn't ask gets.
const DefaultBotFilter = BotsExclude

// ParseBotFilter reads the `bots` query parameter.
func ParseBotFilter(q url.Values) (BotFilter, error) {
	switch raw := q.Get("bots"); BotFilter(raw) {
	case "":
		return DefaultBotFilter, nil
	case BotsExclude:
		return BotsExclude, nil
	case BotsInclude:
		return BotsInclude, nil
	case BotsOnly:
		return BotsOnly, nil
	default:
		return "", fmt.Errorf("invalid bots %q (want exclude, include or only)", raw)
	}
}

// rejectBotScoreMin reports an error if a request to a beacon route
// carries bot_score_min.
//
// Ignoring it would be worse than failing. bot_score_min is the
// collector's 0-100 behavioral score, which lives in traffic_snapshots
// and has no counterpart on a beacon event - so a panel that sent it
// here expecting a filtered number would silently receive an unfiltered
// one, and quietly wrong numbers are the failure mode this project has
// already paid for once (see Summary.PeakWindowRequests). Failing loudly
// with a pointer to the two parameters that *do* work is the honest
// answer.
func rejectBotScoreMin(q url.Values) error {
	if q.Get("bot_score_min") == "" {
		return nil
	}
	return fmt.Errorf("bot_score_min does not apply to beacon endpoints: it is the collector's behavioral score and beacon events carry no such column. Use bots=exclude|include|only to filter by self-identified bot user agent, or the /crossover/ endpoints to work with the collector's score")
}

// beaconParams is the parameter set the beacon list endpoints share.
type beaconParams struct {
	from, to time.Time
	limit    int
	offset   int
	bots     BotFilter
}

// envelope wraps a page of beacon results, mirroring listParams.envelope
// but echoing the bots filter that produced the numbers instead of a bot
// score threshold - so a response always carries enough to explain
// itself.
func (p beaconParams) envelope(site, key string, rows any, total int) map[string]any {
	return map[string]any{
		"site_id": site,
		"from":    p.from,
		"to":      p.to,
		"limit":   p.limit,
		"offset":  p.offset,
		"total":   total,
		"bots":    string(p.bots),
		key:       rows,
	}
}
