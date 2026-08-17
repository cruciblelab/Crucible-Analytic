package ui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// russianPack is a language this repository does not ship.
//
// It exists to demonstrate the claim in the package comment - that
// adding a language is a file and a rebuild, with no Go to change -
// rather than to assert it. It is also the only language here with more
// than two plural categories, which is the part of the design that a
// Turkish-plus-English pair cannot exercise at all: Turkish never
// inflects, English has one and other, and a mechanism tested only
// against those two would look finished while being wrong for most of
// Europe.
const russianPack = `
[dil]
kod = "ru"
ad = "Русский"
yon = "ltr"

[bicim]
aylar = [
  "января", "февраля", "марта", "апреля", "мая", "июня",
  "июля", "августа", "сентября", "октября", "ноября", "декабря",
]
gunler = ["понедельник", "вторник", "среда", "четверг", "пятница", "суббота", "воскресенье"]
tarih = "{gun} {ay} {yil}"
tarih_saat = "{tarih}, {saat}"
kisa_tarih = "02.01.2006"
saat = "15:04"
az_once = "только что"
dakika_once = "%d минут назад"
saat_once = "%d часов назад"
dun = "вчера в {saat}"
saniyeden_az = "меньше секунды"
yok = "—"
bayt = ["Б", "КБ", "МБ", "ГБ", "ТБ", "ПБ"]

[bicim.birim.gun]
one = "%s день"
few = "%s дня"
many = "%s дней"
other = "%s дня"

[bicim.birim.saat]
one = "%s час"
few = "%s часа"
many = "%s часов"
other = "%s часа"

[bicim.birim.dakika]
one = "%s минута"
few = "%s минуты"
many = "%s минут"
other = "%s минуты"

[bicim.birim.saniye]
one = "%s секунда"
few = "%s секунды"
many = "%s секунд"
other = "%s секунды"

[metin.uygulama]
ad = "Crucible Analytic"

[metin.gezinme]
atla = "Перейти к содержимому"
`

// withExtraLanguage builds a file system holding the real base pack
// plus one supplied here.
func withExtraLanguage(t *testing.T, name, body string) fstest.MapFS {
	t.Helper()
	base, err := messagesFS.ReadFile("messages/tr.toml")
	if err != nil {
		t.Fatal(err)
	}
	return fstest.MapFS{
		"diller/tr.toml": {Data: base},
		"diller/" + name: {Data: []byte(body)},
	}
}

// TestAddingALanguageNeedsNoCodeChange is the headline claim of the
// whole design, checked rather than asserted.
func TestAddingALanguageNeedsNoCodeChange(t *testing.T) {
	cats, err := loadCatalogsFS(withExtraLanguage(t, "ru.toml", russianPack), "diller")
	if err != nil {
		t.Fatalf("loading a new language failed: %v", err)
	}

	ru := cats.ByCode("ru")
	if ru == nil {
		t.Fatal("the new pack did not load")
	}
	if ru.Name != "Русский" {
		t.Errorf("endonym = %q", ru.Name)
	}

	// It is reachable by negotiation, which is what makes it usable
	// rather than merely present.
	if got := cats.Match("ru-RU,ru;q=0.9"); got != ru {
		t.Errorf("Accept-Language ru-RU resolved to %q", got.Code)
	}

	// It renders its own words where it has them.
	if got := ru.T("gezinme.atla"); got != "Перейти к содержимому" {
		t.Errorf("gezinme.atla = %q", got)
	}
	// And falls back to the base language where it does not, rather than
	// leaving a hole. This pack deliberately translates two keys out of
	// dozens, which is what a half-finished translation looks like.
	if got := ru.T("giris.govde"); got != cats.Base().T("giris.govde") {
		t.Errorf("an untranslated key returned %q instead of the base text", got)
	}
	if len(cats.Gaps()["ru"]) == 0 {
		t.Error("the gaps were not reported, so nobody would know the pack is unfinished")
	}

	// Its dates use its own month names and its own ordering.
	f := NewFormatter(ru, time.UTC)
	when := time.Date(2026, time.August, 17, 14, 5, 0, 0, time.UTC)
	if got, want := f.Date(when), "17 августа 2026"; got != want {
		t.Errorf("Date = %q, want %q", got, want)
	}
	if got, want := f.Weekday(when), "понедельник"; got != want {
		t.Errorf("Weekday = %q, want %q", got, want)
	}
}

// TestPluralFormsFollowCLDR is why the plural table is not a home-grown
// one/many pair. Russian picks a different form for 1, 2 and 5, and no
// rule anybody would invent by hand gets that right.
func TestPluralFormsFollowCLDR(t *testing.T) {
	cats, err := loadCatalogsFS(withExtraLanguage(t, "ru.toml", russianPack), "diller")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		code string
		n    int
		want string
	}{
		// Turkish does not inflect after a numeral. The pack supplies
		// only "other" and never has to know the mechanism exists.
		{"tr", 1, "1 gün"},
		{"tr", 2, "2 gün"},
		{"tr", 5, "5 gün"},
		{"ru", 1, "1 день"},
		{"ru", 2, "2 дня"},
		{"ru", 5, "5 дней"},
		{"ru", 21, "21 день"},
		{"ru", 11, "11 дней"},
	}
	for _, tc := range cases {
		lang := cats.ByCode(tc.code)
		if lang == nil {
			t.Fatalf("no pack %q", tc.code)
		}
		if got := NewFormatter(lang, time.UTC).Days(tc.n); got != tc.want {
			t.Errorf("%s Days(%d) = %q, want %q", tc.code, tc.n, got, tc.want)
		}
	}
}

// TestEnglishInflectsWhereTurkishDoesNot checks the shipped pair, since
// this is the difference a Turkish-first design forgets.
func TestEnglishInflectsWhereTurkishDoesNot(t *testing.T) {
	en := testLang(t, "en")
	f := NewFormatter(en, time.UTC)
	for n, want := range map[int]string{1: "1 day", 2: "2 days", 90: "90 days"} {
		if got := f.Days(n); got != want {
			t.Errorf("Days(%d) = %q, want %q", n, got, want)
		}
	}
	if got := f.Duration(time.Hour + time.Minute); got != "1 hour 1 minute" {
		t.Errorf("Duration = %q", got)
	}
}

// TestEnglishFormattingIsNotTurkishFormatting proves the formatter is
// actually driven by the language rather than carrying Turkish defaults
// that happen to be applied to English text.
func TestEnglishFormattingIsNotTurkishFormatting(t *testing.T) {
	tr := NewFormatter(testLang(t, "tr"), time.UTC)
	en := NewFormatter(testLang(t, "en"), time.UTC)
	when := time.Date(2026, time.August, 17, 14, 5, 0, 0, time.UTC)

	cases := []struct{ name, trGot, trWant, enGot, enWant string }{
		{"thousands", tr.Number(1234567), "1.234.567", en.Number(1234567), "1,234,567"},
		{"decimal", tr.Decimal(1234.5, 1), "1.234,5", en.Decimal(1234.5, 1), "1,234.5"},
		// The percent sign leads in Turkish and trails in English. That
		// difference comes from x/text, which is the reason to route
		// through it instead of concatenating a character.
		{"percent", tr.Percent(0.457, 1), "%45,7", en.Percent(0.457, 1), "45.7%"},
		{"date", tr.Date(when), "17 Ağustos 2026", en.Date(when), "August 17, 2026"},
		{"short date", tr.ShortDate(when), "17.08.2026", en.ShortDate(when), "2026-08-17"},
		{"date and time", tr.DateTime(when), "17 Ağustos 2026, 14:05", en.DateTime(when), "August 17, 2026 at 14:05"},
		{"weekday", tr.Weekday(when), "Pazartesi", en.Weekday(when), "Monday"},
	}
	for _, tc := range cases {
		if tc.trGot != tc.trWant {
			t.Errorf("%s: tr = %q, want %q", tc.name, tc.trGot, tc.trWant)
		}
		if tc.enGot != tc.enWant {
			t.Errorf("%s: en = %q, want %q", tc.name, tc.enGot, tc.enWant)
		}
	}
}

func TestNegotiationOrder(t *testing.T) {
	cats := testCatalogs(t)
	cases := []struct {
		name      string
		accept    string
		preferred []string
		want      string
	}{
		{"nothing at all", "", nil, "tr"},
		{"deployment default", "", []string{"en"}, "en"},
		// A choice somebody made beats the browser's list.
		{"preference beats the browser", "en-GB,en;q=0.9", []string{"tr"}, "tr"},
		{"browser when nothing was chosen", "en-GB,en;q=0.9", nil, "en"},
		{"browser when the preference is empty", "en", []string{""}, "en"},
		// A language nobody translated falls back rather than 404ing the
		// reader out of their own panel.
		{"unservable language", "de-DE,de;q=0.9", nil, "tr"},
		// A preference naming a pack this build does not carry is
		// ignored, not fatal: the deployment still has to render.
		{"unknown preference", "en", []string{"kl"}, "en"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cats.Match(tc.accept, tc.preferred...)
			if got.Code != tc.want {
				t.Errorf("Match(%q, %v) = %q, want %q", tc.accept, tc.preferred, got.Code, tc.want)
			}
		})
	}
}

func TestMiddlewarePutsTheLanguageOnTheRequest(t *testing.T) {
	cats := testCatalogs(t)
	var seen *Language
	record := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = LanguageFrom(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Language", "en")

	LanguageMiddleware(cats, "tr", record).ServeHTTP(httptest.NewRecorder(), req)
	if seen == nil {
		t.Fatal("no language reached the handler")
	}
	// The deployment default is a choice somebody made, so it wins over
	// the browser's list.
	if seen.Code != "tr" {
		t.Errorf("handler saw %q, want the configured default", seen.Code)
	}

	seen = nil
	LanguageMiddleware(cats, "", record).ServeHTTP(httptest.NewRecorder(), req)
	if seen == nil || seen.Code != "en" {
		t.Fatalf("with no configured default the browser should decide; got %v", seen)
	}
}

func TestLanguageFromAnUnmarkedRequestIsNil(t *testing.T) {
	if LanguageFrom(httptest.NewRequest("GET", "/", nil)) != nil {
		t.Error("a request nothing negotiated reported a language")
	}
	if LanguageFrom(nil) != nil {
		t.Error("a nil request reported a language")
	}
}

// TestPackValidationRefusesWhatWouldRenderNonsense walks the ways a
// translation goes wrong quietly. Each case here is something that
// produces a page nobody would notice was broken.
func TestPackValidationRefusesWhatWouldRenderNonsense(t *testing.T) {
	cases := []struct {
		name, edit, replacement, want string
	}{
		{
			name:        "eleven months",
			edit:        `"июля", "августа",`,
			replacement: `"июля",`,
			want:        "want 12",
		},
		{
			name:        "date pattern with no year",
			edit:        `tarih = "{gun} {ay} {yil}"`,
			replacement: `tarih = "{gun} {ay}"`,
			want:        "{yil}",
		},
		{
			name:        "relative phrase with nowhere for the number",
			edit:        `dakika_once = "%d минут назад"`,
			replacement: `dakika_once = "минут назад"`,
			want:        "exactly one %d",
		},
		{
			// The trap this exists for: a layout that looks like a date
			// pattern, is printed back verbatim by Go, and makes every
			// timestamp in the panel read the same with no error.
			name:        "a layout that is not a layout",
			edit:        `kisa_tarih = "02.01.2006"`,
			replacement: `kisa_tarih = "gg.aa.yyyy"`,
			want:        "not a Go time layout",
		},
		{
			name:        "a counted unit with nowhere for the number",
			edit:        `other = "%s дня"` + "\n\n[bicim.birim.saat]",
			replacement: `other = "дня"` + "\n\n[bicim.birim.saat]",
			want:        "nowhere to put the number",
		},
		{
			name:        "no direction",
			edit:        `yon = "ltr"`,
			replacement: `yon = "yukaridan-asagi"`,
			want:        `want "ltr" or "rtl"`,
		},
		{
			name:        "a code that is not a language",
			edit:        `kod = "ru"`,
			replacement: `kod = "bu-bir-dil-degil"`,
			want:        "not a language tag",
		},
		{
			name:        "no endonym",
			edit:        `ad = "Русский"`,
			replacement: `ad = ""`,
			want:        "no [dil] ad",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(russianPack, tc.edit) {
				t.Fatalf("the fixture no longer contains %q; this test is checking nothing", tc.edit)
			}
			broken := strings.Replace(russianPack, tc.edit, tc.replacement, 1)
			_, err := loadCatalogsFS(withExtraLanguage(t, "ru.toml", broken), "diller")
			if err == nil {
				t.Fatalf("the loader accepted a pack with %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// TestFileNameAndCodeMustAgree: two independent statements of the same
// fact, and somebody hunting "why is my pack not loading" would
// otherwise have nothing to look at.
func TestFileNameAndCodeMustAgree(t *testing.T) {
	_, err := loadCatalogsFS(withExtraLanguage(t, "rus.toml", russianPack), "diller")
	if err == nil {
		t.Fatal("a pack whose file name disagrees with its code was accepted")
	}
	if !strings.Contains(err.Error(), "must match") {
		t.Fatalf("error %q does not explain the rule", err)
	}
}

func TestABuildWithoutTheBaseLanguageIsRefused(t *testing.T) {
	only := fstest.MapFS{"diller/ru.toml": {Data: []byte(russianPack)}}
	_, err := loadCatalogsFS(only, "diller")
	if err == nil {
		t.Fatal("a build with no base language was accepted")
	}
	if !strings.Contains(err.Error(), BaseLanguageCode) {
		t.Fatalf("error %q does not name the base language", err)
	}
}

func TestNoLanguagePacksAtAllIsRefused(t *testing.T) {
	if _, err := loadCatalogsFS(fstest.MapFS{"diller/README.md": {Data: []byte("x")}}, "diller"); err == nil {
		t.Fatal("a build with no language packs was accepted")
	}
}
