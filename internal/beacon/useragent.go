package beacon

import "strings"

// UserAgent is what the User-Agent header could be classified into.
//
// Every field is "" when nothing matched, never a guess and never
// "Unknown": ” means this classifier did not recognize the string,
// which is a different and more useful statement than inventing a
// bucket. A panel showing "browser: (unrecognized) 4%" is telling the
// truth; one showing "Other 4%" invites the reader to assume it was
// classified and simply rare.
type UserAgent struct {
	Browser string
	OS      string
	// Device is "desktop", "mobile", "tablet", or "" when unknown.
	Device string
	// IsBot reports that the user agent *self-identifies* as
	// automation. It is not a bot-detection mechanism and must not be
	// read as one - anything that wants to hide simply sends a normal
	// browser string. Real bot detection in this project is the
	// collector's job (JA4 fingerprinting, request-rate scoring, ASN
	// signals); this flag exists so honest crawlers can be filtered out
	// of human-facing numbers cheaply, and so the far more interesting
	// case - a client that runs JavaScript *and* admits to being a bot -
	// is visible at all.
	IsBot bool
}

// Device values.
const (
	DeviceDesktop = "desktop"
	DeviceMobile  = "mobile"
	DeviceTablet  = "tablet"
)

// browserMarkers maps a substring to a browser name, in the order it
// must be tested.
//
// Order is the entire difficulty of user agent parsing, not a detail.
// Chrome's string contains "Safari"; Edge's contains both "Chrome" and
// "Safari"; Opera's and Samsung Internet's contain all three. Every
// derivative therefore has to be tested before the browser it derives
// from, which is why this is an ordered slice rather than a map.
var browserMarkers = []struct{ marker, name string }{
	// iOS forces every browser onto WebKit, so these say "like Safari"
	// while being anything but. They have to come first: their strings
	// also contain "Safari" and, for CriOS, nothing that says "Chrome".
	{"CriOS", "Chrome"},
	{"FxiOS", "Firefox"},
	{"EdgiOS", "Edge"},
	{"OPiOS", "Opera"},

	// Chromium derivatives, before Chrome itself.
	{"Edg", "Edge"}, // "Edg/" is modern Chromium Edge; "Edge/" was the EdgeHTML one
	{"OPR", "Opera"},
	{"SamsungBrowser", "Samsung Internet"},
	{"YaBrowser", "Yandex"},
	{"Vivaldi", "Vivaldi"},
	{"UCBrowser", "UC Browser"},
	{"HeadlessChrome", "Headless Chrome"},

	{"Firefox", "Firefox"},
	{"Chrome", "Chrome"},
	// Safari last: every Chromium and WebKit browser above also carries
	// it, so reaching here means nothing more specific matched.
	{"Safari", "Safari"},
}

// osMarkers maps a substring to an OS name, in the order it must be
// tested. Android contains "Linux"; iPadOS 13+ deliberately reports
// itself as "Macintosh ... Mac OS X" to get desktop sites, so the
// iPad/iPhone tokens have to be tested before the Mac one.
var osMarkers = []struct{ marker, name string }{
	{"iPhone", "iOS"},
	{"iPad", "iOS"},
	{"iPod", "iOS"},
	{"Android", "Android"},
	{"CrOS", "ChromeOS"},
	{"Windows", "Windows"},
	{"Mac OS X", "macOS"},
	{"Macintosh", "macOS"},
	{"FreeBSD", "FreeBSD"},
	{"OpenBSD", "OpenBSD"},
	{"NetBSD", "NetBSD"},
	{"Linux", "Linux"},
}

// botNames are substrings that identify automation unambiguously enough
// to match anywhere in the string: distinctive product names that
// cannot appear in a real browser's string by accident. They cover the
// tools and libraries that do not follow the "-bot" naming convention
// botTokenSuffixes handles.
var botNames = []string{
	"headlesschrome", "phantomjs", "puppeteer", "playwright", "selenium",
	"python-requests", "python-urllib", "aiohttp", "httpx", "scrapy",
	"go-http-client", "java/", "okhttp", "axios", "node-fetch", "got (",
	"curl/", "wget/", "libwww-perl", "guzzlehttp", "postmanruntime",
	"apachebench", "wrk/", "k6/", "vegeta", "hey/",
	"lighthouse", "pagespeed", "gtmetrix", "pingdom", "uptimerobot",
	"semrush", "ahrefs", "mj12", "bytespider", "baiduspider",
	"slurp", "duckduckgo", "facebookexternalhit", "ia_archiver",
	"chatgpt-user", "sogou", "monitoring", "healthcheck",
}

// botTokenSuffixes catch the naming convention almost every crawler
// follows: Googlebot, bingbot, AhrefsBot, GPTBot, ClaudeBot, Applebot,
// PetalBot, and hundreds of smaller ones nobody will ever enumerate.
// Matching the *end of a token* rather than the whole token is what
// makes that work - "bot" as a standalone word appears in almost no
// real crawler string, since the vendor name is glued to it.
var botTokenSuffixes = []string{"bot", "crawler", "spider", "scraper", "fetcher"}

// notBotTokens are tokens that end in a botTokenSuffix but belong to
// genuine browsers.
//
// This exception list is the price of catching unknown crawlers
// generically, and it is a price worth paying: Cubot is a real Android
// manufacturer whose model names appear in millions of ordinary mobile
// user agents, so without this entry every one of those visitors would
// be filed as a crawler - a silent, systematic undercount of exactly
// the budget-phone mobile users a Turkish site sees most. Add to this
// list when a device brand collides; do not weaken the suffix rule,
// which is what makes the generic case work at all.
var notBotTokens = map[string]bool{
	"cubot": true,
}

// ParseUserAgent classifies a User-Agent header.
//
// It is a deliberately small, dependency-free classifier covering the
// browsers and platforms that make up the overwhelming majority of real
// traffic, in the spirit of the rest of this project's hand-written
// parsers. It does not attempt version numbers, exact device models, or
// the very long tail - a full user agent database is a large dependency
// that needs continuous updating, and modern browsers are actively
// freezing and reducing their user agent strings, so that dependency
// buys steadily less over time. Anything it does not recognize is
// reported as "" rather than guessed at.
func ParseUserAgent(raw string) UserAgent {
	if strings.TrimSpace(raw) == "" {
		// No header at all. Left unflagged rather than assumed hostile:
		// the honest statement is "not classified", and a reader who
		// cares can select on browser = '' explicitly.
		return UserAgent{}
	}

	ua := UserAgent{
		Browser: firstMatch(raw, browserMarkers),
		OS:      firstMatch(raw, osMarkers),
		IsBot:   looksLikeBot(raw),
	}
	ua.Device = classifyDevice(raw, ua)
	return ua
}

func firstMatch(raw string, markers []struct{ marker, name string }) string {
	for _, m := range markers {
		if strings.Contains(raw, m.marker) {
			return m.name
		}
	}
	return ""
}

func looksLikeBot(raw string) bool {
	lower := strings.ToLower(raw)
	for _, name := range botNames {
		if strings.Contains(lower, name) {
			return true
		}
	}
	for _, token := range tokenize(lower) {
		if isBotToken(token) {
			return true
		}
	}
	return false
}

// isBotToken reports whether one alphanumeric token names a crawler.
func isBotToken(token string) bool {
	if notBotTokens[token] {
		return false
	}
	for _, suffix := range botTokenSuffixes {
		if strings.HasSuffix(token, suffix) {
			return true
		}
	}
	return false
}

// tokenize splits on every non-alphanumeric character, which is what
// turns "Mozilla/5.0 (compatible; Googlebot/2.1; +http://...)" into the
// bare token "googlebot". Version numbers, slashes, semicolons and
// parentheses all vary between crawlers; the name does not.
func tokenize(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
}

// classifyDevice picks a form factor.
//
// The Android rule is the standard heuristic and worth stating: Android
// *phones* include the token "Mobile" and Android *tablets* omit it.
// That is a convention Google documents for exactly this purpose, not
// an accident, and there is no more direct signal available.
func classifyDevice(raw string, ua UserAgent) string {
	if ua.IsBot {
		// A crawler has no form factor. Reporting one would put
		// Googlebot in the "desktop" bucket of every device breakdown.
		return ""
	}

	switch {
	case strings.Contains(raw, "iPad"):
		return DeviceTablet
	case strings.Contains(raw, "iPhone"), strings.Contains(raw, "iPod"):
		return DeviceMobile
	case strings.Contains(raw, "Tablet"), strings.Contains(raw, "Kindle"), strings.Contains(raw, "Silk"):
		return DeviceTablet
	case ua.OS == "Android":
		if strings.Contains(raw, "Mobile") {
			return DeviceMobile
		}
		return DeviceTablet
	case strings.Contains(raw, "Mobile"):
		return DeviceMobile
	case ua.OS != "":
		// A recognized desktop OS with no mobile marker.
		return DeviceDesktop
	default:
		return ""
	}
}
