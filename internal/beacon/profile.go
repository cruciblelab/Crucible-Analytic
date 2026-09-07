package beacon

import "github.com/cruciblelab/crucible-analytic/internal/profile"

// The same bridge internal/collector has, for the same two settings.
//
// # Why the beacon needs one at all
//
// It was written without one, and the gap only showed when the panel
// started asking "is this data being collected". The beacon runs its own
// asnlookup.Resolver against its own [asn_lookup] section, so a
// deployment can perfectly well run the collector at Tam Crucible and
// the beacon with the lookup off - and then beacon_events has no country
// while traffic_snapshots does.
//
// Without this the panel had one profile to reason from, the collector's,
// and would have said "collected" about a column the beacon leaves empty.
// One service's configuration is not evidence about another's.
//
// A mapping rather than a stored value, for the reason
// internal/collector/profile.go gives: a name written down beside the
// settings it summarises goes stale the first time somebody edits one of
// them.

// ProfileLevel says which IP-intelligence level this configuration is
// actually running.
func (c *Config) ProfileLevel() profile.Level {
	switch {
	case !c.ASNLookup.Enabled:
		return profile.LevelOff
	case c.ASNLookup.CountryOnly:
		return profile.LevelCountry
	default:
		return profile.LevelFull
	}
}

// Profile names what this configuration is, false when no offered
// profile matches.
func (c *Config) Profile() (profile.Profile, bool) {
	return profile.Match(c.ProfileLevel())
}
