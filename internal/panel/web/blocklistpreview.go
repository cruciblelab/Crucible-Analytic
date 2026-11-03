package web

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// Before a new country or ASN rule is accepted, how much of the last
// day's traffic it would have turned away (B3f, catalog #12).
//
// collector.blocked_countries and collector.blocked_asns refuse every
// matching connection on every site, before anything is recorded. A
// wrong entry - TR for TM, the ASN of the mobile carrier half the
// customers use - throws real visitors away and leaves nothing to notice
// it by. So a save that adds an entry stops once, shows each new rule's
// distinct addresses in the last 24 hours, and saves only when the same
// form is sent back with the added rules confirmed.
//
// # Who sees the numbers
//
// They count every site. Only somebody who may change these settings is
// shown them, and since H10 that is the operator or an account that owns
// every site: the deployment's own traffic, not another customer's. A
// save from anybody else never reaches the preview - it goes on to
// ApplySetting and is refused there with the sentence that says why.
//
// # Warn, do not block
//
// The owner's rule: a real harm is refused, anything else is said and
// left to the person. A rule that matches thousands of addresses may be
// exactly what an attack needs, so the preview never refuses; and a
// preview that could not be read says so and still offers the save.
//
// # What it cannot count
//
// A rule already on the list refuses before anything is recorded, so its
// last day is not in the table; the preview counts only what a new rule
// would add, and says how many rules are left out and why. For those,
// the collector's own count of what the rules turned away since it
// started (heartbeat.CounterGeoBlocked) is shown instead.

// blocklistKey reports whether key is one of the two refusing lists.
func blocklistKey(key panel.Key) bool {
	return key == panel.KeyBlockedCountries || key == panel.KeyBlockedASNs
}

// blocklistPreview is what the page draws above the sections.
type blocklistPreview struct {
	Key   string
	Label string
	// Value is the list as it will be saved, re-sent by the confirm form.
	Value string
	// Confirm is the added rules, joined, posted back as "onay". The save
	// goes through only when it still equals the rules a fresh read of
	// the stored list says are being added, so a list changed in between
	// is previewed again rather than saved on an old answer.
	Confirm string
	Rows    []blocklistPreviewRow
	// More says how many added rules were not asked about (the API's cap).
	More string
	// Totals is the window's sentence; NoPlace says no address in the
	// window was placed, so this kind of rule cannot match anything.
	Totals  string
	NoPlace string
	// Error is set when the counts could not be read.
	Error string
	// InForce is the sentence about the rules already on the list, and
	// Refused the collector's count of what they turned away.
	InForce string
	Refused string
}

// blocklistPreviewRow is one added rule.
type blocklistPreviewRow struct {
	Rule      string
	Addresses string
}

// blocklistPreviewFor decides whether this save stops for a preview, and
// builds it. False means go on and save: not a blocklist, nothing added,
// a value the store will refuse anyway, or the added rules confirmed.
func (s *Server) blocklistPreviewFor(ctx context.Context, lang *ui.Language, def panel.Definition,
	r *http.Request) (blocklistPreview, bool) {

	value, err := parseSettingValue(def, r)
	if err != nil {
		return blocklistPreview{}, false
	}
	canonical, err := panel.Validate(def.Key, value)
	if err != nil {
		// ApplySetting refuses it with its own sentence.
		return blocklistPreview{}, false
	}
	proposed, _ := canonical.([]string)
	stored, err := s.Store.GetSetting(ctx, def.Key, "")
	if err != nil {
		s.logger().Warn("panel: reading the blocklist before a preview", "key", def.Key, "err", err)
	}
	current := toStringList(stored)
	added := addedRules(def.Key, current, proposed)
	if len(added) == 0 {
		return blocklistPreview{}, false
	}
	if r.PostFormValue("onay") == strings.Join(added, ",") {
		return blocklistPreview{}, false
	}

	f := ui.NewFormatter(lang, s.zone(ctx))
	out := blocklistPreview{
		Key:     string(def.Key),
		Label:   def.Label,
		Value:   strings.Join(proposed, ", "),
		Confirm: strings.Join(added, ","),
	}
	asked := added
	if len(asked) > analytics.MaxPreviewRules {
		out.More = lang.Tf("ayarlar.onizleme.fazlasi", len(asked)-analytics.MaxPreviewRules, analytics.MaxPreviewRules)
		asked = asked[:analytics.MaxPreviewRules]
	}

	var countries []string
	var asns []int
	if def.Key == panel.KeyBlockedCountries {
		countries = asked
	} else {
		for _, a := range asked {
			n, _ := strconv.Atoi(a)
			asns = append(asns, n)
		}
	}
	p, err := s.Analytics.BlocklistPreview(ctx, countries, asns)
	switch {
	case err != nil:
		s.logger().Warn("panel: reading the blocklist preview", "key", def.Key, "err", err)
		// Not configured is the one cause with its own sentence: there is
		// nothing to wait for. Unreachable, refused or failed all read
		// the same to the person saving - the counts are not known - and
		// the cause is in the log line above.
		out.Error = lang.T("ayarlar.onizleme.okunamadi")
		if !s.Analytics.Configured() {
			out.Error = lang.T("ayarlar.onizleme.api_yok")
		}
		for _, rule := range asked {
			out.Rows = append(out.Rows, blocklistPreviewRow{Rule: rule, Addresses: f.Dash()})
		}
	default:
		for _, c := range p.Countries {
			out.Rows = append(out.Rows, blocklistPreviewRow{Rule: c.Country, Addresses: f.Number(c.Addresses)})
		}
		for _, a := range p.ASNs {
			out.Rows = append(out.Rows, blocklistPreviewRow{Rule: strconv.Itoa(a.ASN), Addresses: f.Number(a.Addresses)})
		}
		out.Totals = lang.Tf("ayarlar.onizleme.toplam",
			f.Number(p.Addresses), f.Number(p.WithCountry), f.Number(p.WithASN))
		placed := p.WithASN
		if def.Key == panel.KeyBlockedCountries {
			placed = p.WithCountry
		}
		if placed == 0 {
			out.NoPlace = lang.T("ayarlar.onizleme.cozumsuz_asn")
			if def.Key == panel.KeyBlockedCountries {
				out.NoPlace = lang.T("ayarlar.onizleme.cozumsuz_ulke")
			}
		}
	}

	if inForce := len(proposed) - len(added); inForce > 0 {
		out.InForce = lang.Tf("ayarlar.onizleme.yururlukte", inForce)
	}
	refusals, err := s.Store.GeoRefusals(ctx)
	switch {
	case err != nil:
		s.logger().Warn("panel: reading the collector's refusals", "err", err)
		out.Refused = lang.T("ayarlar.onizleme.ret_bilinmiyor")
	case !refusals.Known:
		out.Refused = lang.T("ayarlar.onizleme.ret_bilinmiyor")
	default:
		out.Refused = lang.Tf("ayarlar.onizleme.ret", f.DateTime(refusals.Since), f.Number(refusals.Refused))
	}
	return out, true
}

// addedRules is the entries of proposed that current does not hold, in
// the order typed, compared the way the collector compares them:
// country codes without case, ASNs as numbers.
func addedRules(key panel.Key, current, proposed []string) []string {
	norm := func(v string) string {
		v = strings.TrimSpace(v)
		if key == panel.KeyBlockedCountries {
			return strings.ToUpper(v)
		}
		if n, err := strconv.Atoi(v); err == nil {
			return strconv.Itoa(n)
		}
		return v
	}
	have := map[string]bool{}
	for _, c := range current {
		have[norm(c)] = true
	}
	var out []string
	for _, p := range proposed {
		n := norm(p)
		if have[n] || slices.Contains(out, n) {
			continue
		}
		out = append(out, n)
	}
	return out
}
