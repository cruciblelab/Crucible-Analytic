package beacon

import (
	"net/url"
	"strings"
	"testing"
)

func applyRaw(t *testing.T, p CampaignPolicy, rawQuery string) (Campaign, string) {
	t.Helper()
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", rawQuery, err)
	}
	return p.Apply(values)
}

func TestCampaignPolicy_SplitsEveryStandardDimension(t *testing.T) {
	c, query := applyRaw(t, DefaultCampaignPolicy(),
		"utm_source=instagram&utm_medium=social&utm_campaign=bahar&utm_term=ayakkabi&utm_content=mavi-buton&ref=blog")

	want := Campaign{
		Source:  "instagram",
		Medium:  "social",
		Name:    "bahar",
		Term:    "ayakkabi",
		Content: "mavi-buton",
		Ref:     "blog",
	}
	if c != want {
		t.Errorf("campaign = %+v, want %+v", c, want)
	}
	// The catch-all string keeps the exact combination too, so "which
	// precise link performed best" stays answerable.
	if query == "" {
		t.Error("the re-serialized query string is empty; the exact combination was lost")
	}
}

// The whole reason typed columns exist: one source spread over several
// campaigns must be groupable as one source.
func TestCampaignPolicy_SameSourceAcrossCampaignsSharesASourceValue(t *testing.T) {
	first, firstQuery := applyRaw(t, DefaultCampaignPolicy(), "utm_source=instagram&utm_campaign=bahar")
	second, secondQuery := applyRaw(t, DefaultCampaignPolicy(), "utm_source=instagram&utm_campaign=kis")

	if first.Source != second.Source {
		t.Errorf("Source differs across campaigns: %q vs %q", first.Source, second.Source)
	}
	if firstQuery == secondQuery {
		t.Error("the catch-all query string is identical for two different campaigns; it should still distinguish them")
	}
}

func TestCampaignPolicy_OrderDoesNotChangeTheStoredString(t *testing.T) {
	_, a := applyRaw(t, DefaultCampaignPolicy(), "utm_source=x&utm_medium=y")
	_, b := applyRaw(t, DefaultCampaignPolicy(), "utm_medium=y&utm_source=x")
	if a != b {
		t.Errorf("parameter order changed the stored value: %q vs %q", a, b)
	}
}

func TestCampaignPolicy_DropsAnythingNotAllowlisted(t *testing.T) {
	// The reason the allowlist exists: query strings carry credentials.
	c, query := applyRaw(t, DefaultCampaignPolicy(),
		"utm_source=news&reset_token=SECRET&email=a@b.com&session=abc")

	if c.Source != "news" {
		t.Errorf("Source = %q, want news", c.Source)
	}
	for _, leaked := range []string{"SECRET", "a@b.com", "abc", "reset_token", "email", "session"} {
		if strings.Contains(query, leaked) {
			t.Errorf("stored query %q leaked %q", query, leaked)
		}
	}
}

// Click identifiers are the highest-sensitivity thing this package can
// see: unique per click, and resolvable to a person by the network that
// issued them. The default must record which network and not the value.
func TestCampaignPolicy_RecordsNetworkButNotClickIDByDefault(t *testing.T) {
	c, query := applyRaw(t, DefaultCampaignPolicy(), "gclid=EAIaIQobChMI-UNIQUE-PER-CLICK")

	if c.ClickIDSource != ClickSourceGoogle {
		t.Errorf("ClickIDSource = %q, want %q", c.ClickIDSource, ClickSourceGoogle)
	}
	if c.ClickID != "" {
		t.Errorf("ClickID = %q, want empty by default", c.ClickID)
	}
	if strings.Contains(query, "UNIQUE-PER-CLICK") {
		t.Errorf("stored query %q kept the raw click identifier", query)
	}
}

func TestCampaignPolicy_StoresClickIDWhenDeploymentOptsIn(t *testing.T) {
	p := NewCampaignPolicy(nil, nil, true)
	c, query := applyRaw(t, p, "fbclid=ABC123")

	if c.ClickIDSource != ClickSourceFacebook {
		t.Errorf("ClickIDSource = %q, want %q", c.ClickIDSource, ClickSourceFacebook)
	}
	if c.ClickID != "ABC123" {
		t.Errorf("ClickID = %q, want ABC123", c.ClickID)
	}
	if !strings.Contains(query, "ABC123") {
		t.Errorf("stored query %q dropped the opted-in click identifier", query)
	}
}

// A link shared onward after an ad click can carry two identifiers. The
// attribution must not depend on Go's randomized map order.
func TestCampaignPolicy_MultipleClickIDsAttributeDeterministically(t *testing.T) {
	for i := 0; i < 50; i++ {
		c, _ := applyRaw(t, DefaultCampaignPolicy(), "fbclid=b&gclid=a&msclkid=c")
		if c.ClickIDSource != ClickSourceGoogle {
			t.Fatalf("run %d: ClickIDSource = %q, want %q (first in clickIDParams)", i, c.ClickIDSource, ClickSourceGoogle)
		}
	}
}

// The setting that exists specifically so a lawyer's answer about
// utm_term can be applied without a code change.
func TestCampaignPolicy_DropsAConfiguredStandardParameter(t *testing.T) {
	p := NewCampaignPolicy([]string{"utm_term"}, nil, false)
	c, query := applyRaw(t, p, "utm_source=google&utm_term=kirmizi+ayakkabi")

	if c.Source != "google" {
		t.Errorf("Source = %q, want google", c.Source)
	}
	if c.Term != "" {
		t.Errorf("Term = %q, want empty once dropped", c.Term)
	}
	if strings.Contains(query, "ayakkabi") || strings.Contains(query, "utm_term") {
		t.Errorf("stored query %q kept a dropped parameter", query)
	}
}

func TestCampaignPolicy_KeepsConfiguredExtraParameters(t *testing.T) {
	p := NewCampaignPolicy(nil, []string{"partner"}, false)
	_, query := applyRaw(t, p, "partner=acme&unlisted=nope")

	if !strings.Contains(query, "partner") || !strings.Contains(query, "acme") {
		t.Errorf("stored query %q dropped a configured extra parameter", query)
	}
	if strings.Contains(query, "unlisted") {
		t.Errorf("stored query %q kept an unlisted parameter", query)
	}
}

func TestCampaignPolicy_ZeroValueBehavesLikeTheDefault(t *testing.T) {
	// A Server or test that never sets a policy must keep the standard
	// parameters rather than silently storing nothing.
	zero, zeroQuery := applyRaw(t, CampaignPolicy{}, "utm_source=x&gclid=y")
	def, defQuery := applyRaw(t, DefaultCampaignPolicy(), "utm_source=x&gclid=y")

	if zero != def || zeroQuery != defQuery {
		t.Errorf("zero value differs from default: %+v/%q vs %+v/%q", zero, zeroQuery, def, defQuery)
	}
}

func TestCampaignPolicy_SanitizesAndBoundsValues(t *testing.T) {
	long := ""
	for i := 0; i < maxCampaignValueLen+50; i++ {
		long += "a"
	}
	c, _ := applyRaw(t, DefaultCampaignPolicy(), "utm_source="+long)
	if len([]rune(c.Source)) > maxCampaignValueLen {
		t.Errorf("Source is %d runes, want at most %d", len([]rune(c.Source)), maxCampaignValueLen)
	}

	// A NUL byte would make PostgreSQL reject the whole COPY batch, so
	// one hostile link must not be able to take every other visitor's
	// events down with it.
	values := url.Values{"utm_source": []string{"bad\x00value"}}
	hostile, _ := DefaultCampaignPolicy().Apply(values)
	if strings.Contains(hostile.Source, "\x00") {
		t.Error("a NUL byte survived into a campaign value")
	}
}

func TestCampaignPolicy_EmptyReportsNoAcquisitionContext(t *testing.T) {
	c, query := applyRaw(t, DefaultCampaignPolicy(), "")
	if !c.Empty() {
		t.Errorf("Empty() = false for %+v", c)
	}
	if query != "" {
		t.Errorf("query = %q, want empty", query)
	}
}

func TestCampaignPolicy_RepeatedParameterKeepsOnlyTheFirst(t *testing.T) {
	c, _ := applyRaw(t, DefaultCampaignPolicy(), "utm_source=first&utm_source=second")
	if c.Source != "first" {
		t.Errorf("Source = %q, want first", c.Source)
	}
}

// Extra parameters are matched against what a browser actually sent, and
// query-string keys are case-sensitive. An earlier version folded the
// configured name to lower case, so `extra_params = ["Partner"]` against
// a site emitting "?Partner=" matched nothing at all - no error, no log
// line, the parameter simply never appeared. That is the worst way for a
// configuration option to fail, so it has a test.
func TestCampaignPolicy_ExtraParametersMatchTheCaseTheSiteSends(t *testing.T) {
	cases := map[string]struct {
		configured string
		sent       string
		wantKept   bool
	}{
		"exact match, capitalised": {"Partner", "Partner", true},
		"exact match, lower case":  {"partner", "partner", true},
		"case differs":             {"partner", "Partner", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			p := NewCampaignPolicy(nil, []string{tc.configured}, false)
			_, query := p.Apply(url.Values{tc.sent: []string{"acme"}})
			if kept := strings.Contains(query, "acme"); kept != tc.wantKept {
				t.Errorf("configured %q, site sent %q: kept=%v want %v (query %q)",
					tc.configured, tc.sent, kept, tc.wantKept, query)
			}
		})
	}
}

// Standard names are canonically lower case, so a capitalised entry in
// drop_params must still work.
func TestCampaignPolicy_DropParamsIgnoresConfiguredCase(t *testing.T) {
	p := NewCampaignPolicy([]string{"UTM_Term", "  utm_content  "}, nil, false)
	c, _ := applyRaw(t, p, "utm_term=x&utm_content=y&utm_source=keep")
	if c.Term != "" {
		t.Errorf("Term = %q, want empty (UTM_Term should drop utm_term)", c.Term)
	}
	if c.Content != "" {
		t.Errorf("Content = %q, want empty (whitespace should be trimmed)", c.Content)
	}
	if c.Source != "keep" {
		t.Errorf("Source = %q, want keep", c.Source)
	}
}
