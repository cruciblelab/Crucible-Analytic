package beacon

import (
	"net/netip"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestEvent_ValidateAcceptsBothTypes(t *testing.T) {
	if err := (Event{Site: "s", Type: TypePageview}).Validate(); err != nil {
		t.Fatalf("pageview rejected: %v", err)
	}
	if err := (Event{Site: "s", Type: TypeEvent, Name: "signup"}).Validate(); err != nil {
		t.Fatalf("named event rejected: %v", err)
	}
}

func TestEvent_ValidateRejectsBadInput(t *testing.T) {
	cases := map[string]Event{
		"unknown type":          {Site: "s", Type: "click"},
		"empty type":            {Site: "s"},
		"event with no name":    {Site: "s", Type: TypeEvent},
		"event with blank name": {Site: "s", Type: TypeEvent, Name: "   "},
		"no site":               {Type: TypePageview},
	}
	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			if err := event.Validate(); err == nil {
				t.Fatalf("Validate() = nil, want an error for %+v", event)
			}
		})
	}
}

func TestBuildRow_SplitsPathFromQueryAndKeepsOnlyCampaignParams(t *testing.T) {
	row := BuildRow(Event{
		Site: "s",
		Type: TypePageview,
		// session_token is exactly the kind of parameter that must not
		// reach an analytics table: it is a credential, and the panel
		// has a far wider audience than the application's own database.
		URL: "/checkout?utm_source=newsletter&session_token=secret&utm_medium=email",
	}, Enrichment{}, DefaultCampaignPolicy())

	if row.Path != "/checkout" {
		t.Errorf("Path = %q, want /checkout", row.Path)
	}
	if strings.Contains(row.Query, "secret") || strings.Contains(row.Query, "session_token") {
		t.Fatalf("Query = %q, leaked a non-campaign parameter", row.Query)
	}
	// Sorted, so the same visit spelled with the parameters in either
	// order is one row rather than two.
	if row.Query != "utm_medium=email&utm_source=newsletter" {
		t.Errorf("Query = %q, want utm_medium=email&utm_source=newsletter", row.Query)
	}
}

func TestBuildRow_QueryOrderDoesNotChangeTheStoredValue(t *testing.T) {
	first := BuildRow(Event{Site: "s", Type: TypePageview, URL: "/p?utm_source=a&utm_campaign=b"}, Enrichment{}, DefaultCampaignPolicy())
	second := BuildRow(Event{Site: "s", Type: TypePageview, URL: "/p?utm_campaign=b&utm_source=a"}, Enrichment{}, DefaultCampaignPolicy())
	if first.Query != second.Query {
		t.Errorf("query serialization is order-dependent: %q vs %q", first.Query, second.Query)
	}
}

func TestBuildRow_AbsoluteURLKeepsOnlyThePath(t *testing.T) {
	row := BuildRow(Event{Site: "s", Type: TypePageview, URL: "https://somewhere-else.example/admin?x=1"}, Enrichment{}, DefaultCampaignPolicy())
	if row.Path != "/admin" {
		t.Errorf("Path = %q, want /admin", row.Path)
	}
	if strings.Contains(row.Path, "somewhere-else") {
		t.Errorf("Path = %q, kept a client-supplied host", row.Path)
	}
}

func TestBuildRow_NormalizesPaths(t *testing.T) {
	cases := map[string]string{
		"":        "/",
		"/":       "/",
		"pricing": "/pricing",
		"/about":  "/about",
	}
	for in, want := range cases {
		if got := BuildRow(Event{Site: "s", Type: TypePageview, URL: in}, Enrichment{}, DefaultCampaignPolicy()).Path; got != want {
			t.Errorf("URL %q -> Path %q, want %q", in, got, want)
		}
	}
}

func TestBuildRow_SplitsReferrerAndDropsItsQuery(t *testing.T) {
	row := BuildRow(Event{
		Site:     "s",
		Type:     TypePageview,
		URL:      "/",
		Referrer: "https://WWW.Google.com/search?q=private+search+terms",
	}, Enrichment{}, DefaultCampaignPolicy())

	if row.ReferrerHost != "www.google.com" {
		t.Errorf("ReferrerHost = %q, want www.google.com (lowercased)", row.ReferrerHost)
	}
	if row.ReferrerPath != "/search" {
		t.Errorf("ReferrerPath = %q, want /search", row.ReferrerPath)
	}
	if strings.Contains(row.ReferrerPath, "private") {
		t.Errorf("ReferrerPath = %q, kept the referrer's query string", row.ReferrerPath)
	}
}

func TestBuildRow_ReferrerWithoutAHostIsDropped(t *testing.T) {
	for _, referrer := range []string{"/internal/page", "not a url at all", ""} {
		row := BuildRow(Event{Site: "s", Type: TypePageview, URL: "/", Referrer: referrer}, Enrichment{}, DefaultCampaignPolicy())
		if row.ReferrerHost != "" {
			t.Errorf("referrer %q -> host %q, want empty", referrer, row.ReferrerHost)
		}
	}
}

func TestBuildRow_NameOnlyKeptForNamedEvents(t *testing.T) {
	pageview := BuildRow(Event{Site: "s", Type: TypePageview, URL: "/", Name: "leftover"}, Enrichment{}, DefaultCampaignPolicy())
	if pageview.EventName != "" {
		t.Errorf("EventName = %q on a pageview, want empty", pageview.EventName)
	}
	named := BuildRow(Event{Site: "s", Type: TypeEvent, URL: "/", Name: "signup"}, Enrichment{}, DefaultCampaignPolicy())
	if named.EventName != "signup" {
		t.Errorf("EventName = %q, want signup", named.EventName)
	}
}

func TestBuildRow_ClampsScreenDimensions(t *testing.T) {
	row := BuildRow(Event{Site: "s", Type: TypePageview, URL: "/", ScreenW: -5, ScreenH: 1 << 30}, Enrichment{}, DefaultCampaignPolicy())
	if row.ScreenW != 0 {
		t.Errorf("ScreenW = %d, want 0 for a negative input", row.ScreenW)
	}
	if row.ScreenH != maxScreenPx {
		t.Errorf("ScreenH = %d, want it clamped to %d", row.ScreenH, maxScreenPx)
	}
}

func TestBuildRow_CarriesEnrichmentThrough(t *testing.T) {
	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	ip := netip.MustParseAddr("203.0.113.9")
	row := BuildRow(Event{Site: "acme", Type: TypePageview, URL: "/"}, Enrichment{
		Time:      now,
		IP:        ip,
		VisitorID: "abc123",
		UserAgent: UserAgent{Browser: "Firefox", OS: "Linux", Device: DeviceDesktop, IsBot: true},
		Country:   "TR",
		ASN:       15169,
		ASNOrg:    "GOOGLE",
	}, DefaultCampaignPolicy())

	if !row.Time.Equal(now) || row.IP != ip || row.VisitorID != "abc123" {
		t.Errorf("enrichment identity fields lost: %+v", row)
	}
	if row.Browser != "Firefox" || row.OS != "Linux" || row.Device != DeviceDesktop || !row.IsBotUA {
		t.Errorf("user agent fields lost: %+v", row)
	}
	if row.Country != "TR" || row.ASN != 15169 || row.ASNOrg != "GOOGLE" {
		t.Errorf("geo fields lost: %+v", row)
	}
	if row.SiteID != "acme" {
		t.Errorf("SiteID = %q, want acme", row.SiteID)
	}
}

// The batch writer sends many rows in one COPY. PostgreSQL rejects a
// NUL byte or invalid UTF-8 in a TEXT column outright, failing the whole
// statement - so without this stripping, a single hostile payload would
// destroy every other visitor's events that happened to share its batch.
func TestSanitizeText_RemovesWhatPostgresCannotStore(t *testing.T) {
	cases := map[string]struct{ in, want string }{
		"nul byte":          {"before\x00after", "beforeafter"},
		"newline":           {"a\nb", "ab"},
		"carriage return":   {"a\rb", "ab"},
		"tab":               {"a\tb", "ab"},
		"delete":            {"a\x7fb", "ab"},
		"line separator":    {"a b", "ab"},
		"surrounding space": {"  hello  ", "hello"},
		"kept as-is":        {"Fiyatlar — Ürünler", "Fiyatlar — Ürünler"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := sanitizeText(tc.in, 100); got != tc.want {
				t.Errorf("sanitizeText(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeText_RepairsInvalidUTF8(t *testing.T) {
	// A lone continuation byte: not valid UTF-8 on its own, and enough
	// to make PostgreSQL reject the statement carrying it.
	got := sanitizeText("ok\xffbad", 100)
	if !utf8.ValidString(got) {
		t.Fatalf("sanitizeText produced invalid UTF-8: %q", got)
	}
	if got != "okbad" {
		t.Errorf("sanitizeText = %q, want okbad", got)
	}
}

func TestSanitizeText_TruncatesWithoutSplittingRunes(t *testing.T) {
	// Every rune here is multi-byte, so a byte-wise cut would leave a
	// partial sequence - i.e. exactly the invalid UTF-8 the function
	// just finished removing.
	got := sanitizeText(strings.Repeat("ş", 50), 10)
	if utf8.RuneCountInString(got) != 10 {
		t.Errorf("kept %d runes, want 10", utf8.RuneCountInString(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncation produced invalid UTF-8: %q", got)
	}
}

func TestBuildRow_TruncatesOverlongFields(t *testing.T) {
	long := strings.Repeat("x", 5000)
	row := BuildRow(Event{
		Site: "s", Type: TypeEvent, Name: long,
		URL: "/" + long, Title: long, Language: long,
		Referrer: "https://" + strings.Repeat("h", 400) + ".example/" + long,
	}, Enrichment{}, DefaultCampaignPolicy())

	for field, spec := range map[string]struct {
		got string
		max int
	}{
		"EventName":    {row.EventName, maxNameLen},
		"Path":         {row.Path, maxPathLen},
		"Title":        {row.Title, maxTitleLen},
		"Language":     {row.Language, maxLanguageLen},
		"ReferrerHost": {row.ReferrerHost, maxHostLen},
		"ReferrerPath": {row.ReferrerPath, maxPathLen},
	} {
		if n := utf8.RuneCountInString(spec.got); n > spec.max {
			t.Errorf("%s kept %d runes, want at most %d", field, n, spec.max)
		}
	}
}

// A path without its slash is given one, and the cap still holds.
//
// This test used to allow maxPathLen+1 for Path, "for the normalizing
// leading slash" - it had written the defect down as the contract, while
// the fuzz target beside it held the field to maxPathLen. The nightly
// fuzzer found the disagreement on 2026-09-29: a 1024-rune path without
// a slash, cut and then prefixed, came out at 1025. Both spellings are
// asked here because only the second ever reached the prefix.
func TestBuildRow_APathIsCutAfterItsSlashIsAdded(t *testing.T) {
	long := strings.Repeat("ğ", maxPathLen+10)
	for _, raw := range []string{"/" + long, long, "/" + long[:len("ğ")*(maxPathLen-1)], long[:len("ğ")*maxPathLen]} {
		row := BuildRow(Event{Site: "s", Type: TypePageview, URL: raw}, Enrichment{}, DefaultCampaignPolicy())
		if !strings.HasPrefix(row.Path, "/") {
			t.Errorf("a %d-rune URL stored the path %.20q..., without its slash",
				utf8.RuneCountInString(raw), row.Path)
		}
		if n := utf8.RuneCountInString(row.Path); n != maxPathLen {
			t.Errorf("a %d-rune URL stored a %d-rune path, want exactly %d - the cap, and "+
				"nothing of the path dropped that would have fitted",
				utf8.RuneCountInString(raw), n, maxPathLen)
		}
	}
}

// truncateBytes cuts on a rune boundary at or below the bound, and keeps
// everything that fits.
//
// The cases are the boundary itself: a value exactly at the bound, one
// byte over it where the last rune would be split, and a rune that ends
// exactly at the bound. A cut a byte early passes "at most" and fails
// "everything that fits", which is why both are asked.
func TestTruncateBytes_CutsOnARuneBoundary(t *testing.T) {
	for _, c := range []struct {
		in   string
		max  int
		want string
	}{
		{"abcd", 4, "abcd"},
		{"abcd", 3, "abc"},
		{"abğ", 4, "abğ"}, // 2 + 2 bytes: ends exactly at the bound
		{"abğ", 3, "ab"},  // the ğ would be split
		{"a𠀀", 4, "a"},    // a four-byte rune, three bytes left
		{"a𠀀", 5, "a𠀀"},   // and exactly enough
		{"𠀀𠀀", 7, "𠀀"},    // the second would need eight
		{"", 0, ""},
		{"ğ", 1, ""},
	} {
		got := truncateBytes(c.in, c.max)
		if got != c.want {
			t.Errorf("truncateBytes(%q, %d) = %q, want %q", c.in, c.max, got, c.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("truncateBytes(%q, %d) = %q, which is not valid UTF-8", c.in, c.max, got)
		}
	}
}
