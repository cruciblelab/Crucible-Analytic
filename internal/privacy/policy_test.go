package privacy

import (
	"errors"
	"strings"
	"testing"
)

// What these two values are, and why the table is mostly refusals.
//
// They are the only strings in this product that an operator types and a
// stranger's browser then renders - the policy address lands in an href
// on a page served to the public, and both reach a customer's own page
// through the JSON endpoint, where nothing of ours escapes anything.
//
// So the cases below are not "does it parse". They are the shapes a
// wrong or hostile value arrives in: another scheme, a password in the
// URL, a newline that splits a header, an address with no host. Each one
// has to be refused for a reason somebody can act on, which is why the
// messages are Turkish and why this test reads them.

func TestCleanPolicyURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" with wantErr false means ErrEmpty
		bad  bool
	}{
		{name: "an ordinary page", in: "https://acme.example/gizlilik", want: "https://acme.example/gizlilik"},
		{name: "plain http is allowed", in: "http://acme.example/gizlilik", want: "http://acme.example/gizlilik"},
		{name: "surrounding space is trimmed", in: "  https://acme.example/p  ", want: "https://acme.example/p"},
		{name: "a query survives", in: "https://acme.example/p?dil=tr", want: "https://acme.example/p?dil=tr"},
		{name: "unicode in the path survives", in: "https://acme.example/gizlilik-politikamız", want: "https://acme.example/gizlilik-politikam%C4%B1z"},

		// The refusals.
		{name: "javascript", in: "javascript:alert(1)", bad: true},
		{name: "javascript in mixed case", in: "JavaScript:alert(1)", bad: true},
		{name: "a data url", in: "data:text/html,<script>alert(1)</script>", bad: true},
		{name: "a file url", in: "file:///etc/passwd", bad: true},
		{name: "no scheme", in: "acme.example/gizlilik", bad: true},
		{name: "a bare path", in: "/gizlilik", bad: true},
		{name: "no host", in: "https:///gizlilik", bad: true},
		{name: "credentials in the url", in: "https://ali:parola@acme.example/p", bad: true},
		{name: "a newline", in: "https://acme.example/p\nSet-Cookie: a=b", bad: true},
		{name: "a carriage return", in: "https://acme.example/p\rX", bad: true},
		{name: "a control character", in: "https://acme.example/p\x00", bad: true},
		{name: "invalid utf-8", in: "https://acme.example/\xc0", bad: true},
		{name: "far too long", in: "https://acme.example/" + strings.Repeat("a", MaxPolicyURL), bad: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CleanPolicyURL(tc.in)
			switch {
			case tc.bad:
				if err == nil {
					t.Fatalf("CleanPolicyURL(%q) accepted it and returned %q", tc.in, got)
				}
				if errors.Is(err, ErrEmpty) {
					t.Fatalf("CleanPolicyURL(%q) reported it as empty rather than refused", tc.in)
				}
				// The message is printed under a form field, so it has to
				// be a sentence rather than a package name.
				if strings.Contains(err.Error(), "privacy:") || len(err.Error()) < 10 {
					t.Errorf("the refusal reads %q, which is not something a customer can act on", err)
				}
			case err != nil:
				t.Fatalf("CleanPolicyURL(%q): %v", tc.in, err)
			case got != tc.want:
				t.Errorf("CleanPolicyURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestAnEmptyValueIsNotAnError.
//
// Every deployment starts here, and the panel shows the message a check
// returns. A blank field that reported a fault would train its reader to
// ignore faults.
func TestAnEmptyValueIsNotAnError(t *testing.T) {
	for _, in := range []string{"", "   ", "\t"} {
		for name, clean := range map[string]func(string) (string, error){
			"CleanPolicyURL": CleanPolicyURL,
			"CleanContact":   CleanContact,
		} {
			got, err := clean(in)
			if !errors.Is(err, ErrEmpty) {
				t.Errorf("%s(%q) returned %q, %v; want ErrEmpty", name, in, got, err)
			}
		}
	}
}

func TestCleanContact(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		bad  bool
	}{
		{name: "an address", in: "gizlilik@acme.example", want: "gizlilik@acme.example"},
		{name: "an address with a plus", in: "gizlilik+kvkk@acme.example", want: "gizlilik+kvkk@acme.example"},
		{name: "a mailto is unwrapped", in: "mailto:gizlilik@acme.example", want: "gizlilik@acme.example"},
		{name: "a form page", in: "https://acme.example/iletisim", want: "https://acme.example/iletisim"},
		{name: "space is trimmed", in: "  gizlilik@acme.example ", want: "gizlilik@acme.example"},

		{name: "no at sign", in: "gizlilik.acme.example", bad: true},
		{name: "two at signs", in: "a@b@acme.example", bad: true},
		{name: "no domain", in: "gizlilik@", bad: true},
		{name: "no mailbox", in: "@acme.example", bad: true},
		{name: "a domain with no dot", in: "gizlilik@localhost", bad: true},
		{name: "a display name", in: "Acme <gizlilik@acme.example>", bad: true},
		{name: "two addresses", in: "a@acme.example, b@acme.example", bad: true},
		{name: "javascript", in: "javascript:alert(1)", bad: true},
		{name: "a javascript mailto", in: "mailto:javascript:alert(1)", bad: true},
		{name: "a newline", in: "gizlilik@acme.example\nBcc: kurban@example", bad: true},
		{name: "far too long", in: strings.Repeat("a", MaxContact) + "@acme.example", bad: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CleanContact(tc.in)
			switch {
			case tc.bad:
				if err == nil {
					t.Fatalf("CleanContact(%q) accepted it and returned %q", tc.in, got)
				}
			case err != nil:
				t.Fatalf("CleanContact(%q): %v", tc.in, err)
			case got != tc.want:
				t.Errorf("CleanContact(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestTheShownFormsDropWhatTheyCannotVouchFor.
//
// The renderer has nobody to show an error to, so its rule is different:
// a value that fails becomes nothing at all. This is P3's own criterion
// - *bozuk bağlantı göstermiyor* - and it is the behaviour that protects
// a deployment whose settings row was written before these checks
// existed, or edited by hand afterwards.
func TestTheShownFormsDropWhatTheyCannotVouchFor(t *testing.T) {
	for _, bad := range []string{
		"javascript:alert(1)", "data:text/html,x", "/gizlilik", "acme.example",
		"https://ali:parola@acme.example/p", "https://acme.example/p\nX: y",
	} {
		if got := ShownPolicyURL(bad); got != "" {
			t.Errorf("ShownPolicyURL(%q) = %q; a page would have rendered it", bad, got)
		}
	}
	for _, bad := range []string{"javascript:alert(1)", "gizlilik@localhost", "a b@acme.example"} {
		if got := ShownContact(bad); got != "" {
			t.Errorf("ShownContact(%q) = %q; a page would have rendered it", bad, got)
		}
	}

	// And the good ones survive, or the rule above would be "show
	// nothing", which passes every assertion and serves nobody.
	if got := ShownPolicyURL(" https://acme.example/gizlilik "); got != "https://acme.example/gizlilik" {
		t.Errorf("ShownPolicyURL dropped a usable address: %q", got)
	}
	if got := ShownContact("gizlilik@acme.example"); got != "gizlilik@acme.example" {
		t.Errorf("ShownContact dropped a usable address: %q", got)
	}
}

// TestContactIsMailboxTellsTheTwoKindsApart.
//
// The page writes mailto: in front of one of them and not the other, so
// getting this backwards means either a dead link or a mail client
// opening on a web address.
func TestContactIsMailboxTellsTheTwoKindsApart(t *testing.T) {
	if !ContactIsMailbox("gizlilik@acme.example") {
		t.Error("an email address is not recognised as one")
	}
	if ContactIsMailbox("https://acme.example/iletisim") {
		t.Error("a web address is treated as an email address")
	}
	if ContactIsMailbox("") {
		t.Error("an empty contact is treated as an email address")
	}
}
