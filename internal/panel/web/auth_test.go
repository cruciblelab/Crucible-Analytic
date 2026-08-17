package web

import (
	"testing"
	"time"
)

// TestSafeNextRefusesEverythingOffThisPanel is the test for the one
// parameter on the sign-in page that an attacker controls.
//
// A login form that will redirect anywhere is a phishing springboard
// wearing the customer's own domain: the address bar shows this panel
// right up until the credentials are typed somewhere else. The
// interesting cases are not "http://evil.test" - everybody catches that
// - but the forms that look relative and are not.
func TestSafeNextRefusesEverythingOffThisPanel(t *testing.T) {
	s := &Server{}

	refused := []struct {
		in  string
		why string
	}{
		{"//evil.test/", "scheme-relative: the browser reads this as another host"},
		{"//evil.test", "scheme-relative with no trailing slash"},
		{`/\evil.test`, `backslash form - browsers normalise \ to / and this is the one hand-rolled checks miss`},
		{`/\/evil.test`, "mixed slash and backslash"},
		{"https://evil.test/", "absolute"},
		{"http://evil.test/", "absolute"},
		{"javascript:alert(1)", "a scheme that is not navigation at all"},
		{"data:text/html,<script>alert(1)</script>", "data URL"},
		{"evil.test/path", "no leading slash: resolves against the current directory"},
		{"", "empty"},
		{LoginPath, "the sign-in form itself - a loop"},
		{SecondFactorPath, "the code form - a loop"},
		{SetupPathPrefix + "baslangic", "the developer wizard, which this session does not open"},
		{DevAccessPathPrefix + "abc", "a one-time link, which is not a destination"},
	}
	for _, tc := range refused {
		if got := s.rawNext(tc.in); got != "" {
			t.Errorf("rawNext(%q) = %q, want refused (%s)", tc.in, got, tc.why)
		}
		// safeNext must land somewhere usable rather than passing the
		// bad value through with a warning.
		if got := s.safeNext(tc.in); got != "/" {
			t.Errorf("safeNext(%q) = %q, want the site list", tc.in, got)
		}
	}

	accepted := []struct{ in, want string }{
		{"/hesap", "/hesap"},
		{"/site/ornek/uyeler", "/site/ornek/uyeler"},
		{"/rapor?gun=7", "/rapor?gun=7"},
		{"/yol/alt#bolum", "/yol/alt#bolum"},
	}
	for _, tc := range accepted {
		if got := s.rawNext(tc.in); got != tc.want {
			t.Errorf("rawNext(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// "/" is dropped rather than carried: it is where signing in leads
	// anyway, so echoing it is noise in the address bar.
	if got := s.rawNext("/"); got != "" {
		t.Errorf(`rawNext("/") = %q, want it dropped`, got)
	}
	if got := s.safeNext("/"); got != "/" {
		t.Errorf(`safeNext("/") = %q`, got)
	}
}

func TestWithNextOnlyAppendsWhenThereIsSomewhereToGo(t *testing.T) {
	if got := withNext(LoginPath, ""); got != LoginPath {
		t.Errorf("withNext with no destination = %q, want a bare path", got)
	}
	if got := withNext(LoginPath, "/hesap"); got != "/giris?next=%2Fhesap" {
		t.Errorf("withNext = %q", got)
	}
}

// TestRetryMinutesNeverPromisesZero: "try again in 0 minutes" reads as a
// bug, and rounding down would tell somebody to retry before the window
// has actually passed - which produces a second refusal and the belief
// that the panel is broken.
func TestRetryMinutesNeverPromisesZero(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want int
	}{
		{0, 1},
		{-time.Minute, 1},
		{time.Second, 1},
		{time.Minute, 1},
		{time.Minute + time.Second, 2},
		{14 * time.Minute, 14},
		{14*time.Minute + 30*time.Second, 15},
	}
	for _, tc := range cases {
		if got := retryMinutes(tc.in); got != tc.want {
			t.Errorf("retryMinutes(%s) = %d, want %d", tc.in, got, tc.want)
		}
	}
}
