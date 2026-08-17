package ui

import (
	"strings"
	"testing"
	"time"
)

func testLang(t *testing.T, code string) *Language {
	t.Helper()
	cats, err := LoadCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	lang := cats.ByCode(code)
	if lang == nil {
		t.Fatalf("no language pack %q", code)
	}
	return lang
}

func istanbul(t *testing.T) *time.Location {
	t.Helper()
	// A fixed zone rather than LoadLocation: the container this runs in
	// may have no tzdata, and the point of the test is the arithmetic,
	// not the database. +03:00 is Istanbul all year - Turkey stopped
	// changing clocks in 2016.
	return time.FixedZone("+03", 3*60*60)
}

func TestNumbersReadAsTurkish(t *testing.T) {
	f := NewFormatter(testLang(t, "tr"), time.UTC)
	cases := []struct {
		got, want string
	}{
		{f.Number(0), "0"},
		{f.Number(999), "999"},
		{f.Number(1000), "1.000"},
		{f.Number(1234567), "1.234.567"},
		{f.Number(-4321), "-4.321"},
		{f.Decimal(1234.5, 1), "1.234,5"},
		{f.Decimal(2, 2), "2,00"},
		{f.Percent(0.457, 1), "%45,7"},
		{f.Percent(1, 0), "%100"},
		{f.Share(1, 4), "%25,0"},
		{f.Share(3, 0), "—"},
		{f.Bytes(0), "0 B"},
		{f.Bytes(1023), "1.023 B"},
		{f.Bytes(1536), "1,5 KB"},
		{f.Bytes(5 * 1024 * 1024 * 1024), "5,0 GB"},
		{f.Days(90), "90 gün"},
		{f.Days(1500), "1.500 gün"},
		// Turkish does not inflect after a numeral, so one and many are
		// the same word - and the pack never has to say so.
		{f.Days(1), "1 gün"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// TestPercentSignLeads is separate because it is the detail an
// English-speaking reviewer will "fix": Turkish writes %45, not 45%.
func TestPercentSignLeads(t *testing.T) {
	f := NewFormatter(testLang(t, "tr"), time.UTC)
	got := f.Percent(0.5, 0)
	if !strings.HasPrefix(got, "%") {
		t.Fatalf("Percent = %q; Turkish puts the sign first", got)
	}
}

func TestNonFiniteNumbersDoNotReachThePage(t *testing.T) {
	f := NewFormatter(testLang(t, "tr"), time.UTC)
	zero := 0.0
	if got := f.Decimal(1/zero, 2); got != "—" {
		t.Errorf("Decimal(+Inf) = %q", got)
	}
	if got := f.Percent(zero/zero, 1); got != "—" {
		t.Errorf("Percent(NaN) = %q", got)
	}
	if got := f.Bytes(-1); got != "—" {
		t.Errorf("Bytes(-1) = %q", got)
	}
}

func TestDatesRenderInTheSitesZone(t *testing.T) {
	loc := istanbul(t)
	f := NewFormatter(testLang(t, "tr"), loc)
	// 22:30 UTC is half past one the next morning in Istanbul. A panel
	// that got this wrong would report the busiest hour on the wrong
	// day.
	when := time.Date(2026, time.August, 16, 22, 30, 0, 0, time.UTC)
	if got, want := f.Date(when), "17 Ağustos 2026"; got != want {
		t.Errorf("Date = %q, want %q", got, want)
	}
	if got, want := f.Clock(when), "01:30"; got != want {
		t.Errorf("Clock = %q, want %q", got, want)
	}
	if got, want := f.DateTime(when), "17 Ağustos 2026, 01:30"; got != want {
		t.Errorf("DateTime = %q, want %q", got, want)
	}
	if got, want := f.ShortDate(when), "17.08.2026"; got != want {
		t.Errorf("ShortDate = %q, want %q", got, want)
	}
	if got, want := f.Weekday(when), "Pazartesi"; got != want {
		t.Errorf("Weekday = %q, want %q", got, want)
	}
	if got := f.ZoneName(); got != "+03" {
		t.Errorf("ZoneName = %q", got)
	}
}

func TestEveryMonthHasAName(t *testing.T) {
	f := NewFormatter(testLang(t, "tr"), time.UTC)
	for m := time.January; m <= time.December; m++ {
		when := time.Date(2026, m, 1, 12, 0, 0, 0, time.UTC)
		got := f.Date(when)
		if strings.Contains(got, "  ") || strings.HasSuffix(got, " 2026") == false {
			t.Errorf("month %d rendered as %q", m, got)
		}
		if strings.Contains(got, "1  2026") {
			t.Errorf("month %d has no name: %q", m, got)
		}
	}
}

func TestZeroTimeIsADashRatherThanTheYearOne(t *testing.T) {
	f := NewFormatter(testLang(t, "tr"), time.UTC)
	var zero time.Time
	for name, got := range map[string]string{
		"Date":      f.Date(zero),
		"DateTime":  f.DateTime(zero),
		"Clock":     f.Clock(zero),
		"ShortDate": f.ShortDate(zero),
		"Weekday":   f.Weekday(zero),
		"Since":     f.Since(zero),
	} {
		if got != "—" {
			t.Errorf("%s(zero) = %q; a never-happened time must not render as a date", name, got)
		}
	}
}

func TestSinceUsesTheWordsAPersonWouldUse(t *testing.T) {
	loc := istanbul(t)
	f := NewFormatter(testLang(t, "tr"), loc)
	now := time.Date(2026, time.August, 17, 9, 0, 0, 0, loc)
	cases := []struct {
		when time.Time
		want string
	}{
		{now.Add(-10 * time.Second), "az önce"},
		{now.Add(-59 * time.Second), "az önce"},
		{now.Add(-5 * time.Minute), "5 dakika önce"},
		{now.Add(-90 * time.Minute), "1 saat önce"},
		{now.Add(-5 * time.Hour), "5 saat önce"},
		// Yesterday at 23:50 is nine hours ago but it is still
		// yesterday, and that is what a reader means.
		{time.Date(2026, time.August, 16, 23, 50, 0, 0, loc), "dün 23:50"},
		{time.Date(2026, time.August, 10, 8, 0, 0, 0, loc), "10 Ağustos 2026, 08:00"},
	}
	for _, tc := range cases {
		if got := f.sinceAt(tc.when, now); got != tc.want {
			t.Errorf("sinceAt(%s) = %q, want %q", tc.when.Format(time.RFC3339), got, tc.want)
		}
	}
}

// TestSinceCrossingMidnight pins the boundary between the two rules,
// which is the only interesting part of Since.
//
// Under an hour, elapsed time wins even across midnight: at 00:30, an
// event at 23:50 is "40 dakika önce", and calling it "dün" would be
// true and useless. Past an hour the calendar wins: at 02:00 the same
// event is "dün 23:50" rather than "2 saat önce", because the reader
// would otherwise have to work out which day that lands on.
func TestSinceCrossingMidnight(t *testing.T) {
	loc := istanbul(t)
	f := NewFormatter(testLang(t, "tr"), loc)
	when := time.Date(2026, time.August, 16, 23, 50, 0, 0, loc)
	cases := []struct {
		now  time.Time
		want string
	}{
		{time.Date(2026, time.August, 17, 0, 30, 0, 0, loc), "40 dakika önce"},
		{time.Date(2026, time.August, 17, 2, 0, 0, 0, loc), "dün 23:50"},
		{time.Date(2026, time.August, 18, 2, 0, 0, 0, loc), "16 Ağustos 2026, 23:50"},
	}
	for _, tc := range cases {
		if got := f.sinceAt(when, tc.now); got != tc.want {
			t.Errorf("at %s: sinceAt = %q, want %q", tc.now.Format("02 15:04"), got, tc.want)
		}
	}
}

func TestFutureTimestampShowsTheClockRatherThanAPolitePhrase(t *testing.T) {
	f := NewFormatter(testLang(t, "tr"), time.UTC)
	now := time.Date(2026, time.August, 17, 9, 0, 0, 0, time.UTC)
	got := f.sinceAt(now.Add(time.Hour), now)
	if !strings.Contains(got, "2026") {
		t.Fatalf("a future timestamp rendered as %q; clock skew should be visible", got)
	}
}

func TestDurationStopsAtTwoUnits(t *testing.T) {
	f := NewFormatter(testLang(t, "tr"), time.UTC)
	cases := []struct {
		in   time.Duration
		want string
	}{
		{500 * time.Millisecond, "1 saniyeden az"},
		{45 * time.Second, "45 saniye"},
		{90 * time.Second, "1 dakika 30 saniye"},
		{3*time.Hour + 12*time.Minute + 5*time.Second, "3 saat 12 dakika"},
		{50 * time.Hour, "2 gün 2 saat"},
		{-time.Second, "—"},
	}
	for _, tc := range cases {
		if got := f.Duration(tc.in); got != tc.want {
			t.Errorf("Duration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestTurkishCasing is the one Go's standard library gets wrong: the
// capital of "i" is "İ" and the small letter of "I" is "ı". An email
// upper-cased the wrong way stops matching the address it came from.
func TestTurkishCasing(t *testing.T) {
	f := NewFormatter(testLang(t, "tr"), time.UTC)
	if got := f.Upper("iyi ışık"); got != "İYİ IŞIK" {
		t.Errorf("Upper = %q", got)
	}
	if got := f.Lower("İYİ IŞIK"); got != "iyi ışık" {
		t.Errorf("Lower = %q", got)
	}
	if got := f.Title("ısparta ilçesi"); got != "Isparta İlçesi" {
		t.Errorf("Title = %q", got)
	}
}

func TestNilZoneIsUTCRatherThanTheServersZone(t *testing.T) {
	f := NewFormatter(testLang(t, "tr"), nil)
	if f.Location() != time.UTC {
		t.Fatalf("nil zone became %v", f.Location())
	}
}
