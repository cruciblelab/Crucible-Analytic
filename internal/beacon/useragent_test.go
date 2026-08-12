package beacon

import "testing"

// Real user agent strings, not invented ones: the entire difficulty of
// this classifier is that browsers impersonate each other, and only
// actual strings exercise that.
func TestParseUserAgent_RealStrings(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want UserAgent
	}{
		{
			"Chrome on Windows",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
			UserAgent{Browser: "Chrome", OS: "Windows", Device: DeviceDesktop},
		},
		{
			// Contains "Chrome" and "Safari" as well as "Edg".
			"Edge on Windows",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0",
			UserAgent{Browser: "Edge", OS: "Windows", Device: DeviceDesktop},
		},
		{
			"Opera on Windows",
			"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 OPR/117.0.0.0",
			UserAgent{Browser: "Opera", OS: "Windows", Device: DeviceDesktop},
		},
		{
			"Firefox on Linux",
			"Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0",
			UserAgent{Browser: "Firefox", OS: "Linux", Device: DeviceDesktop},
		},
		{
			"Safari on macOS",
			"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Safari/605.1.15",
			UserAgent{Browser: "Safari", OS: "macOS", Device: DeviceDesktop},
		},
		{
			"Safari on iPhone",
			"Mozilla/5.0 (iPhone; CPU iPhone OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Mobile/15E148 Safari/604.1",
			UserAgent{Browser: "Safari", OS: "iOS", Device: DeviceMobile},
		},
		{
			// iOS forces every browser onto WebKit, so this says
			// "Safari" too - only CriOS distinguishes it.
			"Chrome on iPhone",
			"Mozilla/5.0 (iPhone; CPU iPhone OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) CriOS/131.0.0.0 Mobile/15E148 Safari/604.1",
			UserAgent{Browser: "Chrome", OS: "iOS", Device: DeviceMobile},
		},
		{
			"Firefox on iPhone",
			"Mozilla/5.0 (iPhone; CPU iPhone OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) FxiOS/133.0 Mobile/15E148 Safari/605.1.15",
			UserAgent{Browser: "Firefox", OS: "iOS", Device: DeviceMobile},
		},
		{
			"Safari on iPad",
			"Mozilla/5.0 (iPad; CPU OS 17_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.6 Mobile/15E148 Safari/604.1",
			UserAgent{Browser: "Safari", OS: "iOS", Device: DeviceTablet},
		},
		{
			// Android phones carry the "Mobile" token...
			"Chrome on an Android phone",
			"Mozilla/5.0 (Linux; Android 14; SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36",
			UserAgent{Browser: "Chrome", OS: "Android", Device: DeviceMobile},
		},
		{
			// ...and Android tablets omit it. That is the only signal.
			"Chrome on an Android tablet",
			"Mozilla/5.0 (Linux; Android 13; SM-X700) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
			UserAgent{Browser: "Chrome", OS: "Android", Device: DeviceTablet},
		},
		{
			"Samsung Internet",
			"Mozilla/5.0 (Linux; Android 13; SAMSUNG SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/23.0 Chrome/115.0.0.0 Mobile Safari/537.36",
			UserAgent{Browser: "Samsung Internet", OS: "Android", Device: DeviceMobile},
		},
		{
			"Chrome on ChromeOS",
			"Mozilla/5.0 (X11; CrOS x86_64 14541.0.0) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36",
			UserAgent{Browser: "Chrome", OS: "ChromeOS", Device: DeviceDesktop},
		},
		{
			// A client that runs JavaScript and is obviously automated:
			// exactly the population the beacon exists to make visible.
			"Headless Chrome",
			"Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/131.0.0.0 Safari/537.36",
			UserAgent{Browser: "Headless Chrome", OS: "Linux", Device: "", IsBot: true},
		},
		{
			"Googlebot",
			"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
			UserAgent{Browser: "", OS: "", Device: "", IsBot: true},
		},
		{
			"Bingbot",
			"Mozilla/5.0 (compatible; bingbot/2.0; +http://www.bing.com/bingbot.htm)",
			UserAgent{IsBot: true},
		},
		{
			"curl",
			"curl/8.5.0",
			UserAgent{IsBot: true},
		},
		{
			"empty",
			"",
			UserAgent{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseUserAgent(tc.raw); got != tc.want {
				t.Errorf("ParseUserAgent()\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// Cubot is a real Android manufacturer whose model names appear in
// millions of genuine mobile user agents. A naive
// strings.Contains(ua, "bot") reports every one of those visitors as a
// crawler - a silent, systematic undercount of real mobile users.
func TestParseUserAgent_PhoneModelsContainingBotAreNotBots(t *testing.T) {
	cases := []string{
		"Mozilla/5.0 (Linux; Android 11; CUBOT NOTE 20) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/95.0.4638.74 Mobile Safari/537.36",
		"Mozilla/5.0 (Linux; Android 12; Cubot X30) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/103.0.0.0 Mobile Safari/537.36",
	}
	for _, raw := range cases {
		ua := ParseUserAgent(raw)
		if ua.IsBot {
			t.Errorf("real phone classified as a bot: %q", raw)
		}
		if ua.Device != DeviceMobile {
			t.Errorf("Device = %q for %q, want mobile", ua.Device, raw)
		}
	}
}

func TestParseUserAgent_BotsHaveNoFormFactor(t *testing.T) {
	// Googlebot's mobile crawler carries the full Android+Mobile
	// string. Without the bot check running first it would land in the
	// "mobile" bucket of every device breakdown.
	ua := ParseUserAgent("Mozilla/5.0 (Linux; Android 6.0.1; Nexus 5X Build/MMB29P) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)")
	if !ua.IsBot {
		t.Fatal("Googlebot's mobile crawler was not recognized as a bot")
	}
	if ua.Device != "" {
		t.Errorf("Device = %q, want empty for a crawler", ua.Device)
	}
}

// The generic rule has to catch crawlers nobody has enumerated -
// vendors glue their name to "bot", so matching the end of a token is
// what makes an unknown "Foobot/1.0" work without a list entry.
func TestIsBotToken(t *testing.T) {
	cases := map[string]bool{
		"googlebot":              true,
		"bingbot":                true,
		"gptbot":                 true,
		"claudebot":              true,
		"applebot":               true,
		"petalbot":               true,
		"bot":                    true,
		"screamingfrogseospider": true,
		"somecrawler":            true,
		// Real devices and real words that merely contain a suffix.
		"cubot":       false,
		"robotics":    false,
		"mozilla":     false,
		"chrome":      false,
		"safari":      false,
		"applewebkit": false,
		"note":        false,
	}
	for token, want := range cases {
		if got := isBotToken(token); got != want {
			t.Errorf("isBotToken(%q) = %v, want %v", token, got, want)
		}
	}
}

func TestTokenize_StripsPunctuationAndVersions(t *testing.T) {
	got := tokenize("mozilla/5.0 (compatible; googlebot/2.1; +http://www.google.com/bot.html)")
	for _, want := range []string{"googlebot", "compatible", "mozilla"} {
		if !contains(got, want) {
			t.Errorf("tokenize did not produce %q; got %v", want, got)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
