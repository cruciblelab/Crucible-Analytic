package beacon

import (
	"net/url"
	"sort"
	"strings"
)

// Campaign is the acquisition context an event arrived with, split into
// the dimensions a panel actually groups by.
//
// Splitting matters more than it looks. An earlier version stored only
// the re-serialized query string ("utm_medium=email&utm_source=news"),
// which answers "which exact campaign combination brought traffic" and
// cannot answer "how much did Instagram bring in total" - every distinct
// combination of parameters is its own group, so one source spread
// across five campaigns appears as five unrelated rows. Typed columns
// make each dimension groupable and filterable on its own, which is what
// every real question about marketing spend needs.
type Campaign struct {
	Source  string // utm_source: instagram, newsletter, google
	Medium  string // utm_medium: social, email, cpc
	Name    string // utm_campaign: spring-sale
	Term    string // utm_term: the keyword, when the advertiser sets one
	Content string // utm_content: which variant of the same campaign
	// Ref is the informal equivalent of utm_source used by sites that
	// never adopted UTM. Kept separate rather than folded into Source
	// because the two have different reliability: utm_source is set by
	// whoever built the link, ref is set by anyone at all.
	Ref string

	// ClickIDSource names the ad network whose per-click identifier was
	// present: "google", "facebook", "microsoft", or "" for none.
	//
	// This is the column worth grouping by. The raw click identifier is
	// unique per click, so grouping by it produces one row per visit and
	// tells you nothing; knowing *which network* the paid click came
	// from is the actual analytic value, and it is not a unique
	// identifier for anybody.
	ClickIDSource string
	// ClickID is the raw per-click identifier (gclid/fbclid/msclkid).
	//
	// Empty unless the deployment opts in via CampaignPolicy. It is the
	// single highest-sensitivity field this package can store: unique per
	// click by construction, and resolvable to a person by the ad network
	// that issued it (though never by us). Its only legitimate use is
	// uploading offline conversions back to that network, which this
	// project does not do - so not storing it is both the private default
	// and the honest one.
	ClickID string
}

// Empty reports whether no acquisition context was present at all, which
// is true of the overwhelming majority of pageviews.
func (c Campaign) Empty() bool {
	return c == Campaign{}
}

// Ad network click identifiers, and the network name each one implies.
const (
	ClickSourceGoogle    = "google"
	ClickSourceFacebook  = "facebook"
	ClickSourceMicrosoft = "microsoft"
)

// clickIDParams maps a click-identifier parameter to its network.
//
// Ordered slice rather than a map: when a URL carries more than one -
// which happens when a link is shared onward after an ad click - the
// network recorded must be the same on every run, and map iteration
// order in Go is deliberately not.
var clickIDParams = []struct {
	param   string
	network string
}{
	{"gclid", ClickSourceGoogle},
	{"fbclid", ClickSourceFacebook},
	{"msclkid", ClickSourceMicrosoft},
}

// standardParams is the built-in allowlist: the complete set of query
// parameters this project understands as acquisition context.
//
// Storing the whole query string would be easier and is what several
// analytics tools do, but query strings routinely carry password-reset
// tokens, invite codes, session identifiers and email addresses. Those
// would then sit in an analytics table for as long as retention allows,
// readable by anyone holding a panel token - a much wider audience than
// the application's own database. An allowlist means a parameter nobody
// chose is invisible, which is the right default for a field this
// project has no control over the contents of.
var standardParams = []string{
	"utm_source", "utm_medium", "utm_campaign", "utm_term", "utm_content",
	"ref",
	"gclid", "fbclid", "msclkid",
}

// maxCampaignValueLen bounds one campaign dimension. Shorter than the
// whole-query cap because a single value is a label, not a payload:
// anything approaching this is a mistake or an attempt to bloat the row.
const maxCampaignValueLen = 256

// CampaignPolicy decides which query parameters survive into storage.
//
// It exists because the answer is a legal question as much as a
// technical one, and legal answers differ per deployment and change
// after the fact. utm_term is the clearest case: it normally holds the
// keyword an advertiser bid on, but an ad platform can be configured to
// substitute the visitor's actual search text - so whether it may be
// stored depends on how the customer runs their advertising and what
// their counsel says about it. Making that a config line rather than a
// code change means the answer can change without a release.
type CampaignPolicy struct {
	// dropped names standard parameters this deployment refuses.
	dropped map[string]bool
	// extra names non-standard parameters to keep in Query anyway. They
	// get no dedicated column - they are visible, not groupable.
	extra map[string]bool
	// storeClickIDs keeps the raw per-click identifier, not just which
	// network it came from.
	storeClickIDs bool
}

// The zero CampaignPolicy is deliberately equivalent to
// DefaultCampaignPolicy: its nil maps read as "nothing dropped, nothing
// extra", so a Server or a test that never sets one keeps the standard
// parameters rather than silently storing no campaign data at all.

// NewCampaignPolicy builds a policy. A nil/empty argument set yields the
// default: every standard parameter kept, raw click identifiers not
// stored.
func NewCampaignPolicy(drop, extra []string, storeClickIDs bool) CampaignPolicy {
	p := CampaignPolicy{
		dropped:       make(map[string]bool, len(drop)),
		extra:         make(map[string]bool, len(extra)),
		storeClickIDs: storeClickIDs,
	}
	for _, name := range drop {
		p.dropped[strings.ToLower(strings.TrimSpace(name))] = true
	}
	for _, name := range extra {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		p.extra[name] = true
	}
	return p
}

// DefaultCampaignPolicy keeps every standard parameter and stores no raw
// click identifiers.
func DefaultCampaignPolicy() CampaignPolicy {
	return NewCampaignPolicy(nil, nil, false)
}

// keeps reports whether a parameter survives this policy.
func (p CampaignPolicy) keeps(name string) bool {
	if p.dropped[name] {
		return false
	}
	if p.extra[name] {
		return true
	}
	for _, std := range standardParams {
		if std == name {
			return true
		}
	}
	return false
}

// Apply splits a parsed query string into typed campaign dimensions and
// the re-serialized allowlisted query string.
//
// The returned query string is kept alongside the typed columns rather
// than replaced by them, for two reasons: it preserves the exact
// combination a link carried (so "which precise campaign link performed
// best" stays answerable), and it is where an `extra` parameter with no
// column of its own lives.
func (p CampaignPolicy) Apply(values url.Values) (Campaign, string) {
	var c Campaign
	kept := make(url.Values, len(values))

	keep := func(name, value string) {
		if value == "" {
			return
		}
		kept.Set(name, value)
	}

	get := func(name string) string {
		if !p.keeps(name) {
			return ""
		}
		vals := values[name]
		if len(vals) == 0 {
			return ""
		}
		// Only the first value: a repeated parameter is either a mistake
		// or an attempt to inflate the stored string, and neither is
		// worth a multi-valued column.
		return sanitizeText(vals[0], maxCampaignValueLen)
	}

	c.Source = get("utm_source")
	c.Medium = get("utm_medium")
	c.Name = get("utm_campaign")
	c.Term = get("utm_term")
	c.Content = get("utm_content")
	c.Ref = get("ref")

	keep("utm_source", c.Source)
	keep("utm_medium", c.Medium)
	keep("utm_campaign", c.Name)
	keep("utm_term", c.Term)
	keep("utm_content", c.Content)
	keep("ref", c.Ref)

	for _, click := range clickIDParams {
		raw := get(click.param)
		if raw == "" {
			continue
		}
		// First match wins, so a link carrying two click identifiers is
		// attributed consistently rather than by map order.
		if c.ClickIDSource == "" {
			c.ClickIDSource = click.network
			if p.storeClickIDs {
				c.ClickID = raw
			}
		}
		// The raw value reaches the catch-all query string only when the
		// deployment opted in. Otherwise the fact of a paid click is
		// recorded and the identifier is not, which is the whole point of
		// the setting.
		if p.storeClickIDs {
			keep(click.param, raw)
		}
	}

	for name := range p.extra {
		if _, standard := indexOf(standardParams, name); standard {
			continue
		}
		keep(name, get(name))
	}

	return c, serializeQuery(kept)
}

func indexOf(haystack []string, needle string) (int, bool) {
	for i, s := range haystack {
		if s == needle {
			return i, true
		}
	}
	return 0, false
}

// serializeQuery renders the kept parameters in sorted order, so
// "?utm_source=x&utm_medium=y" and "?utm_medium=y&utm_source=x" produce
// one stored value rather than two.
func serializeQuery(kept url.Values) string {
	if len(kept) == 0 {
		return ""
	}
	keys := make([]string, 0, len(kept))
	for key := range kept {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, key := range keys {
		if b.Len() > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(key))
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(kept.Get(key)))
	}
	return truncateRunes(b.String(), maxQueryLen)
}
