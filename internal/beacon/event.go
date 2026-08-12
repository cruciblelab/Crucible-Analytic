// Package beacon ingests client-side analytics events sent by a small
// JavaScript snippet embedded in the site being measured.
//
// It is the second of this project's two data sources, and the two are
// complementary rather than redundant. The collector (internal/proxy,
// internal/fullproxy) sees every connection that reaches the site,
// including clients that never run JavaScript, and fingerprints them -
// but in passthrough mode it cannot see a URL at all, and in full mode
// it deliberately doesn't record one. The beacon sees paths, referrers,
// screen sizes and custom events - but only from clients that execute
// JavaScript, which makes it structurally near-blind to automated
// traffic. That blindness is why conventional analytics tools
// under-report bots; running both sources against one database is what
// closes the gap, and beacon_events.ip is the key that joins them.
//
// Everything in an Event arrives from a browser and is therefore
// attacker-controlled. Nothing here trusts it: see Event.Validate and
// BuildRow for the normalization every field goes through before it can
// reach the database.
package beacon

import (
	"fmt"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Event types. The wire format admits exactly these two; anything else
// is rejected rather than stored, so a reader can group by event_type
// without first discovering what values exist.
const (
	// TypePageview is emitted automatically by the snippet on load and
	// on SPA navigation.
	TypePageview = "pageview"
	// TypeEvent is a named event the site's own code raised via
	// crucible('event', name).
	TypeEvent = "event"
)

// Field length caps, in runes. These are truncation points, not
// rejection points: a legitimate value never approaches them, and
// keeping a truncated event is more useful than dropping a real
// visitor's pageview because their CMS generated an absurd title. The
// body as a whole is capped separately and much lower - see
// maxBodyBytes in server.go - so these only ever bite on one field
// being disproportionately long, not on bulk.
const (
	maxNameLen     = 128
	maxPathLen     = 1024
	maxQueryLen    = 512
	maxTitleLen    = 512
	maxHostLen     = 253 // the maximum length of a DNS name
	maxLanguageLen = 35  // BCP 47 allows long tags; nothing real is longer
	// maxScreenPx bounds the reported screen dimensions. Anything past
	// this is a client lying or broken, and clamping keeps the value
	// inside INTEGER without the writer having to think about overflow.
	maxScreenPx = 32767
)

// Event is the wire format the JS snippet POSTs. Field names are spelled
// out rather than golfed down to single letters: the payload is a few
// hundred bytes either way, and being able to read a request body in a
// browser's network tab is worth more than the saved bytes.
type Event struct {
	// Site is the site_id this event belongs to. The server checks it
	// against its configured allowlist - the snippet is public, so this
	// field is a claim, not a credential.
	Site string `json:"site"`
	Type string `json:"type"`
	// Name is set only for TypeEvent.
	Name string `json:"name,omitempty"`
	// URL is the root-relative path and query of the page
	// (location.pathname + location.search). A client that sends an
	// absolute URL instead has everything but the path discarded.
	URL string `json:"url"`
	// Referrer is document.referrer, already dropped by the snippet when
	// it is same-origin. Its query string is discarded here regardless.
	Referrer string `json:"referrer,omitempty"`
	Title    string `json:"title,omitempty"`
	ScreenW  int    `json:"screen_w,omitempty"`
	ScreenH  int    `json:"screen_h,omitempty"`
	Language string `json:"language,omitempty"`
}

// Row is one beacon event ready to persist: an Event that has been
// validated, normalized, and enriched with everything the server knows
// that the client didn't send (time, IP, user agent classification,
// country/ASN, visitor ID). Field set matches schema.sql's
// beacon_events table.
type Row struct {
	Time      time.Time
	SiteID    string
	VisitorID string
	EventType string
	EventName string
	Path      string
	Query     string
	Title     string

	ReferrerHost string
	ReferrerPath string

	IP netip.Addr

	Browser string
	OS      string
	Device  string
	IsBotUA bool

	ScreenW  int
	ScreenH  int
	Language string

	Country string
	ASN     int
	ASNOrg  string
}

// Validate checks the parts of an Event that must be right for the event
// to mean anything at all. Everything else is normalized rather than
// rejected - see BuildRow.
//
// It deliberately does not check that Site is a site this server
// accepts: that is an authorization question the server answers against
// its own configuration, not something a payload can be inspected for.
func (e Event) Validate() error {
	switch e.Type {
	case TypePageview:
	case TypeEvent:
		if strings.TrimSpace(e.Name) == "" {
			return fmt.Errorf("beacon: type %q requires a name", TypeEvent)
		}
	default:
		return fmt.Errorf("beacon: invalid type %q (want %q or %q)", e.Type, TypePageview, TypeEvent)
	}
	if e.Site == "" {
		return fmt.Errorf("beacon: site is required")
	}
	return nil
}

// Enrichment is what the server knows about a request that the payload
// itself cannot be trusted for (or does not carry at all).
type Enrichment struct {
	Time      time.Time
	IP        netip.Addr
	VisitorID string
	UserAgent UserAgent
	Country   string
	ASN       int
	ASNOrg    string
}

// BuildRow converts a validated Event plus server-side enrichment into a
// storable Row. It is a pure function, so the whole normalization path -
// which is the only thing standing between a hostile payload and the
// database - is unit-testable without an HTTP server or a live
// connection, the same reasoning behind storage.BuildRows.
func BuildRow(e Event, en Enrichment) Row {
	path, query := splitURL(e.URL)
	refHost, refPath := splitReferrer(e.Referrer)

	name := ""
	if e.Type == TypeEvent {
		name = sanitizeText(e.Name, maxNameLen)
	}

	return Row{
		Time:      en.Time,
		SiteID:    e.Site,
		VisitorID: en.VisitorID,
		EventType: e.Type,
		EventName: name,
		Path:      path,
		Query:     query,
		Title:     sanitizeText(e.Title, maxTitleLen),

		ReferrerHost: refHost,
		ReferrerPath: refPath,

		IP: en.IP,

		Browser: en.UserAgent.Browser,
		OS:      en.UserAgent.OS,
		Device:  en.UserAgent.Device,
		IsBotUA: en.UserAgent.IsBot,

		ScreenW:  clampScreen(e.ScreenW),
		ScreenH:  clampScreen(e.ScreenH),
		Language: sanitizeText(e.Language, maxLanguageLen),

		Country: en.Country,
		ASN:     en.ASN,
		ASNOrg:  en.ASNOrg,
	}
}

// splitURL turns whatever the client sent into a stored path and a
// stored query. A client that sends an absolute URL
// ("https://evil.example/x?y=1") keeps only its path and allowlisted
// query parameters; the scheme and host are dropped, since the server
// already knows which site this is and a payload-supplied host is not
// evidence of anything.
func splitURL(raw string) (path, query string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "/", ""
	}

	// An unparseable URL is not worth rejecting the event over - the
	// pageview still happened - so fall back to treating the whole
	// string as a path and let sanitizeText make it safe.
	u, err := url.Parse(raw)
	if err != nil {
		return normalizePath(sanitizeText(raw, maxPathLen)), ""
	}

	return normalizePath(sanitizeText(u.Path, maxPathLen)), sanitizeQuery(u.Query())
}

// normalizePath guarantees a leading slash, so "/pricing" and "pricing"
// don't become two different rows for the same page.
func normalizePath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

// campaignParams is the complete set of query parameters worth keeping.
//
// Storing the raw query string would be easier and is what several
// analytics tools do, but query strings routinely carry password-reset
// tokens, invite codes, session identifiers and email addresses. Those
// would then sit in an analytics table forever, readable by anyone with
// a panel token - a much wider audience than the application's own
// database. An allowlist means a new parameter is invisible until
// someone deliberately adds it here, which is the right default for a
// field this project has no control over the contents of.
var campaignParams = map[string]bool{
	"utm_source":   true,
	"utm_medium":   true,
	"utm_campaign": true,
	"utm_term":     true,
	"utm_content":  true,
	"ref":          true,
	"gclid":        true,
	"fbclid":       true,
	"msclkid":      true,
}

// sanitizeQuery keeps only campaignParams, re-serialized in sorted order
// so that "?utm_source=x&utm_medium=y" and "?utm_medium=y&utm_source=x"
// produce one row rather than two.
func sanitizeQuery(values url.Values) string {
	kept := make(url.Values, len(values))
	for key, vals := range values {
		if !campaignParams[key] || len(vals) == 0 {
			continue
		}
		// Only the first value: a repeated parameter is either a mistake
		// or an attempt to inflate the stored string, and neither is
		// worth a multi-valued column.
		kept.Set(key, sanitizeText(vals[0], maxQueryLen))
	}
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

// splitReferrer breaks document.referrer into the host you group by and
// the path you occasionally drill into. The referrer's own query string
// is dropped outright: it is another site's parameters, so this project
// has even less idea what might be in it than it does about its own.
func splitReferrer(raw string) (host, path string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// A referrer with no host is not a referrer - either the client
		// sent something malformed, or it sent a relative URL, which
		// would be a same-origin navigation the snippet should already
		// have dropped.
		return "", ""
	}
	return sanitizeText(strings.ToLower(u.Hostname()), maxHostLen), sanitizeText(u.Path, maxPathLen)
}

func clampScreen(px int) int {
	if px < 0 {
		return 0
	}
	if px > maxScreenPx {
		return maxScreenPx
	}
	return px
}

// sanitizeText makes an arbitrary client-supplied string safe to store:
// valid UTF-8, free of control characters, trimmed, and bounded.
//
// The UTF-8 and control-character passes are correctness, not
// tidiness. PostgreSQL TEXT cannot hold a NUL byte or invalid UTF-8 at
// all, and rejects the whole statement when handed one. Because rows
// are written in batches (see writer.go), one hostile payload would
// otherwise take down the entire batch it landed in - every other
// visitor's events included. Stripping here means a malformed event
// degrades only itself.
func sanitizeText(s string, maxRunes int) string {
	if s == "" {
		return ""
	}
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "")
	}
	s = strings.Map(func(r rune) rune {
		// Unicode line/paragraph separators are stripped alongside the
		// ASCII control range: they are invisible in a panel but break
		// line-oriented log and CSV output downstream.
		if r < 0x20 || r == 0x7f || r == 0x2028 || r == 0x2029 {
			return -1
		}
		return r
	}, s)
	return truncateRunes(strings.TrimSpace(s), maxRunes)
}

// truncateRunes cuts s to at most maxRunes runes, never mid-rune - a
// byte-wise cut would leave a partial UTF-8 sequence, which is exactly
// the invalid input sanitizeText just finished removing.
func truncateRunes(s string, maxRunes int) string {
	if len(s) <= maxRunes { // bytes >= runes, so this is a cheap fast path
		return s
	}
	count := 0
	for i := range s {
		count++
		if count > maxRunes {
			return s[:i]
		}
	}
	return s
}
