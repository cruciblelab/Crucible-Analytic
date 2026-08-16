package logging

import (
	"log/slog"
	"time"
)

// Controls are the knobs a running service can turn on its own logging.
//
// Returned by Setup so the settings refresher can apply a level change
// without a restart. Verbose logging is the one log setting a support
// call actually reaches for - "turn on debug, reproduce it, turn it off"
// - and needing SSH for that defeats the point of the whole settings
// layer.
type Controls struct {
	// level is the live minimum. slog reads it on every record, so
	// changing it takes effect on the next line rather than the next
	// restart.
	level *slog.LevelVar
	// base is the configured level to fall back to when a temporary
	// raise expires.
	base slog.Level
}

// Level reports the level in force.
func (c *Controls) Level() slog.Level {
	if c == nil || c.level == nil {
		return slog.LevelInfo
	}
	return c.level.Level()
}

// SetLevel changes the minimum level written from now on.
func (c *Controls) SetLevel(level slog.Level) {
	if c == nil || c.level == nil {
		return
	}
	c.level.Set(level)
}

// Base is the configured level, which a temporary raise returns to.
func (c *Controls) Base() slog.Level {
	if c == nil {
		return slog.LevelInfo
	}
	return c.base
}

// Apply resolves the configured level and any temporary raise into the
// level that should be in force, and sets it.
//
// verboseUntil is an RFC3339 timestamp, empty when nothing is raised.
// Parsing it here rather than storing a boolean is what makes the raise
// self-expiring: nothing has to remember to turn it off, and a service
// that restarts mid-window comes back still raised rather than silently
// dropping back to info.
//
// An unparseable timestamp is treated as "not raised". A malformed value
// must not be able to pin a deployment at debug forever, which is how a
// disk fills.
func (c *Controls) Apply(configured slog.Level, verboseUntil string, now time.Time) slog.Level {
	if c == nil || c.level == nil {
		return configured
	}
	c.base = configured

	effective := configured
	if verboseUntil != "" {
		if until, err := time.Parse(time.RFC3339, verboseUntil); err == nil && now.Before(until) {
			// Only ever downwards to debug. A "verbose until" that made
			// logging *quieter* would be a surprising reading of the word.
			if slog.LevelDebug < effective {
				effective = slog.LevelDebug
			}
		}
	}
	c.level.Set(effective)
	return effective
}

// VerboseActive reports whether a temporary raise is currently in force,
// so the panel can show "ayrıntılı kayıt açık, 14 dakika sonra kapanır"
// rather than leaving the customer to work it out.
func VerboseActive(verboseUntil string, now time.Time) (bool, time.Duration) {
	if verboseUntil == "" {
		return false, 0
	}
	until, err := time.Parse(time.RFC3339, verboseUntil)
	if err != nil || !now.Before(until) {
		return false, 0
	}
	return true, until.Sub(now)
}
