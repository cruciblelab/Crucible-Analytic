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

// Turkish month and day names.
//
// These are interface text and by the rule in messages.tr.toml they
// would belong in the catalog. They are here instead because they are
// not a message, they are part of the locale's date algorithm: an
// ordered list of twelve indexed by time.Month, useless to a translator
// as twelve separate keys and dangerous as twelve separate keys, since
// one edited out of order would produce a date that is wrong rather
// than untranslated.
//
// golang.org/x/text does not export CLDR date patterns, which is why
// this is written out rather than looked up.
var (
	aylar = [...]string{
		"", "Ocak", "Şubat", "Mart", "Nisan", "Mayıs", "Haziran",
		"Temmuz", "Ağustos", "Eylül", "Ekim", "Kasım", "Aralık",
	}
	gunler = map[time.Weekday]string{
		time.Monday:    "Pazartesi",
		time.Tuesday:   "Salı",
		time.Wednesday: "Çarşamba",
		time.Thursday:  "Perşembe",
		time.Friday:    "Cuma",
		time.Saturday:  "Cumartesi",
		time.Sunday:    "Pazar",
	}
)

// turkish is the language tag every formatter and caser here uses. It
// is what makes "45,7" a decimal and "İ" the capital of "i" - both of
// which Go's default behaviour gets wrong for this language.
var turkish = language.Turkish

// Formatter turns values into the strings a Turkish reader expects, in
// one specific time zone.
//
// The zone is a field rather than a parameter because getting it wrong
// is not a formatting bug, it is a wrong answer: a panel that renders
// timestamps in UTC tells a customer in Istanbul that their evening
// traffic peak happened in the afternoon. Every page that shows a time
// gets a formatter built from that site's configured zone.
//
// A Formatter is immutable and safe for concurrent use.
type Formatter struct {
	loc     *time.Location
	printer *message.Printer
	upper   cases.Caser
	lower   cases.Caser
	title   cases.Caser
	now     func() time.Time
}

// NewFormatter returns a formatter for loc. A nil location means UTC,
// which is the honest fallback: it is visibly not a local time rather
// than the server's zone masquerading as the customer's.
func NewFormatter(loc *time.Location) *Formatter {
	if loc == nil {
		loc = time.UTC
	}
	return &Formatter{
		loc:     loc,
		printer: message.NewPrinter(turkish),
		upper:   cases.Upper(turkish),
		lower:   cases.Lower(turkish),
		title:   cases.Title(turkish),
		now:     time.Now,
	}
}

// Location reports the zone this formatter renders in.
func (f *Formatter) Location() *time.Location { return f.loc }

// Zone is the zone's short name at the given instant ("+03"), for
// pages that show a time somebody may need to compare against a clock
// elsewhere.
func (f *Formatter) Zone(t time.Time) string {
	name, _ := t.In(f.loc).Zone()
	return name
}

// ZoneName is the zone's IANA name ("Europe/Istanbul").
func (f *Formatter) ZoneName() string { return f.loc.String() }

// Number formats a whole number with Turkish group separators:
// 1234567 becomes "1.234.567".
func (f *Formatter) Number(v int64) string {
	return f.printer.Sprint(number.Decimal(v))
}

// Decimal formats with at most digits fractional places, using a comma:
// 1234.5 with 1 digit becomes "1.234,5".
func (f *Formatter) Decimal(v float64, digits int) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return catalogDash
	}
	return f.printer.Sprint(number.Decimal(v,
		number.MaxFractionDigits(digits),
		number.MinFractionDigits(digits)))
}

// Percent formats a ratio in 0..1 as Turkish writes it, with the sign
// leading: 0.457 becomes "%45,7".
func (f *Formatter) Percent(ratio float64, digits int) string {
	if math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		return catalogDash
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
		return catalogDash
	}
	return f.Percent(float64(part)/float64(total), 1)
}

// catalogDash is what stands in for a value that does not exist. It is
// not "0" and not "": zero is a measurement, empty is a rendering bug,
// and a dash is neither.
const catalogDash = "—"

// Dash is the placeholder for a value that does not exist, exposed for
// templates.
func (f *Formatter) Dash() string { return catalogDash }

var byteUnits = [...]string{"B", "KB", "MB", "GB", "TB", "PB"}

// Bytes formats a size in binary multiples: 1536 becomes "1,5 KB".
//
// Binary multiples with decimal labels is the convention every disk and
// log tool the operator already uses prints, so matching it means the
// panel's number and `du`'s number agree.
func (f *Formatter) Bytes(n int64) string {
	if n < 0 {
		return catalogDash
	}
	size := float64(n)
	unit := 0
	for size >= 1024 && unit < len(byteUnits)-1 {
		size /= 1024
		unit++
	}
	if unit == 0 {
		return f.printer.Sprint(number.Decimal(n)) + " " + byteUnits[unit]
	}
	return f.Decimal(size, 1) + " " + byteUnits[unit]
}

// Date renders "17 Ağustos 2026".
func (f *Formatter) Date(t time.Time) string {
	if t.IsZero() {
		return catalogDash
	}
	t = t.In(f.loc)
	return fmt.Sprintf("%d %s %d", t.Day(), aylar[t.Month()], t.Year())
}

// ShortDate renders "17.08.2026", for tables where the long form would
// not fit.
func (f *Formatter) ShortDate(t time.Time) string {
	if t.IsZero() {
		return catalogDash
	}
	return t.In(f.loc).Format("02.01.2006")
}

// Clock renders "14:05". Always 24-hour: Turkish does not use AM/PM,
// and a panel about traffic peaks is read by the hour.
func (f *Formatter) Clock(t time.Time) string {
	if t.IsZero() {
		return catalogDash
	}
	return t.In(f.loc).Format("15:04")
}

// DateTime renders "17 Ağustos 2026, 14:05".
func (f *Formatter) DateTime(t time.Time) string {
	if t.IsZero() {
		return catalogDash
	}
	return f.Date(t) + ", " + f.Clock(t)
}

// Weekday renders "Pazartesi".
func (f *Formatter) Weekday(t time.Time) string {
	if t.IsZero() {
		return catalogDash
	}
	return gunler[t.In(f.loc).Weekday()]
}

// Since renders how long ago t was, in the words a person would use.
//
// The boundaries are deliberately coarse. "3 saat önce" is what the
// reader wants from a log line; "3 saat 41 dakika 12 saniye önce" is
// what a machine wants, and putting it on the page only makes the
// number harder to read out loud.
func (f *Formatter) Since(t time.Time) string {
	return f.sinceAt(t, f.now())
}

func (f *Formatter) sinceAt(t, now time.Time) string {
	if t.IsZero() {
		return catalogDash
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		// A timestamp from the future is a clock-skew symptom, not
		// something to phrase politely. Show the absolute time.
		return f.DateTime(t)
	case d < time.Minute:
		return "az önce"
	case d < time.Hour:
		// Under an hour stays relative whatever the calendar says.
		// At 00:30, "40 dakika önce" is what somebody wants to hear
		// about an event at 23:50; "dün" would be technically true and
		// useless.
		return fmt.Sprintf("%d dakika önce", int(d.Minutes()))
	}
	// Past an hour, the calendar takes over from elapsed time. Nine
	// hours ago, read at nine in the morning, makes the reader do the
	// subtraction to find out it was last night; "dün 23:50" tells them
	// directly.
	days := daysBetween(t.In(f.loc), now.In(f.loc))
	switch days {
	case 0:
		return fmt.Sprintf("%d saat önce", int(d.Hours()))
	case 1:
		return "dün " + f.Clock(t)
	}
	return f.DateTime(t)
}

func daysBetween(earlier, later time.Time) int {
	e := time.Date(earlier.Year(), earlier.Month(), earlier.Day(), 0, 0, 0, 0, earlier.Location())
	l := time.Date(later.Year(), later.Month(), later.Day(), 0, 0, 0, 0, later.Location())
	return int(math.Round(l.Sub(e).Hours() / 24))
}

// Duration renders a span as at most two units: "3 saat 12 dakika".
func (f *Formatter) Duration(d time.Duration) string {
	if d < 0 {
		return catalogDash
	}
	if d < time.Second {
		return "1 saniyeden az"
	}
	var parts []string
	if days := int(d.Hours()) / 24; days > 0 {
		parts = append(parts, fmt.Sprintf("%d gün", days))
		d -= time.Duration(days) * 24 * time.Hour
	}
	if hours := int(d.Hours()); hours > 0 {
		parts = append(parts, fmt.Sprintf("%d saat", hours))
		d -= time.Duration(hours) * time.Hour
	}
	if minutes := int(d.Minutes()); minutes > 0 && len(parts) < 2 {
		parts = append(parts, fmt.Sprintf("%d dakika", minutes))
		d -= time.Duration(minutes) * time.Minute
	}
	if seconds := int(d.Seconds()); seconds > 0 && len(parts) < 2 {
		parts = append(parts, fmt.Sprintf("%d saniye", seconds))
	}
	if len(parts) > 2 {
		parts = parts[:2]
	}
	return strings.Join(parts, " ")
}

// Days renders a retention or age in days: "90 gün".
func (f *Formatter) Days(n int) string {
	return f.printer.Sprint(number.Decimal(n)) + " gün"
}

// Upper, Lower and Title case correctly for Turkish, where Go's
// standard casing is wrong in both directions: the capital of "i" is
// "İ" and the small letter of "I" is "ı".
func (f *Formatter) Upper(s string) string { return f.upper.String(s) }
func (f *Formatter) Lower(s string) string { return f.lower.String(s) }
func (f *Formatter) Title(s string) string { return f.title.String(s) }
