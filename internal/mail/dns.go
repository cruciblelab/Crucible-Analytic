package mail

import (
	"context"
	"errors"
	"net"
	"strings"
)

// The DNS side: what the receiving world will check, reported rather
// than enforced.
//
// # Why these never block the wizard
//
// A DNS record added a minute ago is not visible yet, and a wizard that
// says "failed" about one teaches people that its verification is
// unreliable - after which they click past the checks that are reliable.
// So these report what was found and the wizard carries on.
//
// The SMTP connection test is the opposite and does block: it is
// deterministic, it answers in a second, and a wrong answer there means
// nothing will ever be delivered.
//
// # Why there is no DKIM check
//
// The first draft of this phase had one: "does the test message carry a
// signature". It cannot be done. This panel *sends* mail and never
// receives any - it has no mailbox - so it can never read the headers of
// a message it sent.
//
// Checking whether the domain publishes a DKIM record is not available
// either: the selector is chosen by the provider and is not derivable
// from the domain. Guessing a few common ones would report "no DKIM" for
// a correctly configured domain, which is worse than saying nothing.
//
// So DKIM is the provider's job, every real provider does it, and the
// panel says so rather than pretending to check. A check that cannot
// work is worse than an absent one, because somebody believes it.

// SPF is what a domain's SPF record says, as far as this matters here.
type SPF struct {
	// Found is whether any SPF record exists.
	Found bool
	// Record is the raw record, shown as-is: an operator comparing it
	// with their provider's documentation needs the text, not a summary.
	Record string
	// AllQualifier is the policy at the end - "-all" (reject),
	// "~all" (soft fail), "?all" (neutral), or empty when absent.
	AllQualifier string
	Err          error
}

// DMARC is what a domain's DMARC record says.
type DMARC struct {
	Found  bool
	Record string
	// Policy is p= : none, quarantine or reject.
	//
	// This is the one that decides whether Gmail and Yahoo accept bulk
	// mail from the domain at all, after both tightened their rules in
	// 2024. A domain with no DMARC record is the common reason a
	// perfectly working SMTP setup still fails to deliver.
	Policy string
	Err    error
}

// resolver is swapped in tests. The default is the system resolver.
type resolver interface {
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

var defaultResolver resolver = net.DefaultResolver

// lookupTXT reads TXT records, treating "this name has no records" as an
// answer rather than as a failure.
//
// The distinction is the whole point of these functions. A resolver that
// could not answer is a fact about this machine's network; a name with
// nothing on it is a fact about the domain. Reporting the second as the
// first tells an operator their server has a DNS problem when what they
// actually have is a missing record - and they go and look at the wrong
// thing.
func lookupTXT(ctx context.Context, name string) ([]string, error) {
	records, err := defaultResolver.LookupTXT(ctx, name)
	if err == nil {
		return records, nil
	}
	var dnsErr *net.DNSError
	// errors.As rather than a type assertion: a resolver is entitled to
	// wrap, and a bare assertion would silently fall through to the
	// error path for a wrapped NXDOMAIN.
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return nil, nil
	}
	return nil, err
}

// LookupSPF reads the domain's SPF record.
func LookupSPF(ctx context.Context, domain string) SPF {
	var out SPF
	records, err := lookupTXT(ctx, domain)
	if err != nil {
		out.Err = err
		return out
	}
	for _, r := range records {
		if !strings.HasPrefix(strings.ToLower(r), "v=spf1") {
			continue
		}
		out.Found = true
		out.Record = r
		for _, field := range strings.Fields(r) {
			if isAllMechanism(field) {
				out.AllQualifier = field
			}
		}
		break
	}
	return out
}

// isAllMechanism reports whether an SPF term is the "all" mechanism,
// with or without a qualifier.
//
// Exact rather than a suffix match. "redirect=_spf.example.small" ends in
// "all" and is not a policy, and reading it as one would report a
// domain's fallback policy as something nobody wrote.
func isAllMechanism(field string) bool {
	switch strings.ToLower(field) {
	case "all", "+all", "-all", "~all", "?all":
		return true
	}
	return false
}

// LookupDMARC reads the domain's DMARC record, which lives at
// _dmarc.<domain> rather than on the domain itself.
func LookupDMARC(ctx context.Context, domain string) DMARC {
	var out DMARC
	// A missing _dmarc name is an ordinary NXDOMAIN, which lookupTXT
	// already reads as "no record" rather than as a failure.
	records, err := lookupTXT(ctx, "_dmarc."+domain)
	if err != nil {
		out.Err = err
		return out
	}
	for _, r := range records {
		if !strings.HasPrefix(strings.ToLower(r), "v=dmarc1") {
			continue
		}
		out.Found = true
		out.Record = r
		for _, field := range strings.Split(r, ";") {
			field = strings.TrimSpace(field)
			if after, ok := strings.CutPrefix(strings.ToLower(field), "p="); ok {
				out.Policy = after
			}
		}
		break
	}
	return out
}

// DomainOf is the domain part of an address.
func DomainOf(address string) string {
	if at := strings.LastIndex(address, "@"); at >= 0 && at+1 < len(address) {
		return strings.ToLower(address[at+1:])
	}
	return ""
}
