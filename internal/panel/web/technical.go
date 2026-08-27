package web

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The developer-mode views that are not breakdowns.
//
// D3's rule is columns on the same page rather than pages beside it, and
// these obey it: the histogram and the coverage summary are sections on
// the site page, shown only in developer mode. The two address lists are
// paginated, so they get their own pages the way a breakdown's full list
// does - reached from the summary rather than from the navigation, which
// is what keeps them part of that section rather than a new destination.

// addressListPathSegment is where one address list lives:
// /site/{site}/adres/{liste}.
const addressListPathSegment = "/adres/"

// scoreBandView is one bar of the histogram.
type scoreBandView struct {
	Label string
	Count string
	// Width is the bar's length as one of eleven steps: 0, 10, 20 ... 100.
	//
	// A step rather than an exact percentage because the template cannot
	// use a style attribute. The Content-Security-Policy is
	// `style-src 'self'` with no unsafe-inline, so an inline width is
	// blocked - and a bar that silently does not render is worse than a
	// coarse one. Eleven classes in the stylesheet cover it, and 10% is
	// plenty of resolution for a shape somebody reads at a glance.
	//
	// (Written first as an inline style, with a comment claiming the
	// policy allowed style attributes. It does not, and the structural
	// test that forbids them said so before this reached a browser.)
	Width int
	// Bot marks the bands at the top of the range, so the template can
	// colour them as the different thing they are.
	Bot bool
}

// scoreView is the histogram section.
type scoreView struct {
	Bands []scoreBandView
	Total string
	Empty emptiness
	Text  string
}

// crossoverView is the cross-source section: the measurement this
// product exists to make.
type crossoverView struct {
	Seen     string
	RanJS    string
	Silent   string
	Coverage string
	// BeaconOnly is set only when it is non-zero, because zero is the
	// correct value and drawing "0" invites somebody to read it as a
	// measurement rather than as the absence of a fault.
	BeaconOnly string
	Bands      []coverageBandView
	SilentURL  string
	BotsURL    string
	Empty      emptiness
	Text       string
}

type coverageBandView struct {
	Label    string
	Count    string
	RanJS    string
	Coverage string
}

// addressPage is one page of the silent or JS-bot list.
type addressPage struct {
	SiteID   string
	SitePath string
	Kind     analytics.AddressListKind
	Title    string
	Help     string
	Rows     []addressRowView
	Total    int
	Shown    int
	Page     int
	Pages    int
	PrevURL  string
	NextURL  string
	Ranges   []rangeChoice
	From     time.Time
	To       time.Time
	Notice   string
	Empty    emptiness
	Text     string
	// ShowsClient is whether to draw the browser/OS columns, which only
	// the JS-bot list has: a silent address ran no JavaScript, so nothing
	// ever reported one. Drawing the columns empty would suggest the
	// answer was looked for and not found.
	ShowsClient bool
}

type addressRowView struct {
	Address  string
	Score    string
	Rate     string
	Country  string
	Network  string
	JA4      string
	JA4Label string
	Client   string
	BotUA    bool
	Last     string
}

// technicalLists is the closed set of address lists this panel draws.
//
// Closed for the reason the breakdown registry is: the kind becomes a
// path segment in a request to another service, and a registry lookup is
// what stops a URL somebody typed from reaching the API as an endpoint
// name.
var technicalLists = map[analytics.AddressListKind]struct{ showsClient bool }{
	analytics.ListSilent: {showsClient: false},
	analytics.ListJSBots: {showsClient: true},
}

func addressListPath(siteID string, kind analytics.AddressListKind) string {
	if siteID == "" {
		return ""
	}
	return MembersPathPrefix + url.PathEscape(siteID) + addressListPathSegment + url.PathEscape(string(kind))
}

// technicalSections builds the histogram and the coverage summary.
func (s *Server) technicalSections(lang *ui.Language, f *ui.Formatter, siteID string,
	tech analytics.Technical, board analytics.Dashboard, presence sourcePresence,
	days int) (scoreView, crossoverView) {

	return s.scoreSection(lang, f, tech.Scores, board, presence),
		s.crossoverSection(lang, f, siteID, tech.Crossover, board, presence, days)
}

func (s *Server) scoreSection(lang *ui.Language, f *ui.Formatter, dist analytics.ScoreDistribution,
	board analytics.Dashboard, presence sourcePresence) scoreView {

	var view scoreView
	total := dist.Total()

	if empty := technicalEmptiness(dist.Err, total > 0, sourceTraffic, board, presence); empty != hasData {
		view.Empty = empty
		view.Text = lang.T("pano.bos." + string(empty) + "." + string(sourceTraffic))
		return view
	}

	// The widest band is the full bar. Scaling to the total instead would
	// make every band unreadably short on a site whose traffic is almost
	// all human, which is every healthy site.
	var widest int
	for _, b := range dist.Bands {
		if b.Addresses > widest {
			widest = b.Addresses
		}
	}

	view.Total = f.Number(int64(total))
	for _, b := range dist.Bands {
		// Rounded up, so a band with any addresses at all gets a visible
		// bar rather than a stub indistinguishable from empty.
		width := 0
		if widest > 0 && b.Addresses > 0 {
			width = ((b.Addresses*100/widest)/10)*10 + 10
			if width > 100 {
				width = 100
			}
		}
		view.Bands = append(view.Bands, scoreBandView{
			Label: lang.Tf("pano.skor.aralik", strconv.Itoa(b.Min), strconv.Itoa(b.Max)),
			Count: f.Number(int64(b.Addresses)),
			Width: width,
			// The threshold the collector itself scores against, so the
			// colouring and the blocking agree about what a bot is.
			Bot: b.Min >= botBandFrom,
		})
	}
	return view
}

// botBandFrom is where the histogram starts colouring bands as bot-like.
//
// 70 rather than a rounder number because that is scoring's own default
// threshold. A chart that shaded a different line from the one the
// collector acts on would be a chart that disagrees with the product.
const botBandFrom = 70

func (s *Server) crossoverSection(lang *ui.Language, f *ui.Formatter, siteID string,
	cross analytics.Crossover, board analytics.Dashboard, presence sourcePresence,
	days int) crossoverView {

	var view crossoverView

	if empty := technicalEmptiness(cross.Err, cross.Seen > 0, sourceTraffic, board, presence); empty != hasData {
		view.Empty = empty
		view.Text = lang.T("pano.bos." + string(empty) + "." + string(sourceTraffic))
		return view
	}

	view.Seen = f.Number(int64(cross.Seen))
	view.RanJS = f.Number(int64(cross.RanJS))
	view.Silent = f.Number(int64(cross.Silent))
	view.Coverage = f.Share(int64(cross.RanJS), int64(cross.Seen))

	// Only when it is not zero. Zero is the correct value, and drawing it
	// beside the others invites a reader to treat a fault indicator as a
	// measurement.
	if cross.BeaconOnly > 0 {
		view.BeaconOnly = lang.Tf("pano.kesisim.yalniz_beacon", f.Number(int64(cross.BeaconOnly)))
	}

	for _, b := range cross.Bands {
		view.Bands = append(view.Bands, coverageBandView{
			Label:    lang.Tf("pano.skor.aralik", strconv.Itoa(b.Min), strconv.Itoa(b.Max)),
			Count:    f.Number(int64(b.Addresses)),
			RanJS:    f.Number(int64(b.RanJS)),
			Coverage: f.Share(int64(b.RanJS), int64(b.Addresses)),
		})
	}

	period := "?gun=" + strconv.Itoa(days)
	view.SilentURL = addressListPath(siteID, analytics.ListSilent) + period
	view.BotsURL = addressListPath(siteID, analytics.ListJSBots) + period
	return view
}

// technicalEmptiness answers the same four-state question the cards and
// breakdowns answer, from the same presence lookup.
//
// Answered here rather than reimplemented: whether an empty view means an
// uninstalled collector or a quiet period is one question, and answering
// it in three places is how the three come to disagree.
func technicalEmptiness(err error, hasRows bool, source cardSource,
	board analytics.Dashboard, presence sourcePresence) emptiness {

	switch {
	case err != nil:
		board.TrafficErr, board.BeaconErr = err, err
	case hasRows:
		return hasData
	default:
		board.Traffic.Snapshots = 0
		board.Beacon.Pageviews, board.Beacon.Events = 0, 0
	}
	return emptinessFor(source, board, presence)
}

// addressListHandler draws one paginated address list.
func (s *Server) addressListHandler(w http.ResponseWriter, r *http.Request) {
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

	// Registry first, before anything reads the request further and long
	// before the segment could reach a URL.
	kind := analytics.AddressListKind(r.PathValue("liste"))
	def, known := technicalLists[kind]
	if !known {
		s.Renderer.ErrorIn(w, r, http.StatusNotFound, lang)
		return
	}

	access, ok := s.siteAccess(w, r, p, siteID)
	if !ok {
		return
	}

	// Refused by the role, not by the preference - the same rule the
	// technical breakdowns use, and for the same reason: the role is a
	// boundary, the preference is a choice about what appears unbidden,
	// and navigating to an address is itself the request. 404 rather than
	// 403, matching siteAccess, so a refusal does not describe what it is
	// refusing.
	if !access.Can(panel.CapUseDeveloperMode) {
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

	data := s.addressListData(r.Context(), lang, siteID, kind, def.showsClient, days, page, from, to)
	view := s.page(r, lang, access, "siteler", siteID)
	view.Site = ui.SiteView{ID: siteID, Name: s.siteName(r.Context(), access, siteID)}
	view.Heading = data.Title
	view.Data = data
	if data.Notice != "" {
		view.Notices = append(view.Notices, ui.Notice{Level: ui.NoticeWarn, Body: data.Notice})
	}
	view.Notices = append(view.Notices, viewerNotice(lang, access)...)
	s.Renderer.Render(w, r, http.StatusOK, "adres", view)
}

func (s *Server) addressListData(ctx context.Context, lang *ui.Language, siteID string,
	kind analytics.AddressListKind, showsClient bool, days, page int,
	from, to time.Time) addressPage {

	key := "pano.adres." + string(kind)
	base := addressListPath(siteID, kind)

	data := addressPage{
		SiteID:   siteID,
		SitePath: sitePath(siteID) + "?gun=" + strconv.Itoa(days),
		Kind:     kind,
		Title:    lang.T(key + ".baslik"),
		Help:     lang.T(key + ".aciklama"),
		From:     from, To: to,
		Page:        page,
		ShowsClient: showsClient,
	}
	for _, d := range rangeDays {
		data.Ranges = append(data.Ranges, rangeChoice{
			Days:     d,
			Label:    lang.Tn("pano.aralik.gun", d, strconv.Itoa(d)),
			URL:      base + "?gun=" + strconv.Itoa(d),
			Selected: d == days,
		})
	}

	f := ui.NewFormatter(lang, s.zone(ctx))
	tech := s.Analytics.FetchTechnical(ctx, siteID, from, to, analytics.TechnicalRequest{
		List:   kind,
		Limit:  detailRows,
		Offset: (page - 1) * detailRows,
	})
	list := tech.List

	switch {
	case list.Err != nil:
		// The four-state answer needs a presence lookup, and this page
		// fetched no summary to base one on. A transport failure is
		// reported as itself rather than guessed at.
		data.Empty = unreachable
		data.Text = lang.T("pano.bos." + string(unreachable) + "." + string(sourceTraffic))
		data.Notice = lang.T("pano.hata.ulasilamiyor")
		return data
	case len(list.Rows) == 0:
		data.Empty = nothingInRange
		data.Text = lang.T("pano.bos." + string(nothingInRange) + "." + string(sourceTraffic))
		return data
	}

	data.Total = list.Total
	data.Pages = pageCount(list.Total, detailRows)
	for _, row := range list.Rows {
		out := addressRowView{
			Address:  row.Address,
			Score:    f.Number(int64(row.PeakScore)),
			Country:  row.Country,
			JA4:      row.JA4,
			JA4Label: row.JA4Label,
			BotUA:    row.BotUA,
			Last:     f.DateTime(row.Last),
		}
		if row.Rate > 0 {
			out.Rate = f.Decimal(row.Rate, 1)
		}
		if row.ASN != 0 {
			out.Network = strconv.Itoa(row.ASN)
			if row.ASNName != "" {
				out.Network = row.ASNName
			}
		}
		// One string rather than two columns: "HeadlessChrome, Linux" is
		// what a person reads, and a browser with no OS is common enough
		// that a second column would mostly be empty.
		switch {
		case row.Browser != "" && row.OS != "":
			out.Client = row.Browser + ", " + row.OS
		case row.Browser != "":
			out.Client = row.Browser
		case row.OS != "":
			out.Client = row.OS
		}
		data.Rows = append(data.Rows, out)
	}
	data.Shown = len(data.Rows)

	if page > 1 {
		data.PrevURL = detailURL(base, days, page-1)
	}
	if page < data.Pages {
		data.NextURL = detailURL(base, days, page+1)
	}
	return data
}
