package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The breakdowns: why a number is what it is.
//
// D1 put six figures on a page and nothing behind them. A customer could
// read "1 240 pageviews" and had no way to ask which pages. This is that
// question, six times over, and it is the last thing between this panel
// and something a person would actually use daily.
//
// Three things carry over from D1 deliberately.
//
// **The set is closed.** Same reason as the cards, plus a sharper one: a
// breakdown's identity becomes a path segment in a request to another
// service. A registry lookup is what stops a URL somebody typed from
// reaching the API as an endpoint name.
//
// **The empty group is a row, not a gap.** The API flags the group where
// a value was never determined - a visit with no referrer, an unresolved
// country - rather than dropping it, so the groups still add up. Drawing
// it as a blank line, or leaving it out, loses that on the last hop.
//
// **A share needs a denominator from the same population.** The summary
// and the breakdowns both take the API's default bots filter, so a row's
// percentage is a percentage of the number the card above it shows.

// breakdownPathSegment is where one breakdown's full list lives:
// /site/{site}/detay/{kirilim}.
const breakdownPathSegment = "/detay/"

// metric says which summary figure a breakdown's rows are a share of.
//
// Two, because the beacon summary has two totals a row can belong to and
// dividing by the wrong one produces percentages that look plausible and
// are wrong - the failure mode nobody catches by looking.
type metric string

const (
	// metricPageviews: rows are counted in pageviews.
	metricPageviews metric = "goruntuleme"
	// metricEvents: rows are counted in custom-event occurrences.
	metricEvents metric = "olay"
	// metricAddresses: rows are counted in distinct IP addresses seen by
	// the collector.
	//
	// A third metric rather than a reuse of pageviews, for the reason this
	// type exists at all. The collector's breakdowns divide by the traffic
	// summary's address count; dividing them by the beacon's pageviews
	// would produce shares that render perfectly, sum to something near
	// 100%, and mean nothing - twelve addresses out of four hundred
	// pageviews is not a percentage of anything.
	metricAddresses metric = "adres"
)

// breakdownDef describes one section.
type breakdownDef struct {
	Kind   analytics.BreakdownKind
	Source cardSource
	Metric metric
	// NamedGroup is false for the one breakdown that has no
	// never-determined group to name.
	//
	// Campaigns is that breakdown: its endpoint groups by the stored
	// campaign query and excludes the empty one in SQL, so untagged
	// traffic is not a flagged row - it is absent. Giving it a word
	// anyway would put a string in both catalogs for a line that can
	// never render, which is how a catalog starts describing a page that
	// no longer exists.
	NamedGroup bool
	// Technical marks a breakdown that only appears in developer mode.
	//
	// D6's rule is that the default view carries no fingerprints, no ASNs
	// and no jargon, because the person who had the site built cannot act
	// on a JA4 hash. These sections exist for the person who can.
	Technical bool
	// Bots is whether the row's second column is the bot count rather
	// than the visitor count.
	//
	// It tracks Technical today and is still its own field, because the
	// two say different things: one is who may see the section, the other
	// is which number the row carries. A breakdown that is technical and
	// counts visitors is not a contradiction, and the first one that
	// exists should be a table edit rather than a wrong column heading.
	Bots bool
}

// breakdownDefs is every breakdown this panel knows how to draw.
//
// Nine. The first six are the ones a customer asks for unprompted; the
// last three are the collector's, and they appear only in developer mode
// (D3). A shop owner reading their own numbers never meets a JA4 hash.
var breakdownDefs = map[analytics.BreakdownKind]breakdownDef{
	analytics.BreakdownPages: {
		Kind: analytics.BreakdownPages, Source: sourceBeacon,
		Metric: metricPageviews, NamedGroup: true,
	},
	analytics.BreakdownReferrers: {
		Kind: analytics.BreakdownReferrers, Source: sourceBeacon,
		Metric: metricPageviews, NamedGroup: true,
	},
	analytics.BreakdownCampaigns: {
		Kind: analytics.BreakdownCampaigns, Source: sourceBeacon,
		Metric: metricPageviews, NamedGroup: false,
	},
	analytics.BreakdownDevices: {
		Kind: analytics.BreakdownDevices, Source: sourceBeacon,
		Metric: metricPageviews, NamedGroup: true,
	},
	analytics.BreakdownCountries: {
		Kind: analytics.BreakdownCountries, Source: sourceBeacon,
		Metric: metricPageviews, NamedGroup: true,
	},
	analytics.BreakdownEvents: {
		Kind: analytics.BreakdownEvents, Source: sourceBeacon,
		Metric: metricEvents, NamedGroup: true,
	},

	// D3's three. Source is the collector, so their empty-state sentences
	// come from the traffic side: a site with no snippet still has these
	// numbers, because the collector sees every connection whether or not
	// anything ran JavaScript. That is the whole reason they are worth a
	// section.
	analytics.BreakdownFingerprints: {
		Kind: analytics.BreakdownFingerprints, Source: sourceTraffic,
		Metric: metricAddresses, NamedGroup: true, Technical: true, Bots: true,
	},
	analytics.BreakdownASNs: {
		Kind: analytics.BreakdownASNs, Source: sourceTraffic,
		Metric: metricAddresses, NamedGroup: true, Technical: true, Bots: true,
	},
	analytics.BreakdownServerCountries: {
		Kind: analytics.BreakdownServerCountries, Source: sourceTraffic,
		Metric: metricAddresses, NamedGroup: true, Technical: true, Bots: true,
	},
}

// technicalBreakdowns is the order the developer-mode sections appear in,
// after the six ordinary ones.
//
// Fingerprints first because it is the question only this collector can
// answer - what connected, whether or not it ran any JavaScript. Then the
// network behind it, then where it was. Countries last of the three
// because it is the one that overlaps a section already on the page, and
// putting it directly under the beacon's countries would invite exactly
// the comparison the two kinds exist to keep apart.
var technicalBreakdowns = []analytics.BreakdownKind{
	analytics.BreakdownFingerprints,
	analytics.BreakdownASNs,
	analytics.BreakdownServerCountries,
}

// defaultBreakdowns is the order the sections appear in.
//
// Not alphabetical and not arbitrary: it is the order the questions get
// asked. What did they read, where did they come from, which campaign
// brought them, on what, from where - and events last, because a
// deployment that raises none should not open with an empty table.
var defaultBreakdowns = []analytics.BreakdownKind{
	analytics.BreakdownPages,
	analytics.BreakdownReferrers,
	analytics.BreakdownCampaigns,
	analytics.BreakdownDevices,
	analytics.BreakdownCountries,
	analytics.BreakdownEvents,
}

const (
	// sectionRows is how many lines a section shows on the site page.
	//
	// Eight: enough that the shape of a distribution is visible, few
	// enough that six sections still fit on a screen somebody scrolls
	// rather than surveys. The rest are one click away and the section
	// says how many there are.
	sectionRows = 8
	// detailRows is one page of a breakdown's own list.
	detailRows = 25
	// maxDetailPage bounds the pager.
	//
	// The API refuses an offset past 100 000 and says a caller that needs
	// to go deeper should narrow its range instead. The panel stops well
	// before that, because a person clicking to page 400 is not reading -
	// and the honest answer to "I need row 10 000" is a shorter period.
	maxDetailPage = 200
)

// breakdownRow is one line as the template receives it.
type breakdownRow struct {
	// Label is the group's value, or the name given to the group whose
	// value was never determined.
	Label string
	// Named marks that Label is that name rather than a measured value,
	// so the template can style it as the different kind of thing it is.
	Named bool
	// Raw is the unformatted count, kept so a test can check ordering
	// without parsing localised digits.
	Raw      int
	Count    string
	Visitors string
	// Share is the row's percentage of the summary, or the dash when
	// there is no denominator. Never "0%" for a missing total.
	Share string
	// Bar is the same figure as the width of an SVG rect, empty when
	// Share is the dash. See ui.BarWidth for why it is a number rather
	// than a CSS class and why the scale is absolute.
	Bar string
	// Detail is a human-readable name beside Label where the API resolved
	// one: an ASN's organisation, or the bot behind a fingerprint.
	//
	// Beside rather than instead of. "Google LLC" is what a person reads
	// and "15169" is what they paste into a search, and a table that
	// drops either has dropped the half somebody needed.
	Detail string
	// KnownBot marks a fingerprint the scoring package recognises, so the
	// template can say so rather than leaving the reader to infer it from
	// a bot count that happens to equal the address count.
	KnownBot bool
}

// breakdownView is one section.
type breakdownView struct {
	Kind  analytics.BreakdownKind
	Title string
	Help  string
	// ColumnLabel heads the first column and CountLabel the second. Both
	// per breakdown rather than generic: a table headed "Value" and
	// "Count" is one a reader has to look up at the section title to
	// understand, and the counts genuinely are not the same quantity.
	ColumnLabel string
	CountLabel  string
	// SecondLabel heads the third column, which is visitors for a beacon
	// breakdown and bot addresses for a collector one.
	//
	// It moved out of the template, where it was one shared key, when D3
	// gave the column two meanings. Leaving it there would have meant
	// either a second renderer - which this partial's own comment warns
	// against, because two drift - or a table that says "Visitors" over
	// a count of bot addresses.
	SecondLabel string
	Rows        []breakdownRow
	// Total is how many groups exist; Shown is how many are drawn.
	Total int
	Shown int
	// MoreURL is the section's own page, set only when there is more to
	// see than is on the page; MoreText is its label, carrying the total
	// through the locale's digit grouping rather than the template's.
	MoreURL  string
	MoreText string
	// Empty is why there are no rows, using the same four states as a
	// card - a breakdown drawn as "no data" would present an uninstalled
	// snippet as a measurement just as a card would.
	Empty     emptiness
	EmptyText string
}

// detailPage is Data for one breakdown's own page.
type detailPage struct {
	SiteID   string
	SitePath string
	Section  breakdownView
	From     time.Time
	To       time.Time
	Ranges   []rangeChoice
	// Page is 1-based, as the URL and the reader count.
	Page  int
	Pages int
	// PrevURL and NextURL are empty at the ends rather than disabled
	// links, so a keyboard reader is not offered a control that does
	// nothing.
	PrevURL string
	NextURL string
	Notice  string
}

// breakdownPath is one breakdown's own page.
func breakdownPath(siteID string, kind analytics.BreakdownKind) string {
	if siteID == "" {
		return ""
	}
	return MembersPathPrefix + url.PathEscape(siteID) + breakdownPathSegment + url.PathEscape(string(kind))
}

// sections builds the six section views for the site page.
func (s *Server) sections(lang *ui.Language, f *ui.Formatter, siteID string,
	site analytics.Site, presence sourcePresence, days int,
	shown []analytics.BreakdownKind) []breakdownView {

	out := make([]breakdownView, 0, len(shown))
	for _, kind := range shown {
		def, ok := breakdownDefs[kind]
		if !ok {
			continue
		}
		view := s.section(lang, f, def, site, presence)
		if view.Total > len(view.Rows) {
			view.MoreURL = breakdownPath(siteID, kind) + "?gun=" + strconv.Itoa(days)
			view.MoreText = lang.Tf("pano.kirilim.tumu", f.Number(int64(view.Total)))
		}
		out = append(out, view)
	}
	return out
}

// section shapes one breakdown for display.
func (s *Server) section(lang *ui.Language, f *ui.Formatter, def breakdownDef,
	site analytics.Site, presence sourcePresence) breakdownView {

	key := "pano.kirilim." + string(def.Kind)
	view := breakdownView{
		Kind:        def.Kind,
		Title:       lang.T(key + ".baslik"),
		Help:        lang.T(key + ".aciklama"),
		ColumnLabel: lang.T(key + ".sutun"),
		CountLabel:  lang.T(key + ".sayi"),
		SecondLabel: lang.T(secondColumnKey(def)),
	}

	b := site.Breakdowns[def.Kind]
	view.Total = b.Total

	// A breakdown's own failure comes first and is not the summary's.
	// The section can be unreachable while the cards above it are fine -
	// six concurrent calls, six independent answers - and saying "no
	// data" for a call that never returned would report a timeout as a
	// measurement of zero.
	if empty := breakdownEmptiness(def, b, site, presence); empty != hasData {
		view.Empty = empty
		view.EmptyText = lang.T("pano.bos." + string(empty) + "." + string(def.Source))
		return view
	}

	total := shareDenominator(def, site)

	for _, row := range b.Rows {
		out := breakdownRow{
			Label:    row.Key,
			Raw:      row.Count,
			Count:    f.Number(int64(row.Count)),
			Visitors: f.Number(int64(row.Visitors)),
			Share:    f.Share(int64(row.Count), total),
			Bar:      ui.BarWidth(int64(row.Count), total),
		}
		// The collector's rows carry bot addresses where the beacon's
		// carry visitors. Same column position, different question, and
		// the heading above it comes from the same registry entry that
		// set this - see breakdownDef.Bots.
		if def.Bots {
			out.Visitors = f.Number(int64(row.Bots))
		}
		if row.Label != "" {
			// An ASN's organisation or a known bot's name. Shown beside
			// the key rather than instead of it: "Google LLC" is what a
			// person reads, "15169" is what they search for, and a table
			// that drops either is missing the half somebody needs.
			out.Detail = row.Label
		}
		if row.KnownBot {
			out.KnownBot = true
		}
		if row.Empty && def.NamedGroup {
			out.Label = lang.T(key + ".bos_grup")
			out.Named = true
		}
		view.Rows = append(view.Rows, out)
	}
	view.Shown = len(view.Rows)
	return view
}

// secondColumnKey names the third column for this breakdown.
func secondColumnKey(def breakdownDef) string {
	if def.Bots {
		return "pano.kirilim.sutun_bot"
	}
	return "pano.kirilim.sutun_ziyaretci"
}

// shareDenominator picks the total a row's percentage is a share of.
//
// Three metrics, three denominators, and getting it wrong is the failure
// this whole type exists to prevent: a wrong denominator produces
// percentages that look plausible, sum to something near 100, and answer
// a question nobody asked. Twelve addresses out of four hundred pageviews
// is not a percentage of anything.
func shareDenominator(def breakdownDef, site analytics.Site) int64 {
	switch def.Metric {
	case metricEvents:
		return int64(site.Beacon.Events)
	case metricAddresses:
		// The collector's own count of distinct addresses, which is what
		// its breakdowns are groups of. Reaching for the beacon here
		// would divide addresses by pageviews.
		return int64(site.Traffic.UniqueIPs)
	default:
		return int64(site.Beacon.Pageviews)
	}
}

// breakdownEmptiness is emptinessFor with the breakdown's own error and
// its own row count.
//
// It defers to the card rule for everything except "there are no rows":
// whether an empty list means an uninstalled snippet or a quiet period
// is the same question a card asks, answered from the same presence
// lookup, and answering it twice in two places is how the two come to
// disagree.
func breakdownEmptiness(def breakdownDef, b analytics.Breakdown,
	site analytics.Site, presence sourcePresence) emptiness {

	board := site.Dashboard
	// The breakdown's own transport failure outranks the summary's
	// health: this section has nothing to draw whatever the cards did.
	switch {
	case b.Err != nil:
		board.BeaconErr, board.TrafficErr = b.Err, b.Err
	case len(b.Rows) > 0:
		return hasData
	default:
		// No rows and no error. Fall through to the card rule, which
		// distinguishes a source that never wrote from a period that is
		// genuinely empty.
		board.Beacon.Pageviews, board.Beacon.Events = 0, 0
		board.Traffic.Snapshots = 0
	}
	return emptinessFor(def.Source, board, presence)
}

// detailHandler draws one breakdown's full, paginated list.
func (s *Server) detailHandler(w http.ResponseWriter, r *http.Request) {
	lang := s.language(r)
	if !s.haveStore(w, r, lang) {
		return
	}
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	siteID := r.PathValue("site")
	if siteID == "" {
		s.Renderer.ErrorIn(w, r, http.StatusNotFound, lang)
		return
	}

	// The registry lookup happens before the access check reads anything
	// and long before the kind reaches a URL. An unknown segment is a 404
	// here, not an endpoint the API gets asked about.
	kind := analytics.BreakdownKind(r.PathValue("kirilim"))
	def, known := breakdownDefs[kind]
	if !known {
		s.Renderer.ErrorIn(w, r, http.StatusNotFound, lang)
		return
	}

	access, ok := s.siteAccess(w, r, p, siteID)
	if !ok {
		return
	}

	// A technical breakdown reached by URL is refused by the *role*, not
	// by the preference - and the difference is the whole reason those are
	// two things (see panel.Access.ShowsTechnical).
	//
	// The role is a boundary: somebody who may never see fingerprints must
	// not get them by typing a path, which is exactly what this line
	// stops. The preference is not a boundary, it is a choice about what
	// appears unbidden - and navigating to the address of a thing is
	// itself an unambiguous request to see it. Refusing that would make
	// the toggle mean something it was explicitly designed not to mean.
	//
	// 404 rather than 403, matching siteAccess: a refusal that describes
	// what it is refusing tells the reader a page exists for people unlike
	// them, which is a fact about the deployment they have no business
	// learning from an error code.
	if def.Technical && !access.Can(panel.CapUseDeveloperMode) {
		s.Renderer.ErrorIn(w, r, http.StatusNotFound, lang)
		return
	}

	switch r.Method {
	case http.MethodGet, http.MethodHead:
	default:
		w.Header().Set("Allow", "GET, HEAD")
		s.Renderer.ErrorIn(w, r, http.StatusMethodNotAllowed, lang)
		return
	}

	days := s.rangeFrom(r)
	page := detailPageFrom(r)
	from, to := wholeDays(time.Now().In(s.zone(r.Context())), days)

	data := s.detailData(r.Context(), lang, siteID, def, days, page, from, to)
	view := s.page(r, lang, access, "siteler", siteID)
	view.Site = ui.SiteView{ID: siteID, Name: s.siteName(r.Context(), access, siteID)}
	view.Heading = data.Section.Title
	view.Data = data
	if data.Notice != "" {
		view.Notices = append(view.Notices, ui.Notice{Level: ui.NoticeWarn, Body: data.Notice})
	}
	view.Notices = append(view.Notices, viewerNotice(lang, access)...)
	s.Renderer.Render(w, r, http.StatusOK, "detay", view)
}

// detailData fetches and shapes one breakdown page.
func (s *Server) detailData(ctx context.Context, lang *ui.Language, siteID string,
	def breakdownDef, days, page int, from, to time.Time) detailPage {

	data := detailPage{
		SiteID:   siteID,
		SitePath: sitePath(siteID) + "?gun=" + strconv.Itoa(days),
		From:     from, To: to,
		Page: page,
	}
	base := breakdownPath(siteID, def.Kind)
	for _, d := range rangeDays {
		data.Ranges = append(data.Ranges, rangeChoice{
			Days:  d,
			Label: lang.Tn("pano.aralik.gun", d, strconv.Itoa(d)),
			// Changing the period returns to the first page. Keeping the
			// page number would silently show page 9 of a list that is now
			// two pages long, which reads as "there is nothing here".
			URL:      base + "?gun=" + strconv.Itoa(d),
			Selected: d == days,
		})
	}

	f := ui.NewFormatter(lang, s.zone(ctx))
	// The summary this breakdown divides by comes along, because every
	// share on this page is a percentage of it. Which one that is comes
	// from summaryFlags rather than from a constant here: this line read
	// `Beacon: true` until D3, which was right for all six beacon
	// breakdowns and silently wrong for the collector's three.
	//
	// No cards are drawn on this page, so nothing else is asked for.
	traffic, beacon := summaryFlags(def.Kind)
	summaries := analytics.SiteRequest{Traffic: traffic, Beacon: beacon}

	req := summaries
	req.Breakdowns = []analytics.BreakdownRequest{{
		Kind: def.Kind, Limit: detailRows, Offset: (page - 1) * detailRows,
	}}
	site := s.Analytics.FetchSite(ctx, siteID, from, to, req)

	presence := s.presence(ctx, summaries, site.Dashboard, siteID)
	data.Section = s.section(lang, f, def, site, presence)

	b := site.Breakdowns[def.Kind]
	switch {
	case errors.Is(b.Err, analytics.ErrRefused):
		data.Notice = lang.T("pano.hata.reddedildi")
	case b.Err != nil:
		data.Notice = lang.T("pano.hata.ulasilamiyor")
	}

	data.Pages = pageCount(b.Total, detailRows)
	if page > 1 {
		data.PrevURL = detailURL(base, days, page-1)
	}
	if page < data.Pages {
		data.NextURL = detailURL(base, days, page+1)
	}
	return data
}

// detailURL builds a page link, leaving page 1 unnumbered so the first
// page has one address rather than two.
func detailURL(base string, days, page int) string {
	q := url.Values{"gun": {strconv.Itoa(days)}}
	if page > 1 {
		q.Set("sayfa", strconv.Itoa(page))
	}
	return base + "?" + q.Encode()
}

// pageCount is how many pages of size per a total needs. Zero rows is
// zero pages, not one empty one.
func pageCount(total, per int) int {
	if total <= 0 || per <= 0 {
		return 0
	}
	pages := (total + per - 1) / per
	if pages > maxDetailPage {
		return maxDetailPage
	}
	return pages
}

// detailPageFrom reads the 1-based page number from the query string.
//
// Anything unreadable, negative or past the bound takes page 1 rather
// than being an error, for the reason rangeFrom gives: a mistyped URL
// should show the list, not a refusal. The upper bound matters more than
// it looks - the number becomes an offset in a request to another
// service, and an unbounded one is a request to walk a table.
func detailPageFrom(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("sayfa"))
	if err != nil || n < 1 || n > maxDetailPage {
		return 1
	}
	return n
}
