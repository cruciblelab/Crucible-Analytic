package ui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Charts, drawn on the server, in the binary, as SVG.
//
// # Why the geometry is here and not in the browser
//
// The panel's rule is that everything the browser downloads is inside
// the binary: no CDN, no npm, no build step. A charting library would
// break that on the first line, and the alternative - writing one in
// the 51 KB of htmx we already ship - would put arithmetic in a place
// no test in this repository can reach.
//
// So the server does the arithmetic and sends the finished shape. The
// browser receives `<svg>` with a `<path>` in it, the same way it
// receives a `<table>` with `<tr>`s in it, and the numbers are as
// testable as any other page value. A chart that renders with
// JavaScript disabled is a side effect rather than the goal, but it is
// the right side effect.
//
// # The two things a time chart gets wrong
//
// **It plots by index instead of by time.** The analytics API returns
// only the buckets that have rows in them: a real seven-day hourly
// series over this project's own demo data came back with 144 buckets
// across a 169-hour span - twenty-five missing hours in eleven gaps,
// every one of them the small hours of a morning. Drawing those 144
// points evenly spaced produces a line that is smooth, plausible, and
// lying about when things happened. Every quiet night silently becomes
// no time at all.
//
// So Build fills the range itself, stepping by the interval from the
// start, and a bucket the API did not send becomes a zero. That is what
// it means: no rows in that hour is no pageviews in that hour, not an
// hour we know nothing about. The line drops to the floor at 3am
// because it should.
//
// **It scales to a maximum nobody can read.** An axis whose top is the
// peak itself gives gridlines like 369.75. Build picks the gridline
// step first - 1, 2 or 5 times a power of ten - and makes the top a
// whole number of them, so every label is an integer and every line is
// the same distance from the next. See niceAxis.
//
// # What this package does not do
//
// No tooltips, no zoom, no crosshair. Those need a pointer, and the
// numbers behind every point are already on the same page as text -
// which is also what makes the chart safe to hide from a screen reader
// rather than describing it point by point.

// ChartPoint is one measured bucket.
type ChartPoint struct {
	At    time.Time
	Value int
}

// ChartSeries is one line.
type ChartSeries struct {
	// Key names the series for CSS and for the legend. It is a stable
	// identifier, not a translated label.
	Key string
	// Label is what a reader sees in the legend, already translated.
	Label  string
	Points []ChartPoint
}

// ChartLine is one series after layout: the path to stroke, the path to
// fill beneath it, and the legend entry.
type ChartLine struct {
	Key   string
	Label string
	// Path is the "d" of the stroked line.
	Path string
	// Area is the "d" of the filled shape below it, closed along the
	// baseline. Empty when the series should not be filled.
	Area string
	// Total is the sum of the series over the range, so the legend can
	// say what the line is worth without a second query.
	Total int
}

// ChartGrid is one horizontal reference line.
type ChartGrid struct {
	// Y is where it sits in the SVG's coordinate space.
	Y float64
	// TextY is the baseline for its label, nudged so the text sits
	// centred on the line rather than resting on it.
	TextY float64
	// Value is the number it stands for, already formatted.
	Value string
}

// ChartTick is one label along the time axis.
type ChartTick struct {
	X float64
	// Label is the time, already formatted for the reader's locale.
	Label string
	// Anchor is the SVG text-anchor: the labels at the ends are pinned
	// to the edge they sit on rather than centred on it.
	//
	// A centred label at the right edge hangs half its width outside the
	// viewBox and is clipped - the first render of this chart ended its
	// axis with "04.09.2". Nothing errors, and the reader sees a date
	// that stops mid-year.
	Anchor string
}

// Chart is a finished drawing: everything the template needs and no
// decisions left in it.
type Chart struct {
	// Width and Height are the viewBox, in the unitless coordinates SVG
	// scales from. The stylesheet decides the rendered size.
	Width, Height float64
	// PlotLeft, PlotTop, PlotWidth and PlotHeight are the drawing area
	// inside the axis labels.
	PlotLeft, PlotTop     float64
	PlotWidth, PlotHeight float64
	// PlotRight, ValueLabelX and TickLabelY are the same geometry the
	// template would otherwise compute.
	//
	// Precomputed on purpose. A template that can add is a template that
	// holds layout decisions, and layout decisions in a template are the
	// ones no test in this repository can reach - which is the whole
	// reason this package exists rather than a script in the page.
	PlotRight   float64
	ValueLabelX float64
	TickLabelY  float64

	Lines []ChartLine
	Grid  []ChartGrid
	Ticks []ChartTick

	// Max is the top of the y-axis after rounding, kept so a test can
	// assert the rounding without parsing a path.
	Max int
	// Empty is true when every point in every series is zero, which is a
	// measurement rather than a missing chart. The template draws the
	// axes and says so.
	Empty bool
	// Label is the chart's accessible name.
	Label string
}

// chartGeometry is the fixed layout. Chosen once rather than passed in:
// two charts with different margins on one page look like a mistake,
// and the only caller that would want different numbers is a caller
// that should be drawing a different kind of chart.
const (
	chartWidth        = 720.0
	chartHeight       = 200.0
	chartPadLeft      = 44.0
	chartPadRight     = 8.0
	chartPadTop       = 10.0
	chartPadBottom    = 22.0
	chartMaxDivisions = 5
	chartMaxBuckets   = 2000
	// chartLabelGap is the space between an axis number and its line.
	chartLabelGap = 6.0
	// chartTextRise centres a label on its gridline. Half a cap height
	// at this font size; SVG text sits on its baseline, so a label given
	// the line's own y hangs above it.
	chartTextRise = 4.0
	// chartTickDrop lifts the time labels off the bottom edge.
	chartTickDrop = 6.0
)

// ChartInput is what Build needs to lay a time chart out.
type ChartInput struct {
	// From and To bound the x-axis. The range is half-open, matching the
	// rest of the panel: To is not drawn.
	From, To time.Time
	// Step is the bucket width the series were measured at. Build walks
	// From to To by this, so it must be the interval the API was asked
	// for - a Step that disagrees with the data produces a chart that is
	// wrong in a way that looks fine.
	Step time.Duration
	// Series are the lines, drawn in order. The first is the one the
	// area is filled under.
	Series []ChartSeries
	// Label is the accessible name.
	Label string
	// Number formats a y-axis value for this reader.
	Number func(int64) string
	// Time formats an x-axis tick for this reader.
	Time func(time.Time) string
	// Ticks is how many time labels to place. Zero means five.
	Ticks int
	// Now is where the measured range actually ends. Zero means To.
	//
	// # Why a chart needs to know the time
	//
	// The panel's periods are whole local days, so "the last seven days"
	// runs to midnight tonight - and most of the way through the day,
	// several buckets of that range have not happened yet. A bucket that
	// has not started is not a measured zero, and this file's own rule
	// ("no rows in that hour is no pageviews in that hour") is true only
	// of hours that have passed.
	//
	// Drawing them anyway is what the first render of this chart did: the
	// line plunged to the floor at the right-hand end and stayed there,
	// which reads as traffic collapsing this morning. The numbers on the
	// cards beside it said otherwise, and the picture is the half people
	// believe.
	//
	// The bucket in progress is kept. It is real measurement, it is what
	// somebody opening the panel came to see, and its dip is the ordinary
	// artefact of an interval that is not over - unlike the flat tail,
	// which is an assertion about the future.
	Now time.Time
}

// Build lays out a time chart.
//
// It never fails: a chart is decoration around numbers that are already
// on the page as text, so the worst outcome of nonsense input is an
// empty box, not a page that will not render.
func Build(in ChartInput) Chart {
	c := Chart{
		Width:      chartWidth,
		Height:     chartHeight,
		PlotLeft:   chartPadLeft,
		PlotTop:    chartPadTop,
		PlotWidth:  chartWidth - chartPadLeft - chartPadRight,
		PlotHeight: chartHeight - chartPadTop - chartPadBottom,
		Label:      in.Label,
		Empty:      true,
	}
	c.PlotRight = c.PlotLeft + c.PlotWidth
	c.ValueLabelX = c.PlotLeft - chartLabelGap
	c.TickLabelY = c.Height - chartTickDrop
	if !in.To.After(in.From) || in.Step <= 0 {
		return c
	}
	span := in.To.Sub(in.From)
	if int(span/in.Step) > chartMaxBuckets {
		// A step so small it would put thousands of points in a 720-unit
		// box. Nothing calls it that way today; the guard is here so a
		// future caller gets an empty chart rather than a megabyte of
		// path data.
		return c
	}

	// ---- 1. the buckets the range has, not the ones the API sent ----
	end := in.To
	if !in.Now.IsZero() && in.Now.Before(end) {
		end = in.Now
	}
	slots := bucketStarts(in.From, end, in.Step)
	if len(slots) == 0 {
		return c
	}

	filled := make([][]int, len(in.Series))
	peak := 0
	for i, s := range in.Series {
		values := fillSeries(slots, in.Step, s.Points)
		filled[i] = values
		for _, v := range values {
			if v > peak {
				peak = v
			}
		}
	}
	if peak > 0 {
		c.Empty = false
	}

	// ---- 2. a top somebody can read ----
	var gridStep int
	c.Max, gridStep = niceAxis(peak)
	top := float64(c.Max)

	// ---- 3. the lines ----
	x := func(i int) float64 {
		if len(slots) == 1 {
			return c.PlotLeft
		}
		return c.PlotLeft + c.PlotWidth*float64(i)/float64(len(slots)-1)
	}
	y := func(v int) float64 {
		return c.PlotTop + c.PlotHeight*(1-float64(v)/top)
	}

	for i, s := range in.Series {
		values := filled[i]
		line := ChartLine{Key: s.Key, Label: s.Label}
		var path strings.Builder
		for j, v := range values {
			if j == 0 {
				path.WriteString("M")
			} else {
				path.WriteString(" L")
			}
			fmt.Fprintf(&path, "%s %s", coord(x(j)), coord(y(v)))
			line.Total += v
		}
		line.Path = path.String()
		// Only the first series is filled. Two overlapping translucent
		// areas make the region where they cross a third colour that
		// stands for nothing.
		if i == 0 && len(values) > 0 {
			base := coord(c.PlotTop + c.PlotHeight)
			line.Area = line.Path +
				" L" + coord(x(len(values)-1)) + " " + base +
				" L" + coord(x(0)) + " " + base + " Z"
		}
		c.Lines = append(c.Lines, line)
	}

	// ---- 4. the gridlines, labelled ----
	number := in.Number
	if number == nil {
		number = func(v int64) string { return strconv.FormatInt(v, 10) }
	}
	for v := 0; v <= c.Max; v += gridStep {
		gy := y(v)
		c.Grid = append(c.Grid, ChartGrid{
			Y: gy, TextY: gy + chartTextRise, Value: number(int64(v)),
		})
	}

	// ---- 5. the time labels ----
	ticks := in.Ticks
	if ticks <= 0 {
		ticks = 5
	}
	if ticks > len(slots) {
		ticks = len(slots)
	}
	label := in.Time
	if label == nil {
		label = func(t time.Time) string { return t.Format("2006-01-02") }
	}
	for k := 0; k < ticks; k++ {
		i := 0
		if ticks > 1 {
			i = (len(slots) - 1) * k / (ticks - 1)
		}
		anchor := "middle"
		switch {
		case k == 0:
			anchor = "start"
		case k == ticks-1:
			anchor = "end"
		}
		c.Ticks = append(c.Ticks, ChartTick{X: x(i), Label: label(slots[i]), Anchor: anchor})
	}
	return c
}

// bucketStarts is every bucket the range contains, walked from From.
//
// From rather than from the first point the API returned: a series that
// begins at noon because nothing happened that morning would otherwise
// start the chart at noon, and the morning would disappear instead of
// showing as the quiet it was.
func bucketStarts(from, to time.Time, step time.Duration) []time.Time {
	var out []time.Time
	for t := from; t.Before(to); t = t.Add(step) {
		out = append(out, t)
		if len(out) > chartMaxBuckets {
			return nil
		}
	}
	return out
}

// fillSeries places each point in its bucket and leaves the rest at
// zero.
//
// Zero, not absent. The API omits a bucket with no rows in it, and "no
// rows in that hour" is a measurement: nobody visited. Treating it as
// unknown and drawing straight through it is how a chart reports a
// quiet night as steady traffic.
//
// A point outside the range is dropped rather than clamped to an edge -
// clamping would pile a week of traffic onto the first bucket and draw
// a spike that never happened.
func fillSeries(slots []time.Time, step time.Duration, points []ChartPoint) []int {
	out := make([]int, len(slots))
	if len(slots) == 0 {
		return out
	}
	start := slots[0]
	for _, p := range points {
		if p.At.Before(start) {
			continue
		}
		i := int(p.At.Sub(start) / step)
		if i < 0 || i >= len(out) {
			continue
		}
		// Added rather than assigned: two points inside one bucket is a
		// caller passing a finer interval than it said, and the sum is
		// the answer that keeps the column total right.
		out[i] += p.Value
	}
	return out
}

// niceAxis picks the gridline step first and the top second.
//
// # Why not "round the top up to a nice number"
//
// That is the obvious rule and it produces unreadable axes. Round 1 479
// up to 2 000, divide by four, and the gridlines are 500 / 1 000 /
// 1 500: fine. Round 5 up to 5, divide by four, and they are 1.25 /
// 2.5 / 3.75 - and if the code divides with integers instead, they are
// 1 / 2 / 3 / 5, which are *unevenly spaced lines wearing evenly
// spaced labels*. That is worse than a fraction, because it looks
// right.
//
// So the step is chosen from 1, 2 or 5 times a power of ten, and the
// top is a whole number of steps. Every label is then an integer, every
// line is the same distance from the next, and the top sits just above
// the data rather than at the next power of ten.
//
// A peak of zero returns a top of 1, so nothing below divides by zero:
// an axis top of 0 makes every y a NaN, and a path full of NaN renders
// as nothing at all with no error anywhere.
func niceAxis(peak int) (top, step int) {
	if peak <= 0 {
		return 1, 1
	}
	magnitude := 1.0
	for {
		for _, mult := range []float64{1, 2, 5} {
			s := mult * magnitude
			divisions := int(math.Ceil(float64(peak) / s))
			if divisions >= 1 && divisions <= chartMaxDivisions {
				return divisions * int(s), int(s)
			}
		}
		magnitude *= 10
		if magnitude > 1e15 {
			// Unreachable with any real count; the bound is here so a
			// nonsense peak ends in a chart rather than in a loop.
			return peak, peak
		}
	}
}

// coord trims a coordinate to one decimal place.
//
// SVG's own precision, not Go's: %v on a float64 writes seventeen
// significant digits, and a 720-point path written that way is thirty
// kilobytes of a page whose entire stylesheet is fourteen. One decimal
// on a 720-unit box is a tenth of a pixel at native size.
func coord(v float64) string {
	return strconv.FormatFloat(v, 'f', 1, 64)
}
