package web

import (
	"errors"
	"slices"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/panel/analytics"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
)

// The shape behind the totals.
//
// # What the page said before this
//
// Six numbers and nine tables. "1 479 sayfa görüntüleme" over seven
// days, and no way to ask the only question a person actually has after
// reading it: is that steady, is it climbing, or was it one good
// Tuesday and six quiet days. The answer was in the database and in the
// API - /beacon/timeseries has existed since the API was written - and
// no page had ever called it.
//
// # Why the chart is above the cards
//
// A reader who takes in the totals first has formed an impression by
// the time the shape appears, and the shape then has to argue with it.
// Traffic that halved on Thursday and a total that reads "1 479" are
// the same week; whichever is read first is the one that is believed.
//
// # Why it is not a card
//
// The cards are one number each, and a card set is a row of comparable
// things. A chart in that row is a card that is six cards wide and says
// something of a different kind, which is how a layout starts having
// exceptions in it.

// chartSeriesKeys names the two lines, in drawing order. The first is
// the one filled underneath.
//
// Pageviews first because it is the larger of the two by definition -
// every visitor produces at least one - so the filled area never hides
// the line drawn over it. That is arithmetic rather than taste: the
// reverse order would put the visitor area on top of the pageview line
// for every site that has ever existed.
var chartSeriesKeys = []string{"goruntuleme", "ziyaretci"}

// chartPlots reports whether a card is one the chart draws over time.
//
// The two are written as one list rather than as a condition in the
// handler, because they are two claims that have to agree: what the
// chart plots, and what makes the chart worth fetching. Reading the
// series keys means the second cannot drift from the first - adding a
// third line to the chart makes its card count here without anybody
// remembering to.
func chartPlots(id cardID) bool {
	return slices.Contains(chartSeriesKeys, string(id))
}

// chart lays out the dashboard's time chart, and says why there is not
// one when there is not.
//
// The emptiness vocabulary is the cards', not a second one. A period
// with no rows is a measurement and draws axes with a sentence in them;
// an unreachable API draws nothing, because an empty chart over a
// failed call is a picture asserting that nothing happened.
func (s *Server) chart(lang *ui.Language, f *ui.Formatter, req analytics.SiteRequest,
	site analytics.Site, presence sourcePresence, now, from, to time.Time) (ui.Chart, emptiness) {

	if req.SeriesDays == 0 {
		// Nothing asked for a series, so there is no chart and no
		// failure. Reported as unreachable would put a warning on a page
		// whose owner turned the beacon blocks off deliberately.
		return ui.Chart{}, unreachable
	}
	switch {
	case errors.Is(site.Series.Err, analytics.ErrRefused):
		return ui.Chart{}, refused
	case site.Series.Err != nil:
		return ui.Chart{}, unreachable
	}

	// The same three-way distinction the cards draw, asked of the same
	// source: a beacon that has never written a row for this site is a
	// snippet nobody embedded, and drawing an empty chart for it would
	// present a setup step as a result.
	if len(site.Series.Buckets) == 0 {
		if presence.known && !presence.bacon {
			return ui.Chart{}, neverInstalled
		}
		return ui.Chart{}, nothingInRange
	}

	views := make([]ui.ChartPoint, 0, len(site.Series.Buckets))
	visitors := make([]ui.ChartPoint, 0, len(site.Series.Buckets))
	for _, b := range site.Series.Buckets {
		views = append(views, ui.ChartPoint{At: b.Time, Value: b.Pageviews})
		visitors = append(visitors, ui.ChartPoint{At: b.Time, Value: b.Visitors})
	}

	// The x-axis is read in the panel's zone, like every other time on
	// the page. The buckets arrive in UTC; a chart labelled 03:00 beside
	// a range that says "3 Eylül" in Europe/Istanbul is two clocks on
	// one screen.
	zone := f.Location()
	local := func(t time.Time) time.Time { return t.In(zone) }

	c := ui.Build(ui.ChartInput{
		From: local(from),
		To:   local(to),
		Step: site.Series.Step,
		Series: []ui.ChartSeries{
			{
				Key:    chartSeriesKeys[0],
				Label:  lang.T("pano.kart.goruntuleme.baslik"),
				Points: inZone(views, zone),
			},
			{
				Key:    chartSeriesKeys[1],
				Label:  lang.T("pano.kart.ziyaretci.baslik"),
				Points: inZone(visitors, zone),
			},
		},
		Now:    now.In(zone),
		Label:  lang.T("pano.grafik.baslik"),
		Number: f.Number,
		Time:   chartTickLabel(f, req.SeriesDays),
	})
	if c.Empty {
		// Buckets came back and every one of them is zero. That is the
		// bots filter having removed everything, or a period whose rows
		// are all events rather than pageviews - a measurement either
		// way, and the same sentence a card would use.
		return c, nothingInRange
	}
	return c, hasData
}

// inZone moves each point into the panel's zone.
//
// Not cosmetic: the chart fills its gaps by stepping from From, and
// From is local. A point still in UTC would land in the bucket three
// hours away from the one it belongs to, and the chart would be wrong
// by exactly the offset - which looks like a chart, not like an error.
func inZone(points []ui.ChartPoint, zone *time.Location) []ui.ChartPoint {
	for i := range points {
		points[i].At = points[i].At.In(zone)
	}
	return points
}

// chartTickLabel picks how a time label reads for this period.
//
// A ninety-day chart labelled with clock times says nothing, and a
// one-day chart labelled with dates says the same date five times. The
// threshold is the period rather than the bucket width because the
// question is what the reader is looking at, not how it was measured.
func chartTickLabel(f *ui.Formatter, days int) func(time.Time) string {
	if days <= 1 {
		return f.Clock
	}
	return f.ShortDate
}
