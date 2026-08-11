package api

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// allowedIntervals restricts time_bucket's interval to a fixed set.
// Two reasons, both real: the value reaches SQL (as a bound parameter cast
// to ::interval, so injection isn't the concern), and an unrestricted
// interval lets one request ask for millions of buckets - '1 second' over
// a year - which is a trivial way to exhaust memory on both sides.
var allowedIntervals = map[string]bool{
	"1 minute":   true,
	"5 minutes":  true,
	"15 minutes": true,
	"1 hour":     true,
	"6 hours":    true,
	"1 day":      true,
	"1 week":     true,
}

// DefaultInterval is the bucket size used when a request doesn't ask for
// one.
const DefaultInterval = "1 hour"

// maxRange caps how long a queried time range may be, so a single request
// can't scan the entire retained history.
const maxRange = 90 * 24 * time.Hour

// defaultRange is the window used when a request specifies neither end.
const defaultRange = 24 * time.Hour

// ParseInterval validates a requested bucket interval against the
// allowlist, defaulting when empty.
func ParseInterval(raw string) (string, error) {
	if raw == "" {
		return DefaultInterval, nil
	}
	if !allowedIntervals[raw] {
		return "", fmt.Errorf("invalid interval %q (allowed: 1 minute, 5 minutes, 15 minutes, 1 hour, 6 hours, 1 day, 1 week)", raw)
	}
	return raw, nil
}

// ParseRange reads the from/to query parameters as RFC 3339 timestamps,
// filling in defaults and rejecting ranges that are inverted or longer
// than maxRange. now is passed in rather than read from the clock so
// tests get deterministic defaults.
func ParseRange(q url.Values, now time.Time) (from, to time.Time, err error) {
	to = now
	if raw := q.Get("to"); raw != "" {
		to, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid to %q (want an RFC 3339 timestamp, e.g. 2026-01-31T15:04:05Z)", raw)
		}
	}

	from = to.Add(-defaultRange)
	if raw := q.Get("from"); raw != "" {
		from, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid from %q (want an RFC 3339 timestamp, e.g. 2026-01-31T15:04:05Z)", raw)
		}
	}

	if !from.Before(to) {
		return time.Time{}, time.Time{}, fmt.Errorf("from must be strictly before to (got from=%s, to=%s)", from.Format(time.RFC3339), to.Format(time.RFC3339))
	}
	if to.Sub(from) > maxRange {
		return time.Time{}, time.Time{}, fmt.Errorf("range too long: %s exceeds the %s maximum", to.Sub(from), maxRange)
	}
	return from, to, nil
}

// ParseLimit reads a row limit, defaulting when absent and rejecting
// anything outside 1..maxRows.
func ParseLimit(q url.Values, def int) (int, error) {
	raw := q.Get("limit")
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid limit %q (want a number)", raw)
	}
	if n < 1 || n > maxRows {
		return 0, fmt.Errorf("limit %d out of range (want 1..%d)", n, maxRows)
	}
	return n, nil
}

// ParseBotScoreMin reads the bot-score cutoff used to split bot from
// human IPs, defaulting to DefaultBotScoreMin. Bounded to 0..100 to match
// scoring.MaxScore's range.
func ParseBotScoreMin(q url.Values) (int, error) {
	raw := q.Get("bot_score_min")
	if raw == "" {
		return DefaultBotScoreMin, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid bot_score_min %q (want a number)", raw)
	}
	if n < 0 || n > 100 {
		return 0, fmt.Errorf("bot_score_min %d out of range (want 0..100)", n)
	}
	return n, nil
}
