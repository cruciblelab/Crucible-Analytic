package web

import (
	"context"
	"net/netip"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/logging"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/privacy"
)

// The diagnostic file's log lines (catalogue #37, second half).
//
// The owner's decision, 2026-09-28: option (c). The message and the
// classified attributes go, addresses masked the way the product stores
// them (/24, /64), and the client's claim never. Three things make that
// true, and each is its own function below:
//
//   - which lines: WARN and ERROR of the last week, and never a line
//     about a site the person downloading may not see - one deployment
//     hosts several customers, and a line about one site is not the
//     business of another's owner;
//   - which attributes: logging.ExportRuleFor, a list held against every
//     key the tree writes; an attribute nobody classified is withheld;
//   - what is inside them: every network address and email address in
//     the text is shortened, because an address does not stay in the
//     field meant for it - an error can quote what a client sent.

const (
	// diagnosticLogWindow is how far back the file reads.
	diagnosticLogWindow = 7 * 24 * time.Hour
	// diagnosticLogLimit bounds the lines, newest first. The file says
	// when there were more.
	diagnosticLogLimit = 200
)

// diagnosticLogs reads the lines this person may take.
func (s *Server) diagnosticLogs(ctx context.Context, p panel.Principal, now time.Time) diagnosticLogs {
	out := diagnosticLogs{
		Since: now.Add(-diagnosticLogWindow).UTC(),
		Limit: diagnosticLogLimit,
		Lines: []diagnosticLogLine{},
	}
	filter := panel.LogFilter{Since: diagnosticLogWindow, Limit: diagnosticLogLimit}
	// The operator sees every site already. A developer session is one:
	// it carries superadmin authority (panel.developerPrincipal).
	if p.Superadmin {
		out.Scope = "all"
	} else {
		out.Scope = "owned"
		owned, err := s.ownedSites(ctx, p)
		if err != nil {
			// Never widened to "all" for want of an answer: a section that
			// could not decide whose lines to show shows none.
			out.Error = diagnosticError(err)
			return out
		}
		filter.Sites = owned
	}
	lines, truncated, err := s.Store.RecentWarnings(ctx, filter)
	if err != nil {
		out.Error = diagnosticError(err)
		return out
	}
	out.Truncated = truncated
	for _, l := range lines {
		out.Lines = append(out.Lines, diagnosticLogLineFrom(l))
	}
	return out
}

// ownedSites is the sites this person owns - the non-nil list, so owning
// none means lines about no site only.
func (s *Server) ownedSites(ctx context.Context, p panel.Principal) ([]string, error) {
	sites, err := s.Store.Sites(ctx, p, nil)
	if err != nil {
		return nil, err
	}
	owned := []string{}
	for _, site := range sites {
		if site.Role == panel.RoleOwner {
			owned = append(owned, site.SiteID)
		}
	}
	return owned, nil
}

// diagnosticLogLineFrom is one line as it may leave the machine.
func diagnosticLogLineFrom(l panel.LogLine) diagnosticLogLine {
	out := diagnosticLogLine{
		At:       l.At.UTC(),
		Service:  l.Service,
		Level:    l.Level,
		Category: l.Category,
		Message:  maskText(l.Message),
		Site:     l.Site,
	}
	keys := make([]string, 0, len(l.Attrs))
	for k := range l.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := l.Attrs[key]
		switch logging.ExportRuleFor(key) {
		case logging.ExportKept:
			out.keep(key, maskText(value))
			continue
		case logging.ExportAddress:
			// Kept only when there was an address to shorten: a field
			// meant for one that holds something else has not been
			// reasoned about.
			if masked := maskAddresses(value); masked != value || value == "" {
				out.keep(key, masked)
				continue
			}
		}
		out.Withheld = append(out.Withheld, key)
	}
	return out
}

func (l *diagnosticLogLine) keep(key, value string) {
	if l.Attrs == nil {
		l.Attrs = map[string]string{}
	}
	l.Attrs[key] = value
}

// maskText shortens every network address and every email address in a
// piece of text.
func maskText(text string) string {
	return maskEmails(maskAddresses(text))
}

// emailAddress is an email address, loosely: what matters is catching
// one, not validating it.
var emailAddress = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@([A-Za-z0-9\-]+(?:\.[A-Za-z0-9\-]+)*\.[A-Za-z]{2,})`)

// maskEmails keeps the domain and drops the person: "…@example.com" still
// says which mail provider refused a message, and no longer says whose.
func maskEmails(text string) string {
	return emailAddress.ReplaceAllString(text, "…@$1")
}

// maskAddresses replaces every IP address in text with the network it
// belongs to, as the product stores addresses in masked mode.
//
// Every run of characters an address can be written with is tried, and
// only what net/netip accepts is replaced: a version string or a clock
// time is not an address and is left as it is.
func maskAddresses(text string) string {
	var b strings.Builder
	start := -1
	flush := func(end int) {
		if start >= 0 {
			b.WriteString(maskRun(text[start:end]))
			start = -1
		}
	}
	for i, r := range text {
		if isAddressRune(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		flush(i)
		b.WriteRune(r)
	}
	flush(len(text))
	return b.String()
}

func isAddressRune(r rune) bool {
	return r == '.' || r == ':' || (r >= '0' && r <= '9') ||
		(r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

// maskRun masks one run when it is an address, an address with a port,
// or an address closing a sentence.
func maskRun(run string) string {
	if !strings.ContainsAny(run, ".:") {
		return run
	}
	// The whole run first: an IPv6 address may end in "::", which the
	// trimming below would cut into something that is not an address -
	// and leave it unmasked.
	if addr, err := netip.ParseAddr(run); err == nil {
		return maskedNetwork(addr)
	}
	core := strings.TrimRight(run, ".:")
	tail := run[len(core):]
	if addr, err := netip.ParseAddr(core); err == nil {
		return maskedNetwork(addr) + tail
	}
	if ap, err := netip.ParseAddrPort(core); err == nil {
		return maskedNetwork(ap.Addr()) + ":" + strconv.Itoa(int(ap.Port())) + tail
	}
	return run
}

// maskedNetwork is an address shortened by the product's own rule, and
// written as the network it now names.
func maskedNetwork(a netip.Addr) string {
	masked := privacy.MaskIP(a, privacy.IPMasked)
	return netip.PrefixFrom(masked, privacy.MaskedBits(masked)).String()
}
