package limiter

import "strings"

// GeoBlocklist is a denylist of countries (ISO 3166-1 alpha-2) and ASNs,
// checked independently of Limiter's own concurrency/rate state - it has
// no shared state with Limiter's counters, just the same Decision
// vocabulary callers already switch on for Admit's result. Kept as its
// own small, stateless type rather than a new field/dimension folded
// into Limiter/Config: see NOTES.md for the richer per-rule-policy
// version of this that was deliberately deferred, and why that version
// (unlike this one) might actually deserve to live somewhere else.
//
// A match is meant to be treated as an unconditional reject by the
// caller, regardless of Config.Policy - blocking by geography/ASN is a
// deliberate security decision, not collector-load-shedding behavior.
type GeoBlocklist struct {
	countries map[string]struct{}
	asns      map[int]struct{}
}

// NewGeoBlocklist builds a blocklist from countries (normalized to
// uppercase, matching asnlookup's own country-code casing) and asns.
// Returns nil if both are empty, so callers can skip resolving a
// connection's country/ASN entirely when there's nothing to check it
// against, rather than doing that work only to consult an always-empty
// blocklist.
func NewGeoBlocklist(countries []string, asns []int) *GeoBlocklist {
	if len(countries) == 0 && len(asns) == 0 {
		return nil
	}
	b := &GeoBlocklist{
		countries: make(map[string]struct{}, len(countries)),
		asns:      make(map[int]struct{}, len(asns)),
	}
	for _, c := range countries {
		b.countries[strings.ToUpper(strings.TrimSpace(c))] = struct{}{}
	}
	for _, a := range asns {
		b.asns[a] = struct{}{}
	}
	return b
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
	if country != "" {
		if _, ok := b.countries[strings.ToUpper(country)]; ok {
			return true
		}
	}
	if asn != 0 {
		if _, ok := b.asns[asn]; ok {
			return true
		}
	}
	return false
}
