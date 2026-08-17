package ui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"golang.org/x/text/number"
)

// formatPack is the locale's date and unit data, read from the language
// file's [bicim] section.
//
// These are not messages and they are not code. Month names are an
// ordered list indexed by time.Month - useless to a translator as twelve
// separate keys and dangerous as twelve separate keys, since one edited
// out of order produces a date that is wrong rather than untranslated.
// Date ordering is data for the same reason: Turkish writes
// "17 Ağustos 2026" and English "August 17, 2026", and a formatter that
// hard-codes either is a formatter that cannot be translated at all.
//
// golang.org/x/text does not export CLDR date patterns, which is why
// this is declared rather than looked up.
type formatPack struct {
	Months   []string `toml:"aylar"`
	Weekdays []string `toml:"gunler"`

	Date      string `toml:"tarih"`
	DateTime  string `toml:"tarih_saat"`
	ShortDate string `toml:"kisa_tarih"`
	Clock     string `toml:"saat"`

	JustNow    string `toml:"az_once"`
	MinutesAgo string `toml:"dakika_once"`
	HoursAgo   string `toml:"saat_once"`
	Yesterday  string `toml:"dun"`
	LessThanA  string `toml:"saniyeden_az"`
	Missing    string `toml:"yok"`

	Bytes []string `toml:"bayt"`

	Units struct {
		Day    pluralForms `toml:"gun"`
		Hour   pluralForms `toml:"saat"`
		Minute pluralForms `toml:"dakika"`
		Second pluralForms `toml:"saniye"`
	} `toml:"birim"`
}

// validate refuses a pack that would render nonsense.
//
// Every check here stands for a specific way a translation goes wrong
// without anybody noticing: eleven months, a date pattern missing the
// year, a relative phrase with no place to put the number.
func (f formatPack) validate(file string) error {
	if len(f.Months) != 12 {
		return fmt.Errorf("ui: %s: [bicim] aylar has %d entries, want 12", file, len(f.Months))
	}
	if len(f.Weekdays) != 7 {
		return fmt.Errorf("ui: %s: [bicim] gunler has %d entries, want 7 (starting Monday)", file, len(f.Weekdays))
	}
	if len(f.Bytes) < 4 {
		return fmt.Errorf("ui: %s: [bicim] bayt has %d entries, want at least 4", file, len(f.Bytes))
	}
	for i, name := range append(append([]string{}, f.Months...), f.Weekdays...) {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("ui: %s: [bicim] name %d is empty", file, i)
		}
	}
	for _, req := range []struct{ field, value, needs string }{
		{"tarih", f.Date, "{gun}"},
		{"tarih", f.Date, "{ay}"},
		{"tarih", f.Date, "{yil}"},
		{"tarih_saat", f.DateTime, "{tarih}"},
		{"tarih_saat", f.DateTime, "{saat}"},
		{"dun", f.Yesterday, "{saat}"},
	} {
		if !strings.Contains(req.value, req.needs) {
			return fmt.Errorf("ui: %s: [bicim] %s = %q is missing %s", file, req.field, req.value, req.needs)
		}
	}
	for _, req := range []struct{ field, value string }{
		{"dakika_once", f.MinutesAgo},
		{"saat_once", f.HoursAgo},
	} {
		if strings.Count(req.value, "%d") != 1 {
			return fmt.Errorf("ui: %s: [bicim] %s = %q needs exactly one %%d", file, req.field, req.value)
		}
	}
	for _, req := range []struct{ field, value string }{
		{"az_once", f.JustNow},
		{"saniyeden_az", f.LessThanA},
		{"yok", f.Missing},
	} {
		if strings.TrimSpace(req.value) == "" {
			return fmt.Errorf("ui: %s: [bicim] %s is empty", file, req.field)
		}
	}
	for name, unit := range map[string]pluralForms{
		"gun": f.Units.Day, "saat": f.Units.Hour,
		"dakika": f.Units.Minute, "saniye": f.Units.Second,
	} {
		if unit.empty() {
			return fmt.Errorf("ui: %s: [bicim.birim.%s] has no \"other\" form", file, name)
		}
		if !strings.Contains(unit.Other, "%s") {
			return fmt.Errorf("ui: %s: [bicim.birim.%s] other = %q has nowhere to put the number (%%s)", file, name, unit.Other)
		}
	}
	if err := checkLayout(file, "kisa_tarih", f.ShortDate); err != nil {
		return err
	}
	return checkLayout(file, "saat", f.Clock)
}

// checkLayout proves a Go time layout is a layout rather than literal
// text.
//
// "gg.aa.yyyy" looks like a date pattern and is not one: Go prints it
// back verbatim, so every timestamp in the panel would read the same and
// nothing would error. Formatting two genuinely different instants and
// requiring different output is the cheapest way to tell the two apart.
func checkLayout(file, field, layout string) error {
	if strings.TrimSpace(layout) == "" {
		return fmt.Errorf("ui: %s: [bicim] %s is empty", file, field)
	}
	a := time.Date(2026, time.March, 4, 5, 6, 0, 0, time.UTC)
	b := time.Date(2027, time.November, 12, 13, 14, 0, 0, time.UTC)
	if a.Format(layout) == b.Format(layout) {
		return fmt.Errorf("ui: %s: [bicim] %s = %q is not a Go time layout - it prints the same for every instant "+
			"(Go layouts are written with the reference time: 2006-01-02 15:04:05)", file, field, layout)
	}
	return nil
}

// Formatter turns values into the strings a reader of one language, in
// one time zone, expects.
//
// Both are fields rather than parameters because getting either wrong is
// not a formatting bug, it is a wrong answer: a panel that renders
// timestamps in UTC tells a customer in Istanbul that their evening
// traffic peak happened in the afternoon, and one that renders
// "1.234,5" to an English reader has said twelve thousand.
//
// A Formatter is immutable and safe for concurrent use.
type Formatter struct {
	lang    *Language
	loc     *time.Location
	printer *message.Printer
	upper   cases.Caser
	lower   cases.Caser
	title   cases.Caser
	now     func() time.Time
}

// NewFormatter returns a formatter for one language and zone.
//
// A nil location means UTC, which is the honest fallback: it is visibly
// not a local time rather than the server's zone masquerading as the
// customer's.
func NewFormatter(lang *Language, loc *time.Location) *Formatter {
	if loc == nil {
		loc = time.UTC
	}
	tag := language.Und
	if lang != nil {
		tag = lang.Tag
	}
	return &Formatter{
		lang:    lang,
		loc:     loc,
		printer: message.NewPrinter(tag),
		upper:   cases.Upper(tag),
		lower:   cases.Lower(tag),
		title:   cases.Title(tag),
		now:     time.Now,
	}
}

func (f *Formatter) pack() formatPack {
	if f.lang == nil {
		return formatPack{}
	}
	return f.lang.format
}

func (f *Formatter) tag() language.Tag {
	if f.lang == nil {
		return language.Und
	}
	return f.lang.Tag
}

// Language is the pack this formatter renders for.
func (f *Formatter) Language() *Language { return f.lang }

// Location reports the zone this formatter renders in.
func (f *Formatter) Location() *time.Location { return f.loc }

// Zone is the zone's short name at the given instant ("+03"), for pages
// showing a time somebody may need to compare against a clock
// elsewhere.
func (f *Formatter) Zone(t time.Time) string {
	name, _ := t.In(f.loc).Zone()
	return name
}

// ZoneName is the zone's IANA name ("Europe/Istanbul").
func (f *Formatter) ZoneName() string { return f.loc.String() }

// Dash is the placeholder for a value that does not exist. It is not
// "0" and not "": zero is a measurement, empty is a rendering bug, and
// this is neither.
func (f *Formatter) Dash() string {
	if m := f.pack().Missing; m != "" {
		return m
	}
	return "—"
}

// Number formats a whole number with this language's group separators:
// 1234567 is "1.234.567" in Turkish and "1,234,567" in English.
func (f *Formatter) Number(v int64) string {
	return f.printer.Sprint(number.Decimal(v))
}

// Decimal formats with exactly digits fractional places.
func (f *Formatter) Decimal(v float64, digits int) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return f.Dash()
	}
	return f.printer.Sprint(number.Decimal(v,
		number.MaxFractionDigits(digits),
		number.MinFractionDigits(digits)))
}

// Percent formats a ratio in 0..1 the way this language writes it -
// with the sign leading in Turkish ("%45,7") and trailing in English
// ("45.7%"). That difference is x/text's, not ours, which is the reason
// to route through it rather than appending a character.
func (f *Formatter) Percent(ratio float64, digits int) string {
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return f.Dash()
	}
	return f.printer.Sprint(number.Percent(ratio,
		number.MaxFractionDigits(digits),
		number.MinFractionDigits(digits)))
}

// Share is Percent for a part of a whole, guarding the division. A zero
// total yields the dash rather than NaN, because "no requests yet" and
// "0% of requests" are different facts and the panel never draws the
// first as the second.
func (f *Formatter) Share(part, total int64) string {
	if total == 0 {
		return f.Dash()
	}
	return f.Percent(float64(part)/float64(total), 1)
}

// Bytes formats a size in binary multiples: 1536 is "1,5 KB".
//
// Binary multiples with decimal labels is the convention every disk and
// log tool the operator already uses prints, so the panel's number and
// du's number agree.
func (f *Formatter) Bytes(n int64) string {
	units := f.pack().Bytes
	if n < 0 || len(units) == 0 {
		return f.Dash()
	}
	size := float64(n)
	unit := 0
	for size >= 1024 && unit < len(units)-1 {
		size /= 1024
		unit++
	}
	if unit == 0 {
		return f.printer.Sprint(number.Decimal(n)) + " " + units[unit]
	}
	return f.Decimal(size, 1) + " " + units[unit]
}

// Date renders the language's date pattern: "17 Ağustos 2026",
// "August 17, 2026".
func (f *Formatter) Date(t time.Time) string {
	if t.IsZero() {
		return f.Dash()
	}
	p := f.pack()
	if len(p.Months) != 12 || p.Date == "" {
		return f.Dash()
	}
	t = t.In(f.loc)
	return strings.NewReplacer(
		"{gun}", f.printer.Sprint(t.Day()),
		"{ay}", p.Months[t.Month()-1],
		"{yil}", fmt.Sprint(t.Year()),
	).Replace(p.Date)
}

// ShortDate renders the compact form, for tables where the long one
// would not fit.
func (f *Formatter) ShortDate(t time.Time) string {
	if t.IsZero() || f.pack().ShortDate == "" {
		return f.Dash()
	}
	return t.In(f.loc).Format(f.pack().ShortDate)
}

// Clock renders the time of day. 24-hour in both packs shipped today: a
// panel about traffic peaks is read by the hour, and the layout is data
// so a language that wants 3:04 PM can say so.
func (f *Formatter) Clock(t time.Time) string {
	if t.IsZero() || f.pack().Clock == "" {
		return f.Dash()
	}
	return t.In(f.loc).Format(f.pack().Clock)
}

// DateTime renders the date and the time together.
func (f *Formatter) DateTime(t time.Time) string {
	if t.IsZero() {
		return f.Dash()
	}
	p := f.pack()
	if p.DateTime == "" {
		return f.Date(t)
	}
	return strings.NewReplacer(
		"{tarih}", f.Date(t),
		"{saat}", f.Clock(t),
	).Replace(p.DateTime)
}

// Weekday renders the day's name.
func (f *Formatter) Weekday(t time.Time) string {
	p := f.pack()
	if t.IsZero() || len(p.Weekdays) != 7 {
		return f.Dash()
	}
	// Go counts from Sunday, the packs list from Monday, because that is
	// where the week starts for the languages shipped here.
	index := (int(t.In(f.loc).Weekday()) + 6) % 7
	return p.Weekdays[index]
}

// Since renders how long ago t was, in the words a person would use.
//
// The boundaries are deliberately coarse. "3 saat önce" is what the
// reader wants from a log line; "3 saat 41 dakika 12 saniye önce" is
// what a machine wants, and putting it on the page only makes the number
// harder to read out loud.
func (f *Formatter) Since(t time.Time) string {
	return f.sinceAt(t, f.now())
}

func (f *Formatter) sinceAt(t, now time.Time) string {
	if t.IsZero() {
		return f.Dash()
	}
	p := f.pack()
	d := now.Sub(t)
	switch {
	case d < 0:
		// A timestamp from the future is a clock-skew symptom, not
		// something to phrase politely. Show the absolute time.
		return f.DateTime(t)
	case d < time.Minute:
		return p.JustNow
	case d < time.Hour:
		// Under an hour stays relative whatever the calendar says. At
		// 00:30, "40 dakika önce" is what somebody wants to hear about
		// an event at 23:50; "dün" would be true and useless.
		return fmt.Sprintf(p.MinutesAgo, int(d.Minutes()))
	}
	// Past an hour the calendar takes over from elapsed time. Nine hours
	// ago, read at nine in the morning, makes the reader do the
	// subtraction to find out it was last night; "dün 23:50" tells them
	// directly.
	switch daysBetween(t.In(f.loc), now.In(f.loc)) {
	case 0:
		return fmt.Sprintf(p.HoursAgo, int(d.Hours()))
	case 1:
		return strings.ReplaceAll(p.Yesterday, "{saat}", f.Clock(t))
	}
	return f.DateTime(t)
}

func daysBetween(earlier, later time.Time) int {
	e := time.Date(earlier.Year(), earlier.Month(), earlier.Day(), 0, 0, 0, 0, earlier.Location())
	l := time.Date(later.Year(), later.Month(), later.Day(), 0, 0, 0, 0, later.Location())
	return int(math.Round(l.Sub(e).Hours() / 24))
}

// unit renders a count with its unit, in the plural form the language's
// CLDR rules choose: "1 gün", "1 day", "2 days", "5 дней".
func (f *Formatter) unit(forms pluralForms, n int) string {
	if forms.empty() {
		return f.Number(int64(n))
	}
	return fmt.Sprintf(forms.pick(f.tag(), n), f.Number(int64(n)))
}

// Duration renders a span as at most two units: "3 saat 12 dakika".
func (f *Formatter) Duration(d time.Duration) string {
	if d < 0 {
		return f.Dash()
	}
	p := f.pack()
	if d < time.Second {
		return p.LessThanA
	}
	var parts []string
	if days := int(d.Hours()) / 24; days > 0 {
		parts = append(parts, f.unit(p.Units.Day, days))
		d -= time.Duration(days) * 24 * time.Hour
	}
	if hours := int(d.Hours()); hours > 0 {
		parts = append(parts, f.unit(p.Units.Hour, hours))
		d -= time.Duration(hours) * time.Hour
	}
	if minutes := int(d.Minutes()); minutes > 0 && len(parts) < 2 {
		parts = append(parts, f.unit(p.Units.Minute, minutes))
		d -= time.Duration(minutes) * time.Minute
	}
	if seconds := int(d.Seconds()); seconds > 0 && len(parts) < 2 {
		parts = append(parts, f.unit(p.Units.Second, seconds))
	}
	if len(parts) > 2 {
		parts = parts[:2]
	}
	return strings.Join(parts, " ")
}

// Days renders a retention or age: "90 gün", "90 days", "1 day".
func (f *Formatter) Days(n int) string {
	return f.unit(f.pack().Units.Day, n)
}

// Upper, Lower and Title case for this language, where Go's standard
// casing is wrong in both directions for Turkish: the capital of "i" is
// "İ" and the small letter of "I" is "ı".
func (f *Formatter) Upper(s string) string { return f.upper.String(s) }
func (f *Formatter) Lower(s string) string { return f.lower.String(s) }
func (f *Formatter) Title(s string) string { return f.title.String(s) }
