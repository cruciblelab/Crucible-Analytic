package limiter

import (
	"strings"
	"sync/atomic"
)

// GeoBlocklist is a denylist of countries (ISO 3166-1 alpha-2) and ASNs,
// checked independently of Limiter's own concurrency/rate state - it has
// no shared state with Limiter's counters, just the same Decision
// vocabulary callers already switch on for Admit's result. Kept as its
// own small type rather than a new field/dimension folded into
// Limiter/Config: see NOTES.md for the richer per-rule-policy version of
// this that was deliberately deferred, and why that version (unlike this
// one) might actually deserve to live somewhere else.
//
// A match is meant to be treated as an unconditional reject by the
// caller, regardless of Config.Policy - blocking by geography/ASN is a
// deliberate security decision, not collector-load-shedding behavior.
//
// # Replaceable while connections are being served
//
// The rules are held behind an atomic pointer, the same shape Limiter
// uses for its Config, because this is the setting a support call
// actually reaches for: "we are being hit from there, block it". Until
// A5.2 that meant SSH, an edit and a restart - the longest possible path
// while an attack is in progress.
//
// The rules themselves are never mutated after Set builds them, so a
// connection that read the pointer keeps a consistent view for the whole
// of its check even if another list arrives mid-decision.
type GeoBlocklist struct {
	rules atomic.Pointer[geoRules]
}

// geoRules is one immutable version of the lists.
type geoRules struct {
	countries map[string]struct{}
	asns      map[int]struct{}
}

// NewGeoBlocklist builds a blocklist from countries (normalized to
// uppercase, matching asnlookup's own country-code casing) and asns.
//
// # It no longer returns nil for empty input
//
// It used to, so that a caller could skip resolving a connection's
// country and ASN entirely when there was nothing to check them against.
// That optimisation is real and is kept - but a blocklist that can be
// filled in later cannot answer "is there anything here" once, at
// startup. Callers ask Active instead, which is one atomic load and
// gives the same answer for a deployment that blocks nothing.
func NewGeoBlocklist(countries []string, asns []int) *GeoBlocklist {
	b := &GeoBlocklist{}
	b.Set(countries, asns)
	return b
}

// Set replaces the rules.
//
// Safe to call while Blocked is running on other goroutines: the new
// rules are built first and published in one store, so no reader ever
// sees a half-filled map.
func (b *GeoBlocklist) Set(countries []string, asns []int) {
	if b == nil {
		return
	}
	rules := &geoRules{
		countries: make(map[string]struct{}, len(countries)),
		asns:      make(map[int]struct{}, len(asns)),
	}
	for _, c := range countries {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" {
			// An empty entry would match the "" country asnlookup
			// returns for an address it could not resolve, which would
			// block every unresolvable connection - the opposite of what
			// a blank line in a config file means.
			continue
		}
		rules.countries[c] = struct{}{}
	}
	for _, a := range asns {
		if a == 0 {
			// Same reasoning: 0 is asnlookup's "not resolved".
			continue
		}
		rules.asns[a] = struct{}{}
	}
	b.rules.Store(rules)
}

// Active reports whether anything is blocked at all.
//
// The question callers used to answer with a nil check. Worth asking
// before resolving an address, because a deployment that blocks nothing
// - the default - should not pay for a geography lookup per connection.
// Nil-safe, like Blocked.
func (b *GeoBlocklist) Active() bool {
	if b == nil {
		return false
	}
	rules := b.rules.Load()
	return rules != nil && (len(rules.countries) > 0 || len(rules.asns) > 0)
}

// Blocked reports whether country or asn is on the blocklist. country ==
// "" or asn == 0 (asnlookup's own "not resolved" zero values) never
// match on their own - a rule needs a known value to compare against, so
// an unresolved lookup can never be geo-blocked. Safe to call on a nil
// *GeoBlocklist (always false), matching this package's other nil-safe
// accessors.
func (b *GeoBlocklist) Blocked(country string, asn int) bool {
	if b == nil {
		return false
	}
	rules := b.rules.Load()
	if rules == nil {
		return false
	}
	if country != "" {
		if _, ok := rules.countries[strings.ToUpper(country)]; ok {
			return true
		}
	}
	if asn != 0 {
		if _, ok := rules.asns[asn]; ok {
			return true
		}
	}
	return false
}
