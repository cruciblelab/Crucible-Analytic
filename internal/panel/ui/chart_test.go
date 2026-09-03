package ui

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The chart's arithmetic, checked without a browser.
//
// Everything here is geometry, so everything here is checkable by
// reading numbers out of a path. That is the reason the layout lives in
// Go rather than in a script the page runs: a chart drawn in the
// browser is a chart whose only test is somebody looking at it.

func hours(n int) time.Duration { return time.Duration(n) * time.Hour }

var chartFrom = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func at(h int) time.Time { return chartFrom.Add(hours(h)) }

// pathPoints pulls the coordinates back out of an SVG path so a test
// can assert about them as numbers.
var pathPoint = regexp.MustCompile(`[ML](-?[0-9.]+) (-?[0-9.]+)`)

func pointsOf(t *testing.T, d string) [][2]float64 {
	t.Helper()
	var out [][2]float64
	for _, m := range pathPoint.FindAllStringSubmatch(d, -1) {
		x, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			t.Fatalf("path has an unparsable x %q: %v", m[1], err)
		}
		y, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			t.Fatalf("path has an unparsable y %q: %v", m[2], err)
		}
		out = append(out, [2]float64{x, y})
	}
	return out
}

// TestAMissingBucketIsDrawnAsZeroAndNotSkipped is the headline.
//
// The analytics API returns only the buckets that have rows. A chart
// that plots the points it was given, evenly spaced, turns every quiet
// hour into no time at all: this project's own demo data came back with
// 144 buckets across a 169-hour span, and the twenty-five missing hours
// were every small-hours gap in the week.
//
// So the assertion is not "the gap is handled" but the specific shape:
// the point at the missing hour exists, sits at the baseline, and sits
// at the x the clock says rather than the x the array index says.
func TestAMissingBucketIsDrawnAsZeroAndNotSkipped(t *testing.T) {
	c := Build(ChartInput{
		From: chartFrom,
		To:   chartFrom.Add(hours(4)),
		Step: time.Hour,
		Series: []ChartSeries{{
			Key: "goruntuleme",
			// Hour 1 and hour 2 are missing, exactly as the API sends
			// them: absent, not zero.
			Points: []ChartPoint{{At: at(0), Value: 10}, {At: at(3), Value: 10}},
		}},
	})

	pts := pointsOf(t, c.Lines[0].Path)
	if len(pts) != 4 {
		t.Fatalf("the chart drew %d points for a four-hour range; the missing hours "+
			"were skipped rather than drawn as the zeroes they are", len(pts))
	}

	baseline := c.PlotTop + c.PlotHeight
	for _, i := range []int{1, 2} {
		if math.Abs(pts[i][1]-baseline) > 0.05 {
			t.Errorf("hour %d is at y=%.1f; the baseline is %.1f. An hour with no rows "+
				"is an hour with no pageviews, and the line has to reach the floor",
				i, pts[i][1], baseline)
		}
	}

	// And the two hours that do have data are at the ends, not adjacent.
	if math.Abs(pts[0][0]-c.PlotLeft) > 0.05 {
		t.Errorf("the first point is at x=%.1f, want the plot's left edge %.1f", pts[0][0], c.PlotLeft)
	}
	right := c.PlotLeft + c.PlotWidth
	if math.Abs(pts[3][0]-right) > 0.05 {
		t.Errorf("the last point is at x=%.1f, want the plot's right edge %.1f", pts[3][0], right)
	}
}

// TestAQuietStartStillOccupiesItsPartOfTheAxis.
//
// The other half of the same mistake, and the one a "skip the gaps"
// implementation passes the test above while still getting wrong: if
// the walk began at the first point rather than at From, a morning with
// no traffic would not be a flat morning - it would not be on the chart
// at all, and the afternoon would stretch across the whole width.
func TestAQuietStartStillOccupiesItsPartOfTheAxis(t *testing.T) {
	c := Build(ChartInput{
		From:   chartFrom,
		To:     chartFrom.Add(hours(8)),
		Step:   time.Hour,
		Series: []ChartSeries{{Key: "goruntuleme", Points: []ChartPoint{{At: at(6), Value: 40}}}},
	})

	pts := pointsOf(t, c.Lines[0].Path)
	if len(pts) != 8 {
		t.Fatalf("an eight-hour range drew %d points", len(pts))
	}
	if pts[0][0] != c.PlotLeft {
		t.Errorf("the chart starts at x=%.1f rather than the left edge", pts[0][0])
	}
	// The one busy hour is the seventh of eight, so it belongs at 6/7 of
	// the width - not at the start, and not filling the chart.
	want := c.PlotLeft + c.PlotWidth*6/7
	if math.Abs(pts[6][0]-want) > 0.05 {
		t.Errorf("the busy hour is at x=%.1f, want %.1f. Six empty hours have to take "+
			"up six hours of the axis", pts[6][0], want)
	}
}

// TestAPointOutsideTheRangeIsDroppedRatherThanClamped.
//
// Clamping is the tempting repair, and it is the wrong one: a point
// from before the range piled onto the first bucket draws a spike on a
// morning that was quiet.
func TestAPointOutsideTheRangeIsDroppedRatherThanClamped(t *testing.T) {
	c := Build(ChartInput{
		From: chartFrom,
		To:   chartFrom.Add(hours(3)),
		Step: time.Hour,
		Series: []ChartSeries{{Key: "goruntuleme", Points: []ChartPoint{
			{At: chartFrom.Add(-hours(5)), Value: 900},
			{At: at(1), Value: 10},
			{At: chartFrom.Add(hours(50)), Value: 900},
		}}},
	})

	if c.Max != 10 {
		t.Errorf("the axis tops out at %d; the 900s are outside the range and must not "+
			"reach the scale, let alone the line", c.Max)
	}
	pts := pointsOf(t, c.Lines[0].Path)
	baseline := c.PlotTop + c.PlotHeight
	if math.Abs(pts[0][1]-baseline) > 0.05 {
		t.Errorf("the first bucket is at y=%.1f rather than the baseline; a point from "+
			"before the range was clamped onto it", pts[0][1])
	}
}

// TestTwoPointsInOneBucketAreAdded.
//
// A caller passing a finer interval than it declared is a bug, and the
// honest failure is a total that still adds up rather than a value that
// silently replaced another.
func TestTwoPointsInOneBucketAreAdded(t *testing.T) {
	c := Build(ChartInput{
		From: chartFrom,
		To:   chartFrom.Add(hours(2)),
		Step: time.Hour,
		Series: []ChartSeries{{Key: "goruntuleme", Points: []ChartPoint{
			{At: chartFrom, Value: 3},
			{At: chartFrom.Add(30 * time.Minute), Value: 4},
		}}},
	})
	if c.Lines[0].Total != 7 {
		t.Errorf("the series totals %d; two points inside one bucket have to add to 7",
			c.Lines[0].Total)
	}
}

// TestTheAxisTopIsANumberSomebodyCanRead.
//
// The step is chosen first and the top is a whole number of steps, so
// every gridline label is an integer and the lines are evenly spaced.
// The cases are the ones that actually appear: a real pageview total, a
// handful, and zero.
func TestTheAxisTopIsANumberSomebodyCanRead(t *testing.T) {
	for _, tc := range []struct {
		peak, top, step int
	}{
		{1479, 1500, 500},
		{1, 1, 1},
		{4, 4, 1},
		{5, 5, 1},
		{12, 15, 5},
		{21, 25, 5},
		{60, 60, 20},
		{0, 1, 1},
	} {
		top, step := niceAxis(tc.peak)
		if top != tc.top || step != tc.step {
			t.Errorf("a peak of %d gives top %d step %d, want top %d step %d",
				tc.peak, top, step, tc.top, tc.step)
		}
		if top < tc.peak {
			t.Errorf("a peak of %d gives an axis top of %d, which is below the data", tc.peak, top)
		}
		if top%step != 0 {
			t.Errorf("a peak of %d gives top %d and step %d, which do not divide: the "+
				"gridlines would be unevenly spaced under evenly spaced labels",
				tc.peak, top, step)
		}
	}
}

// TestTheGridlinesAreLabelledWithTheReadersOwnNumbers.
//
// The chart is drawn on the server, so its labels go through the same
// formatter as every other number on the page. A gridline reading
// "2000" beside a card reading "2.000" is two number systems on one
// screen.
func TestTheGridlinesAreLabelledWithTheReadersOwnNumbers(t *testing.T) {
	c := Build(ChartInput{
		From:   chartFrom,
		To:     chartFrom.Add(hours(4)),
		Step:   time.Hour,
		Series: []ChartSeries{{Key: "goruntuleme", Points: []ChartPoint{{At: chartFrom, Value: 1479}}}},
		Number: func(v int64) string { return "«" + strconv.FormatInt(v, 10) + "»" },
	})
	if len(c.Grid) < 3 {
		t.Fatalf("the chart drew %d gridlines", len(c.Grid))
	}
	for _, g := range c.Grid {
		if !strings.HasPrefix(g.Value, "«") {
			t.Errorf("a gridline is labelled %q, which did not go through the "+
				"formatter it was given", g.Value)
		}
	}
	if c.Grid[len(c.Grid)-1].Value != "«1500»" {
		t.Errorf("the top gridline reads %q, want the rounded axis top",
			c.Grid[len(c.Grid)-1].Value)
	}
}

// TestAnEmptyPeriodSaysSoRatherThanDrawingAFlatLineAtTheTop.
//
// Every value zero is a measurement. The failure this guards is the
// division: an axis top of zero makes every y a NaN, and a path full of
// NaN renders as nothing at all with no error anywhere.
func TestAnEmptyPeriodSaysSoRatherThanDrawingAFlatLineAtTheTop(t *testing.T) {
	c := Build(ChartInput{
		From:   chartFrom,
		To:     chartFrom.Add(hours(6)),
		Step:   time.Hour,
		Series: []ChartSeries{{Key: "goruntuleme"}},
	})
	if !c.Empty {
		t.Error("a period with no rows did not report itself empty")
	}
	if strings.Contains(c.Lines[0].Path, "NaN") {
		t.Errorf("the path contains NaN: %q. Dividing by an axis top of zero renders "+
			"as an invisible chart with nothing in any log", c.Lines[0].Path)
	}
	baseline := c.PlotTop + c.PlotHeight
	for _, p := range pointsOf(t, c.Lines[0].Path) {
		if math.Abs(p[1]-baseline) > 0.05 {
			t.Errorf("an empty series drew a point at y=%.1f rather than on the floor", p[1])
		}
	}
}

// TestOnlyTheFirstSeriesIsFilled.
//
// Two translucent areas overlapping make the region where they cross a
// third colour that stands for nothing.
func TestOnlyTheFirstSeriesIsFilled(t *testing.T) {
	c := Build(ChartInput{
		From: chartFrom,
		To:   chartFrom.Add(hours(3)),
		Step: time.Hour,
		Series: []ChartSeries{
			{Key: "goruntuleme", Points: []ChartPoint{{At: chartFrom, Value: 5}}},
			{Key: "ziyaretci", Points: []ChartPoint{{At: chartFrom, Value: 3}}},
		},
	})
	if c.Lines[0].Area == "" {
		t.Error("the first series has no area to fill")
	}
	if c.Lines[1].Area != "" {
		t.Error("the second series is filled too; two overlapping areas make a third " +
			"colour that means nothing")
	}
	if !strings.HasSuffix(c.Lines[0].Area, " Z") {
		t.Errorf("the area path is not closed: %q", c.Lines[0].Area)
	}
}

// TestThePathIsNotWrittenAtFloat64Precision.
//
// %v on a float64 writes seventeen significant digits. A 720-point path
// written that way is tens of kilobytes on a page whose whole
// stylesheet is fourteen, and it is invisible in review because the
// path is one long line either way.
func TestThePathIsNotWrittenAtFloat64Precision(t *testing.T) {
	var points []ChartPoint
	for i := 0; i < 168; i++ {
		points = append(points, ChartPoint{At: at(i), Value: i*7 + 1})
	}
	c := Build(ChartInput{
		From:   chartFrom,
		To:     chartFrom.Add(hours(168)),
		Step:   time.Hour,
		Series: []ChartSeries{{Key: "goruntuleme", Points: points}},
	})
	if n := len(c.Lines[0].Path); n > 3000 {
		t.Errorf("a 168-point path is %d bytes; one decimal place per coordinate "+
			"should keep it under 3 000", n)
	}
	for _, m := range pathPoint.FindAllStringSubmatch(c.Lines[0].Path, -1) {
		for _, v := range m[1:] {
			if dot := strings.IndexByte(v, '.'); dot >= 0 && len(v)-dot-1 > 1 {
				t.Fatalf("coordinate %q has more than one decimal place", v)
			}
		}
	}
}

// TestABadRangeGivesAnEmptyChartRatherThanAPanic.
//
// The chart is decoration around numbers that are already on the page.
// Nothing about it justifies taking the page down, so every shape that
// cannot be drawn has to end in a box rather than in a recover.
func TestABadRangeGivesAnEmptyChartRatherThanAPanic(t *testing.T) {
	for name, in := range map[string]ChartInput{
		"to before from":   {From: chartFrom, To: chartFrom.Add(-hours(1)), Step: time.Hour},
		"zero step":        {From: chartFrom, To: chartFrom.Add(hours(4))},
		"negative step":    {From: chartFrom, To: chartFrom.Add(hours(4)), Step: -time.Hour},
		"empty range":      {From: chartFrom, To: chartFrom, Step: time.Hour},
		"absurdly fine":    {From: chartFrom, To: chartFrom.Add(hours(24)), Step: time.Millisecond},
		"no series at all": {From: chartFrom, To: chartFrom.Add(hours(4)), Step: time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			c := Build(in)
			if !c.Empty {
				t.Error("an undrawable chart did not report itself empty")
			}
			for _, line := range c.Lines {
				if strings.Contains(line.Path, "NaN") || strings.Contains(line.Path, "Inf") {
					t.Errorf("the path contains a non-number: %q", line.Path)
				}
			}
		})
	}
}

// TestTheTimeLabelsAreSpreadAcrossTheAxis.
//
// Five labels, first and last at the ends. A tick set that clustered at
// one end would be a chart whose axis says less than the gridlines do.
func TestTheTimeLabelsAreSpreadAcrossTheAxis(t *testing.T) {
	c := Build(ChartInput{
		From:   chartFrom,
		To:     chartFrom.Add(hours(24)),
		Step:   time.Hour,
		Series: []ChartSeries{{Key: "goruntuleme", Points: []ChartPoint{{At: chartFrom, Value: 1}}}},
		Time:   func(t time.Time) string { return t.Format("15:04") },
	})
	if len(c.Ticks) != 5 {
		t.Fatalf("the axis carries %d labels, want 5", len(c.Ticks))
	}
	if c.Ticks[0].Label != "00:00" {
		t.Errorf("the first label is %q, want the start of the range", c.Ticks[0].Label)
	}
	for i := 1; i < len(c.Ticks); i++ {
		if c.Ticks[i].X <= c.Ticks[i-1].X {
			t.Errorf("label %d is at x=%.1f, not to the right of the one before it (%.1f)",
				i, c.Ticks[i].X, c.Ticks[i-1].X)
		}
	}
	if got, want := c.Ticks[len(c.Ticks)-1].X, c.PlotLeft+c.PlotWidth; math.Abs(got-want) > 0.05 {
		t.Errorf("the last label is at x=%.1f, want the right edge %.1f", got, want)
	}
}

// TestAskingForMoreLabelsThanBucketsDoesNotRepeatOne.
//
// A one-day range at a one-day interval has a single bucket, and five
// labels on one point is five copies of the same date.
func TestAskingForMoreLabelsThanBucketsDoesNotRepeatOne(t *testing.T) {
	c := Build(ChartInput{
		From:   chartFrom,
		To:     chartFrom.Add(hours(48)),
		Step:   24 * time.Hour,
		Series: []ChartSeries{{Key: "goruntuleme", Points: []ChartPoint{{At: chartFrom, Value: 1}}}},
		Ticks:  5,
		Time:   func(t time.Time) string { return t.Format("2006-01-02") },
	})
	if len(c.Ticks) != 2 {
		t.Fatalf("two buckets produced %d labels; a label per bucket is the ceiling",
			len(c.Ticks))
	}
	if c.Ticks[0].Label == c.Ticks[1].Label {
		t.Errorf("both labels read %q", c.Ticks[0].Label)
	}
}

// TestTheFutureIsNotDrawnAsAMeasuredZero.
//
// The panel's periods are whole local days, so at nine in the morning
// "the last seven days" still has fifteen hours of tonight in it. Those
// buckets have not happened; filling them with zeroes draws a line that
// plunges to the floor and stays there, and the picture reads as traffic
// collapsing this morning.
//
// Found by looking at the rendered page rather than by reasoning: the
// arithmetic was doing exactly what this file's own rule said, and the
// rule was true only of intervals that have already passed.
func TestTheFutureIsNotDrawnAsAMeasuredZero(t *testing.T) {
	c := Build(ChartInput{
		From:   chartFrom,
		To:     chartFrom.Add(hours(24)),
		Now:    chartFrom.Add(hours(6)),
		Step:   time.Hour,
		Series: []ChartSeries{{Key: "goruntuleme", Points: []ChartPoint{{At: at(5), Value: 40}}}},
	})

	pts := pointsOf(t, c.Lines[0].Path)
	if len(pts) != 6 {
		t.Fatalf("the chart drew %d points; six hours have elapsed and the other "+
			"eighteen have not happened yet", len(pts))
	}
	// The busy hour is the last one, so the line ends high rather than
	// on the floor.
	baseline := c.PlotTop + c.PlotHeight
	if math.Abs(pts[len(pts)-1][1]-baseline) < 1 {
		t.Error("the line ends on the baseline; the hours after now were drawn as " +
			"zeroes and the chart says traffic stopped")
	}
}

// TestTheBucketInProgressIsKept.
//
// The other direction, and the one an over-eager fix breaks: dropping
// every incomplete bucket throws away the hour somebody opened the panel
// to look at. Its dip is the ordinary artefact of an interval that is
// not over; the flat tail above is an assertion about the future.
func TestTheBucketInProgressIsKept(t *testing.T) {
	c := Build(ChartInput{
		From:   chartFrom,
		To:     chartFrom.Add(hours(24)),
		Now:    chartFrom.Add(hours(3) + 20*time.Minute),
		Step:   time.Hour,
		Series: []ChartSeries{{Key: "goruntuleme", Points: []ChartPoint{{At: at(3), Value: 5}}}},
	})
	if got := len(pointsOf(t, c.Lines[0].Path)); got != 4 {
		t.Errorf("the chart drew %d points twenty minutes into the fourth hour; the "+
			"hour in progress is measurement, not a guess", got)
	}
	if c.Lines[0].Total != 5 {
		t.Errorf("the series totals %d; the partial hour's rows were dropped", c.Lines[0].Total)
	}
}

// TestANowAfterTheRangeChangesNothing.
//
// Looking at last week does not truncate anything, and a chart that
// treated Now as a ceiling unconditionally would draw a historical
// period as though it were still running.
func TestANowAfterTheRangeChangesNothing(t *testing.T) {
	in := ChartInput{
		From:   chartFrom,
		To:     chartFrom.Add(hours(12)),
		Step:   time.Hour,
		Series: []ChartSeries{{Key: "goruntuleme", Points: []ChartPoint{{At: at(1), Value: 9}}}},
	}
	without := Build(in)
	in.Now = chartFrom.Add(hours(500))
	with := Build(in)
	if with.Lines[0].Path != without.Lines[0].Path {
		t.Error("a Now past the end of the range changed the drawing; a period that " +
			"is over is not still running")
	}
}

// TestTheEndLabelsArePinnedToTheEdgesTheySitOn.
//
// A centred label at the right edge hangs half its width outside the
// viewBox and is clipped. The first render of this chart ended its axis
// with "04.09.2" - a date that stops mid-year, with nothing in any log.
func TestTheEndLabelsArePinnedToTheEdgesTheySitOn(t *testing.T) {
	c := Build(ChartInput{
		From:   chartFrom,
		To:     chartFrom.Add(hours(24)),
		Step:   time.Hour,
		Series: []ChartSeries{{Key: "goruntuleme", Points: []ChartPoint{{At: chartFrom, Value: 3}}}},
		Time:   func(t time.Time) string { return t.Format("02.01.2006") },
	})
	if len(c.Ticks) < 3 {
		t.Fatalf("only %d labels", len(c.Ticks))
	}
	if got := c.Ticks[0].Anchor; got != "start" {
		t.Errorf("the first label is anchored %q; centred on the left edge it is "+
			"clipped in half", got)
	}
	if got := c.Ticks[len(c.Ticks)-1].Anchor; got != "end" {
		t.Errorf("the last label is anchored %q; centred on the right edge it is "+
			"clipped in half", got)
	}
	for _, tick := range c.Ticks[1 : len(c.Ticks)-1] {
		if tick.Anchor != "middle" {
			t.Errorf("an inner label is anchored %q rather than centred on its own "+
				"position", tick.Anchor)
		}
	}
}
