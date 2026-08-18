package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

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
}

// breakdownDefs is every breakdown this panel knows how to draw.
//
// Six, and the six are the ones a customer asks for unprompted. What is
// missing is missing on purpose: fingerprints, ASNs, score distributions
// and the cross-source views belong to D3, which adds columns to these
// sections rather than pages beside them.
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
	site analytics.Site, presence sourcePresence, days int) []breakdownView {

	out := make([]breakdownView, 0, len(defaultBreakdowns))
	for _, kind := range defaultBreakdowns {
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

	total := int64(site.Beacon.Pageviews)
	if def.Metric == metricEvents {
		total = int64(site.Beacon.Events)
	}

	for _, row := range b.Rows {
		out := breakdownRow{
			Label:    row.Key,
			Raw:      row.Count,
			Count:    f.Number(int64(row.Count)),
			Visitors: f.Number(int64(row.Visitors)),
			Share:    f.Share(int64(row.Count), total),
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
	site := s.Analytics.FetchSite(ctx, siteID, from, to, analytics.BreakdownRequest{
		Kind: def.Kind, Limit: detailRows, Offset: (page - 1) * detailRows,
	})

	presence := s.presence(ctx, site.Dashboard, siteID)
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
