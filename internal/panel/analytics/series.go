package analytics

import (
	"context"
	"net/url"
	"time"
)

// The one endpoint the panel never asked for.
//
// The API has answered /beacon/timeseries since the day it was written.
// Nothing in the panel called it, so for every period the customer could
// pick, the page showed six totals and no shape at all: "1 479
// pageviews in seven days" with no way to see whether that was steady,
// climbing, or one good Tuesday and six quiet days.
//
// # Why the interval is chosen here and not by the caller
//
// The API accepts seven bucket widths and the right one depends
// entirely on the range: an hourly series over ninety days is 2 160
// points in a box 720 units wide, and a daily series over one day is a
// single point. Leaving the choice to each caller means the choice gets
// made twice, differently, and the second one is made by whoever adds
// the next page.
//
// So Interval maps a period to its width, aiming at roughly 24 to 90
// buckets - enough for a shape, few enough that each one is a pixel a
// person can see.
//
// # What the API does not send
//
// Buckets with no rows in them. That is correct for a JSON list and
// wrong for anything that draws it: measured against this project's own
// demo data, a seven-day hourly series came back with 144 buckets
// across a 169-hour span. The twenty-five absent hours were every quiet
// night in the week. Filling them is the drawing layer's job and it is
// documented where it happens, in internal/panel/ui.Build.

// Bucket is one interval of measured activity.
type Bucket struct {
	Time      time.Time `json:"time"`
	Pageviews int       `json:"pageviews"`
	Visitors  int       `json:"visitors"`
	Sessions  int       `json:"sessions"`
}

// Series is a site's activity over time, with the bucket width it was
// measured at.
//
// Step travels with the buckets deliberately. A drawing layer that
// filled the gaps using an interval it assumed rather than the one the
// data was measured at would place every point after the first gap in
// the wrong place, and the chart would look entirely reasonable.
type Series struct {
	Buckets []Bucket
	Step    time.Duration
	Err     error
}

// intervalChoice is one bucket width: what the API is asked for, and how
// long it actually is.
//
// The string and the duration are one table entry rather than two,
// because they are the same fact said twice and the failure when they
// disagree is silent - the API buckets by one width and the chart fills
// gaps by another.
type intervalChoice struct {
	name string
	step time.Duration
}

// intervals maps a period, in days, to the bucket width to ask for.
// Ordered longest-first so the lookup takes the first that fits.
var intervals = []struct {
	upToDays int
	choice   intervalChoice
}{
	{1, intervalChoice{"1 hour", time.Hour}},
	{7, intervalChoice{"6 hours", 6 * time.Hour}},
	{90, intervalChoice{"1 day", 24 * time.Hour}},
}

// fallbackInterval covers a range longer than any row above. A week is
// the coarsest the API offers, and a range that needs it is one nothing
// currently asks for.
var fallbackInterval = intervalChoice{"1 week", 7 * 24 * time.Hour}

// Interval picks the bucket width for a range of this many days.
func Interval(days int) (name string, step time.Duration) {
	for _, row := range intervals {
		if days <= row.upToDays {
			return row.choice.name, row.choice.step
		}
	}
	return fallbackInterval.name, fallbackInterval.step
}

// series fetches one site's activity over time.
func (c *Client) series(ctx context.Context, site string, from, to time.Time,
	interval string, step time.Duration) Series {

	out := Series{Step: step}
	var body struct {
		Buckets []Bucket `json:"buckets"`
	}
	extra := url.Values{"interval": []string{interval}}
	if err := c.get(ctx, "/api/v1/sites/"+url.PathEscape(site)+"/beacon/timeseries",
		from, to, extra, &body); err != nil {
		out.Err = err
		return out
	}
	out.Buckets = body.Buckets
	return out
}
