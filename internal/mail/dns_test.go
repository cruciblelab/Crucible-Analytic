package mail

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
)

// stubResolver answers from a map. Real DNS is not usable in a test:
// the answers change under it, and the cases worth checking - a domain
// with no DMARC, a resolver that is down - cannot be arranged on demand.
//
// Unlike the SMTP server, there is nothing here worth speaking the real
// protocol for. This package does not parse DNS; it parses a string that
// DNS handed it, and that string is what the stub provides.
type stubResolver struct {
	records map[string][]string
	err     error
}

func (s stubResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	records, ok := s.records[name]
	if !ok {
		// What the system resolver returns for a name with no records,
		// reproduced so the "no record" path is exercised as it will
		// actually be reached.
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	return records, nil
}

func useResolver(t *testing.T, r resolver) {
	t.Helper()
	previous := defaultResolver
	defaultResolver = r
	t.Cleanup(func() { defaultResolver = previous })
}

func TestLookupSPF(t *testing.T) {
	tests := []struct {
		name          string
		records       []string
		wantFound     bool
		wantQualifier string
	}{
		{
			name:          "reject policy",
			records:       []string{"v=spf1 include:_spf.google.com -all"},
			wantFound:     true,
			wantQualifier: "-all",
		},
		{
			name:          "soft fail",
			records:       []string{"v=spf1 mx ~all"},
			wantFound:     true,
			wantQualifier: "~all",
		},
		{
			name:          "no qualifier at the end",
			records:       []string{"v=spf1 include:_spf.example.com"},
			wantFound:     true,
			wantQualifier: "",
		},
		{
			// A redirect modifier can end in the letters "all" without
			// being the all mechanism. A suffix match read this as a
			// policy the domain never declared.
			name:          "redirect that ends in all",
			records:       []string{"v=spf1 redirect=_spf.example.small"},
			wantFound:     true,
			wantQualifier: "",
		},
		{
			// TXT records at a domain are a mixed bag - verification
			// tokens, site ownership proofs. The SPF one has to be picked
			// out rather than assumed to be the only one.
			name: "alongside other TXT records",
			records: []string{
				"google-site-verification=abc123",
				"v=spf1 a mx -all",
				"MS=ms12345678",
			},
			wantFound:     true,
			wantQualifier: "-all",
		},
		{
			name:      "TXT records but no SPF",
			records:   []string{"google-site-verification=abc123"},
			wantFound: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			useResolver(t, stubResolver{records: map[string][]string{"ornek.com": tc.records}})

			got := LookupSPF(context.Background(), "ornek.com")
			if got.Err != nil {
				t.Fatalf("unexpected error: %v", got.Err)
			}
			if got.Found != tc.wantFound {
				t.Errorf("found = %v, want %v", got.Found, tc.wantFound)
			}
			if got.AllQualifier != tc.wantQualifier {
				t.Errorf("qualifier = %q, want %q", got.AllQualifier, tc.wantQualifier)
			}
		})
	}
}

// A domain with no TXT records at all is a domain with no SPF record -
// not a broken resolver. Getting this wrong tells an operator to go and
// look at their server's networking when what they need is to add a
// record.
func TestLookupSPFNoRecordsIsNotAnError(t *testing.T) {
	useResolver(t, stubResolver{records: map[string][]string{}})

	got := LookupSPF(context.Background(), "ornek.com")
	if got.Err != nil {
		t.Errorf("err = %v, want nil for a domain with no TXT records", got.Err)
	}
	if got.Found {
		t.Error("found = true for a domain with no TXT records")
	}
}

// And a resolver that genuinely could not answer must not come back as
// "no record" either, which would tell the same operator to add a record
// they already have.
func TestLookupResolverFailureIsReported(t *testing.T) {
	broken := errors.New("dial udp 127.0.0.53:53: connection refused")
	useResolver(t, stubResolver{err: broken})

	spf := LookupSPF(context.Background(), "ornek.com")
	if !errors.Is(spf.Err, broken) {
		t.Errorf("SPF err = %v, want the resolver's error", spf.Err)
	}
	if spf.Found {
		t.Error("SPF found = true after a resolver failure")
	}

	dmarc := LookupDMARC(context.Background(), "ornek.com")
	if !errors.Is(dmarc.Err, broken) {
		t.Errorf("DMARC err = %v, want the resolver's error", dmarc.Err)
	}
}

// A wrapped NXDOMAIN must still read as "no record". The first version
// used a type assertion, which sees through no wrapping at all.
func TestLookupWrappedNotFoundIsNotAnError(t *testing.T) {
	wrapped := fmt.Errorf("resolving: %w", &net.DNSError{Err: "no such host", IsNotFound: true})
	useResolver(t, stubResolver{err: wrapped})

	if got := LookupSPF(context.Background(), "ornek.com"); got.Err != nil || got.Found {
		t.Errorf("SPF = {found:%v err:%v}, want a clean absence", got.Found, got.Err)
	}
	if got := LookupDMARC(context.Background(), "ornek.com"); got.Err != nil || got.Found {
		t.Errorf("DMARC = {found:%v err:%v}, want a clean absence", got.Found, got.Err)
	}
}

func TestLookupDMARC(t *testing.T) {
	tests := []struct {
		name       string
		records    map[string][]string
		wantFound  bool
		wantPolicy string
	}{
		{
			name:       "reject",
			records:    map[string][]string{"_dmarc.ornek.com": {"v=DMARC1; p=reject; rua=mailto:d@ornek.com"}},
			wantFound:  true,
			wantPolicy: "reject",
		},
		{
			name:       "quarantine with spacing",
			records:    map[string][]string{"_dmarc.ornek.com": {"v=DMARC1;  p=quarantine ; pct=100"}},
			wantFound:  true,
			wantPolicy: "quarantine",
		},
		{
			name:       "none",
			records:    map[string][]string{"_dmarc.ornek.com": {"v=DMARC1; p=none"}},
			wantFound:  true,
			wantPolicy: "none",
		},
		{
			// The record lives at _dmarc.<domain>, never on the domain
			// itself. Looking at the wrong name reports every correctly
			// configured domain as unconfigured.
			name:      "record on the domain itself does not count",
			records:   map[string][]string{"ornek.com": {"v=DMARC1; p=reject"}},
			wantFound: false,
		},
		{
			name:      "no record",
			records:   map[string][]string{},
			wantFound: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			useResolver(t, stubResolver{records: tc.records})

			got := LookupDMARC(context.Background(), "ornek.com")
			if got.Err != nil {
				t.Fatalf("unexpected error: %v", got.Err)
			}
			if got.Found != tc.wantFound {
				t.Errorf("found = %v, want %v", got.Found, tc.wantFound)
			}
			if got.Policy != tc.wantPolicy {
				t.Errorf("policy = %q, want %q", got.Policy, tc.wantPolicy)
			}
		})
	}
}

func TestDomainOf(t *testing.T) {
	tests := []struct{ in, want string }{
		{"panel@ornek.com", "ornek.com"},
		{"panel@ORNEK.COM", "ornek.com"},
		{"a@b@ornek.com", "ornek.com"},
		{"panel", ""},
		{"panel@", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := DomainOf(tc.in); got != tc.want {
			t.Errorf("DomainOf(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
