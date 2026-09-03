package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The page the whole product exists for.
//
// Everything before this phase built the machinery: two collectors
// writing two tables, a read-only API over them, a panel with accounts
// and permissions. None of it showed a customer a number. This does.
//
// Three decisions run through it.
//
// **The numbers arrive over HTTP.** The panel's database role cannot
// read the analytics tables, deliberately, so this page calls the
// read-only API exactly as an external dashboard would. See
// internal/panel/analytics.
//
// **The card set is a closed registry, not six blocks of template.**
// C6 makes which cards appear a per-deployment setting, and a page with
// six hard-coded cards is a page C6 has to take apart first. Written as
// data now, so that phase becomes a selection rather than a rewrite.
//
// **A range is whole days in the panel's timezone.** Sessions are
// counted within the range, so one that began before it is truncated:
// "the last seven days" measured from the current instant produces
// numbers that do not match what the customer means by last week and
// cannot be added to the neighbouring period. Whole local days are what
// makes the timezone setting more than cosmetic.

// SitePathPrefix is where a site's numbers live: /site/{site}.
//
// The member list already lives under this prefix as
// /site/{site}/uyeler, so the dashboard takes the bare path - a site's
// own page, with its sections beneath it.
const dashboardPathSuffix = ""

// cardSource says which of the two data sources a card reads.
//
// It decides what an empty card means, which is the whole reason the
// distinction is carried this far up: a site with no beacon rows and a
// working collector is an ordinary deployment, not a fault.
type cardSource string

const (
	sourceTraffic cardSource = "trafik"
	sourceBeacon  cardSource = "beacon"
)

// cardID is the closed set of things this panel can show.
type cardID string

const (
	cardVisitors   cardID = "ziyaretci"
	cardPageviews  cardID = "goruntuleme"
	cardSessions   cardID = "oturum"
	cardBounceRate cardID = "hemen_cikma"
	cardHumanIPs   cardID = "insan"
	cardBotIPs     cardID = "bot"
)

// cardDef describes one card.
//
// Value takes the formatter rather than returning a number, because
// these are not all the same kind of quantity: a count, a ratio and a
// duration format differently and in a way that depends on the reader's
// language. Handing back a float and letting the template guess is how a
// bounce rate ends up rendered as "0.42".
type cardDef struct {
	ID     cardID
	Source cardSource
	// Value is the figure as text, already formatted for this language.
	Value func(analytics.Dashboard, *ui.Formatter) string
}

// cards is every card this panel knows how to draw.
//
// Adding one is a code change that goes through review - the same rule
// the settings registry follows, for the same reason: a running system
// should not be talkable into rendering a figure nobody designed.
var cards = map[cardID]cardDef{
	cardVisitors: {
		ID: cardVisitors, Source: sourceBeacon,
		Value: func(d analytics.Dashboard, f *ui.Formatter) string {
			return f.Number(int64(d.Beacon.Visitors))
		},
	},
	cardPageviews: {
		ID: cardPageviews, Source: sourceBeacon,
		Value: func(d analytics.Dashboard, f *ui.Formatter) string {
			return f.Number(int64(d.Beacon.Pageviews))
		},
	},
	cardSessions: {
		ID: cardSessions, Source: sourceBeacon,
		Value: func(d analytics.Dashboard, f *ui.Formatter) string {
			return f.Number(int64(d.Beacon.Sessions))
		},
	},
	cardBounceRate: {
		ID: cardBounceRate, Source: sourceBeacon,
		Value: func(d analytics.Dashboard, f *ui.Formatter) string {
			return f.Percent(d.Beacon.BounceRate, 0)
		},
	},
	cardHumanIPs: {
		ID: cardHumanIPs, Source: sourceTraffic,
		Value: func(d analytics.Dashboard, f *ui.Formatter) string {
			return f.Number(int64(d.Traffic.HumanIPs))
		},
	},
	cardBotIPs: {
		ID: cardBotIPs, Source: sourceTraffic,
		Value: func(d analytics.Dashboard, f *ui.Formatter) string {
			return f.Number(int64(d.Traffic.BotIPs))
		},
	},
}

// defaultCards is what a deployment shows before anybody configures it.
//
// Four a customer recognises from any analytics tool, and two that are
// the reason this product exists at all: the collector counts the
// visitors no JavaScript-based tool can see, and separates the automated
// ones from the people. Ordered so the familiar four come first - the
// point of the last two is best made to somebody who has already found
// their footing on the page.
var defaultCards = []cardID{
	cardVisitors, cardPageviews, cardSessions, cardBounceRate,
	cardHumanIPs, cardBotIPs,
}

// emptiness is why a card has no number, which is three different facts
// wearing one appearance.
//
// Collapsing them into "no data" would present an installation step
// nobody performed as though it were a measurement - the distinction
// §D5 draws between "we stopped collecting" and "we never collected",
// applied one level down.
type emptiness string

const (
	// hasData means the number is real, including when it is zero.
	hasData emptiness = ""
	// neverInstalled means this source has never written a row for this
	// site. For the beacon that is the usual cause: no snippet.
	neverInstalled emptiness = "kurulmamis"
	// nothingInRange means the source is installed and this period is
	// genuinely empty. A measurement, not a gap.
	nothingInRange emptiness = "bos"
	// unreachable means the read API did not answer.
	unreachable emptiness = "ulasilamiyor"
	// refused means it answered and would not serve this token.
	refused emptiness = "reddedildi"
)

// cardView is one card as the template receives it.
type cardView struct {
	ID    cardID
	Label string
	Help  string
	Value string
	// Empty is why there is no number. hasData means Value is real.
	Empty emptiness
	// EmptyText is the sentence for Empty, already in the reader's
	// language.
	EmptyText string
}

// dashboardPage is Data for the template.
type dashboardPage struct {
	SiteID string
	Cards  []cardView
	// Sections are the breakdowns beneath the cards: which pages, from
	// where, which campaign. A card without one of these is a number a
	// customer can read and not act on.
	Sections []breakdownView
	// Scores and Crossover are the developer-mode views that are not
	// breakdowns. Drawn only when technical is true; a zero value renders
	// nothing, which is what the template checks.
	Scores    scoreView
	Crossover crossoverView
	// Technical says whether those two were asked for at all, so the
	// template can tell "developer mode is off" from "developer mode is
	// on and the collector has nothing".
	Technical bool
	// From and To are the range these numbers cover, in the panel's
	// zone, shown because a figure with no period attached is not a
	// figure.
	From time.Time
	To   time.Time
	// Ranges are the choices offered, with the current one marked.
	Ranges []rangeChoice
	// Notice is a page-wide problem: the API is unreachable, so no card
	// has anything. Per-card emptiness is on the card.
	Notice string
}

// rangeChoice is one option in the period picker.
type rangeChoice struct {
	Days     int
	Label    string
	URL      string
	Selected bool
}

// rangeDays are the periods offered.
//
// Whole days only, and short ones: the API caps a range at ninety days
// and the honest reason to keep the list short is that a picker with
// eleven options is a picker nobody reads. Ninety is the last entry
// because it matches the default retention period - offering a range
// longer than the data is kept would produce a page that quietly starts
// midway.
var rangeDays = []int{1, 7, 30, 90}

const defaultRangeDays = 7

// dashboardHandler draws one site's numbers.
func (s *Server) dashboardHandler(w http.ResponseWriter, r *http.Request) {
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
	// The only thing between one customer's numbers and another's. The
	// panel's API token reads every site, so this check is not a
	// convenience - it is the boundary.
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
	from, to := wholeDays(time.Now().In(s.zone(r.Context())), days)

	data := s.dashboardData(r.Context(), lang, siteID, days, from, to, access.ShowsTechnical())
	page := s.page(r, lang, access, "siteler", siteID)
	page.Site = ui.SiteView{ID: siteID, Name: s.siteName(r.Context(), access, siteID)}
	page.Heading = page.Site.Name
	page.Data = data
	if data.Notice != "" {
		page.Notices = append(page.Notices, ui.Notice{Level: ui.NoticeWarn, Body: data.Notice})
	}
	page.Notices = append(page.Notices, viewerNotice(lang, access)...)
	s.Renderer.Render(w, r, http.StatusOK, "pano", page)
}

// dashboardData fetches and shapes the page.
// technical says whether to draw the developer-mode sections. It is
// Access.ShowsTechnical - both the role and the preference - rather than
// the role alone, because this is the page that appears without anybody
// asking for it, and D6's rule is that the default view carries no
// fingerprints and no jargon. The detail pages take the role alone; see
// detailHandler for why the two differ.
func (s *Server) dashboardData(ctx context.Context, lang *ui.Language,
	siteID string, days int, from, to time.Time, technical bool) dashboardPage {

	data := dashboardPage{SiteID: siteID, From: from, To: to}
	for _, d := range rangeDays {
		data.Ranges = append(data.Ranges, rangeChoice{
			Days:     d,
			Label:    lang.Tn("pano.aralik.gun", d, strconv.Itoa(d)),
			URL:      sitePath(siteID) + "?gun=" + strconv.Itoa(d),
			Selected: d == days,
		})
	}

	f := ui.NewFormatter(lang, s.zone(ctx))

	// What this customer chose to see, and nothing else. The request is
	// built from that set rather than from the registry, so a block that
	// is off is not merely hidden - its call is never made.
	//
	// One call for the whole page: the summaries and the sections
	// together, concurrently, under one deadline. Fetching the cards and
	// then the sections would bound the page at twice PageTimeout while
	// reading, here, as though it were bounded by one.
	shown := s.visible(ctx, siteID)
	// The technical sections join the visible set here, before the request
	// is built, because they have to be *fetched* as well as drawn -
	// appending them further down would have rendered three sections whose
	// every call was never made.
	//
	// Appended rather than configured: C6 lets a deployment choose which
	// blocks its customer sees, and these are not among them. They are not
	// a customer's choice to make, they are a different reader's page, and
	// putting "fingerprints" on a visibility screen would offer a setting
	// to somebody who will never see its effect either way.
	if technical {
		shown.Breakdowns = append(append([]analytics.BreakdownKind(nil), shown.Breakdowns...), technicalBreakdowns...)
	}
	req := shown.request(sectionRows)
	site := s.Analytics.FetchSite(ctx, siteID, from, to, req)
	board := site.Dashboard

	// Every source this page actually asked for failing the same way is a
	// page-wide problem rather than six identical card messages.
	//
	// "Asked for" and not "both", now that a page can skip one: a summary
	// nobody wanted comes back zero with a nil error, so testing both
	// unconditionally would silence the notice on a page that shows only
	// beacon cards - exactly the page a customer with no collector has.
	if failed, err := allRequestedFailed(req, board); failed {
		switch {
		case errors.Is(err, analytics.ErrRefused):
			data.Notice = lang.T("pano.hata.reddedildi")
		default:
			data.Notice = lang.T("pano.hata.ulasilamiyor")
		}
	}

	// Asked only when something came back empty, and only once for the
	// page however many cards need it - and only about sources this page
	// fetched, so a skipped summary does not buy a lookup nothing reads.
	presence := s.presence(ctx, req, board, siteID)

	for _, id := range shown.Cards {
		def, ok := cards[id]
		if !ok {
			// Unreachable: visibleCards drops unknown ids and logs them.
			// Kept because rendering a blank card is the worse failure.
			continue
		}
		view := cardView{
			ID:    id,
			Label: lang.T("pano.kart." + string(id) + ".baslik"),
			Help:  lang.T("pano.kart." + string(id) + ".aciklama"),
		}
		view.Empty = emptinessFor(def.Source, board, presence)
		if view.Empty == hasData {
			view.Value = def.Value(board, f)
		} else {
			view.EmptyText = lang.T("pano.bos." + string(view.Empty) + "." + string(def.Source))
		}
		data.Cards = append(data.Cards, view)
	}
	data.Sections = s.sections(lang, f, siteID, site, presence, days, shown.Breakdowns)

	// The histogram and the coverage summary. Fetched separately from the
	// breakdowns rather than folded into FetchSite: they are not
	// breakdown-shaped, and a request type that carried both would have
	// one field set for nine callers and three for one.
	if technical {
		tech := s.Analytics.FetchTechnical(ctx, siteID, from, to, analytics.TechnicalRequest{
			Scores: true, Crossover: true,
		})
		data.Technical = true
		data.Scores, data.Crossover = s.technicalSections(lang, f, siteID, tech, board, presence, days)
	}
	if shown.Empty() {
		// Somebody turned everything off. Said in a sentence rather than
		// left as a heading over blank space, which reads as a fault and
		// sends a customer to support for a setting.
		data.Notice = lang.T("pano.hicbiri")
	}
	return data
}

// sourcePresence records whether each source has ever written for a
// site, and whether the question could be answered at all.
type sourcePresence struct {
	known          bool
	traffic, bacon bool
}

// allRequestedFailed reports whether every summary this page asked for
// came back failed, and with which error.
//
// A page that asked for neither is not a failed page: it draws
// breakdowns and their own errors, which are carried per section.
func allRequestedFailed(req analytics.SiteRequest, board analytics.Dashboard) (bool, error) {
	var asked int
	var failed int
	var first error
	if req.Traffic {
		asked++
		if board.TrafficErr != nil {
			failed++
			first = board.TrafficErr
		}
	}
	if req.Beacon {
		asked++
		if board.BeaconErr != nil {
			failed++
			if first == nil {
				first = board.BeaconErr
			}
		}
	}
	return asked > 0 && asked == failed, first
}

// presence answers "has this source ever seen this site" - once, and
// only when something is empty.
func (s *Server) presence(ctx context.Context, req analytics.SiteRequest,
	board analytics.Dashboard, siteID string) sourcePresence {

	// A summary that was not fetched is zero with no error, which is
	// exactly what "installed and empty" looks like. Counting it as empty
	// would buy a KnownSites round trip for a source nothing on the page
	// reads - the one thing this whole setting exists to stop.
	trafficEmpty := req.Traffic && board.TrafficErr == nil && board.Traffic.Snapshots == 0
	beaconEmpty := req.Beacon && board.BeaconErr == nil &&
		board.Beacon.Pageviews == 0 && board.Beacon.Events == 0
	if !trafficEmpty && !beaconEmpty {
		return sourcePresence{}
	}

	traffic, beacon, err := s.Analytics.KnownSites(ctx, trafficEmpty, beaconEmpty)
	if err != nil {
		// Unknown rather than assumed. Telling somebody to install a
		// snippet they already have is worse than saying only that the
		// period is empty.
		s.logger().Warn("panel: listing known sites", "err", err)
		return sourcePresence{}
	}
	// A list that was not asked for reports "not seen", which is only
	// ever read for the source that was empty in the first place - the
	// caller decides emptiness per source and never consults the other.
	return sourcePresence{
		known:   true,
		traffic: slices.Contains(traffic, siteID),
		bacon:   slices.Contains(beacon, siteID),
	}
}

// emptinessFor decides what one card says instead of a number.
//
// The order is the order of what is actually known: a failure first,
// because a number that was never fetched is not zero; then whether the
// source has ever written, because that is a different fact from an
// empty period; and only then the period itself.
func emptinessFor(source cardSource, board analytics.Dashboard, seen sourcePresence) emptiness {
	var (
		err       error
		isEmpty   bool
		everWrote bool
	)
	switch source {
	case sourceBeacon:
		err = board.BeaconErr
		isEmpty = board.Beacon.Pageviews == 0 && board.Beacon.Events == 0
		everWrote = seen.bacon
	default:
		err = board.TrafficErr
		isEmpty = board.Traffic.Snapshots == 0
		everWrote = seen.traffic
	}

	switch {
	case errors.Is(err, analytics.ErrRefused):
		return refused
	case err != nil:
		return unreachable
	case !isEmpty:
		return hasData
	case seen.known && !everWrote:
		return neverInstalled
	default:
		return nothingInRange
	}
}

// rangeFrom reads the period from the query string.
//
// A closed set rather than an arbitrary number of days: this value ends
// up in a request to another service, and the honest bound is the list
// the page itself offers. Anything else takes the default rather than
// being an error - a mistyped URL should show the dashboard, not a
// refusal.
func (s *Server) rangeFrom(r *http.Request) int {
	raw := r.URL.Query().Get("gun")
	for _, days := range rangeDays {
		if raw == strconv.Itoa(days) {
			return days
		}
	}
	return defaultRangeDays
}

// wholeDays returns the range covering the last n whole local days,
// ending at the end of the day now falls in.
//
// Local rather than UTC, and whole days rather than "now minus n×24h",
// because sessions are counted inside the range: one that began before
// it is truncated at the boundary. A range that starts at 14:37 cuts
// every session running at 14:37 on that day, in both the period being
// shown and the one before it - so neighbouring periods cannot be added
// up and neither matches what the customer means by "yesterday".
func wholeDays(now time.Time, days int) (from, to time.Time) {
	loc := now.Location()
	startOfToday := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	// The end of today rather than this instant: a person looking at
	// "today" wants today, and the API has nothing to report for the
	// hours that have not happened yet.
	to = startOfToday.AddDate(0, 0, 1)
	from = startOfToday.AddDate(0, 0, -(days - 1))
	return from, to
}

// siteName is what the customer calls this site, falling back to its id.
func (s *Server) siteName(ctx context.Context, access panel.Access, siteID string) string {
	if s.Store == nil {
		return siteID
	}
	value, err := s.Store.GetSetting(ctx, panel.KeySiteName, siteID)
	if err != nil {
		s.logger().Error("panel: reading site name", "err", err, "site", siteID)
		return siteID
	}
	if name, ok := value.(string); ok && name != "" {
		return name
	}
	return siteID
}

// sitePath is a site's dashboard URL.
func sitePath(siteID string) string {
	if siteID == "" {
		return ""
	}
	return MembersPathPrefix + url.PathEscape(siteID)
}
