package collector

import "github.com/cruciblelab/crucible-analytic/internal/profile"

// The bridge between what this file says and what internal/profile
// calls it.
//
// # Why the derivation lives here and not there
//
// internal/profile knows what each profile costs; it does not know what
// a collector's TOML looks like, and it must not, or the package that
// answers "will this fit" would depend on the package whose settings it
// is judging. So the mapping is here, in one place, and it is a mapping
// rather than a stored value: nothing writes "the profile is Dengeli"
// anywhere. A2's rule, and the reason for it, is that a name stored
// beside the settings it summarises is a second source of truth that
// goes stale the first time somebody edits one setting by hand.

// ProfileLevel says which IP-intelligence level this configuration is
// actually running.
//
// One axis, because one axis is what costs memory. See the Level
// documentation for why JA4 and the beacon are not axes here despite
// A2's original table listing them.
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

// Profile names what this configuration is. The second result is false
// when nothing in the offered set matches, which today cannot happen -
// the three levels are exhaustive - and is returned anyway so a fourth
// level added later cannot be silently reported as one of these three.
func (c *Config) Profile() (profile.Profile, bool) {
	return profile.Match(c.ProfileLevel())
}

// RateStoreBound is the worst case the in-memory rate store can reach,
// and whether that worst case exists at all.
//
// It is bounded by the limiter rather than by the store: internal/proxy
// checks the geo list and then the limiter, and only a connection that
// passes both reaches RecordRequest. So the map cannot hold more than
// one entry per admitted request per TTL, whatever an attacker does with
// addresses. With the limit off there is no such ceiling, and the second
// result says so rather than returning a number that looks like one.
func (c *Config) RateStoreBound() (bytes uint64, bounded bool) {
	return profile.RateStoreBytes(c.Limits.MaxRequestsPerSecond, c.Cache.TTLSeconds)
}
