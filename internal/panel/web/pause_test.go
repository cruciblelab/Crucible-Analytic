package web

import (
	"strings"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
	"github.com/cruciblelab/crucible-analytic/internal/settings"
)

func pauseDefinition(t *testing.T) panel.Definition {
	t.Helper()
	def, ok := panel.Lookup(panel.KeyCollectionPausedUntil)
	if !ok {
		t.Fatal("the pause is not in the registry")
	}
	return def
}

// TestThePauseChoicesAreLabelledInTheirOwnUnits: each duration in hours
// or in whole days, in the language's own plural - "1 hours" is what a
// single format string gave in English.
func TestThePauseChoicesAreLabelledInTheirOwnUnits(t *testing.T) {
	catalogs, err := ui.LoadCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	view := panel.SettingView{Definition: pauseDefinition(t), Value: ""}
	for code, want := range map[string][]string{
		"tr": {"1 saat", "6 saat", "1 gün", "7 gün"},
		"en": {"1 hour", "6 hours", "1 day", "7 days"},
	} {
		row := settingRowFor(view, catalogs.ByCode(code), time.Now())
		if len(row.UntilChoices) != len(want) {
			t.Fatalf("%s: %d choices, want %d", code, len(row.UntilChoices), len(want))
		}
		for i, choice := range row.UntilChoices {
			if choice.Label != want[i] {
				t.Errorf("%s: choice %d is labelled %q, want %q", code, i, choice.Label, want[i])
			}
		}
	}
	minutes := []int{60, 360, 1440, 10080}
	row := settingRowFor(view, catalogs.ByCode("tr"), time.Now())
	for i, choice := range row.UntilChoices {
		if choice.Minutes != minutes[i] {
			t.Errorf("choice %d posts %d minutes, want %d", i, choice.Minutes, minutes[i])
		}
	}
}

// TestThePauseRowSaysWhetherItIsInEffect: the row's state comes from the
// stored end and the clock, on both sides of the end.
func TestThePauseRowSaysWhetherItIsInEffect(t *testing.T) {
	lang := turkishCatalogue(t)
	end := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	view := panel.SettingView{Definition: pauseDefinition(t), Value: end.Format(time.RFC3339)}
	if row := settingRowFor(view, lang, end.Add(-time.Second)); !row.UntilActive || !row.UntilAt.Equal(end) {
		t.Errorf("a second before the end: active %v at %s, want active at %s", row.UntilActive, row.UntilAt, end)
	}
	if row := settingRowFor(view, lang, end); row.UntilActive {
		t.Error("at the end the row still says the pause is on")
	}
}

// TestAPostedDurationIsOneTheDefinitionOffers: the form posts minutes;
// only the offered ones become an end, zero resumes, and nothing else
// becomes a moment.
func TestAPostedDurationIsOneTheDefinitionOffers(t *testing.T) {
	def := pauseDefinition(t)
	now := time.Date(2026, 9, 29, 9, 30, 0, 0, time.FixedZone("TRT", 3*3600))
	got, err := untilValue(def, "360", now)
	if err != nil || got != "2026-09-29T12:30:00Z" {
		t.Errorf("360 minutes gave %v (%v); want six hours on, written in UTC", got, err)
	}
	if got, err := untilValue(def, "0", now); err != nil || got != "" {
		t.Errorf("0 gave %v (%v); want the empty value that resumes", got, err)
	}
	for _, raw := range []string{"7", "-60", "10081", "", "6h", "360.0"} {
		if got, err := untilValue(def, raw, now); err == nil {
			t.Errorf("%q was accepted as %v", raw, got)
		}
	}
}

// TestThePausedCounterIsDrawnOnlyWhenItHeldSomething: every writer
// reports the counter, zero included, so the page draws it only once a
// pause has held something back - and then in the heartbeat's order,
// after what was accepted.
func TestThePausedCounterIsDrawnOnlyWhenItHeldSomething(t *testing.T) {
	lang := turkishCatalogue(t)
	label := func(key string) string { return lang.T("saglik.sayac." + key) }

	for _, c := range labelledCounters(lang, map[string]int64{
		heartbeat.CounterAccepted: 10, heartbeat.CounterPaused: 0,
	}) {
		if c.Label == label(heartbeat.CounterPaused) {
			t.Error("a pause that held nothing is drawn")
		}
	}

	drawn := labelledCounters(lang, map[string]int64{
		heartbeat.CounterAccepted: 10, heartbeat.CounterPaused: 3, heartbeat.CounterRejected: 1,
	})
	var labels []string
	for _, c := range drawn {
		labels = append(labels, c.Label)
		if c.Label == label(heartbeat.CounterPaused) && c.Value != 3 {
			t.Errorf("the held count reads %d, want 3", c.Value)
		}
	}
	want := []string{label(heartbeat.CounterAccepted), label(heartbeat.CounterPaused), label(heartbeat.CounterRejected)}
	if len(labels) != len(want) {
		t.Fatalf("drawn %q, want %q", labels, want)
	}
	for i := range want {
		if labels[i] != want[i] {
			t.Errorf("drawn %q, want %q", labels, want)
			break
		}
	}
}

// TestPastPausesNameTheMostRecentAndCountTheRest: the notice names the
// three most recent stretches, oldest first, and counts the ones before
// them - asked on both sides of three.
func TestPastPausesNameTheMostRecentAndCountTheRest(t *testing.T) {
	lang := turkishCatalogue(t)
	f := ui.NewFormatter(lang, time.UTC)
	t0 := time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)
	var spans []panel.PauseSpan
	for i := range 5 {
		from := t0.AddDate(0, 0, i)
		spans = append(spans, panel.PauseSpan{From: from, Until: from.Add(2 * time.Hour)})
	}
	named := func(span panel.PauseSpan) string {
		return lang.Tf("pano.duraklatma_araligi", f.DateTime(span.From), f.DateTime(span.Until))
	}

	text := pastPausesText(lang, f, spans)
	for i, span := range spans {
		if in := strings.Contains(text, named(span)); in != (i >= 2) {
			t.Errorf("stretch %d named: %v; want only the three most recent", i, in)
		}
	}
	if !strings.Contains(text, lang.Tf("pano.duraklatma_oncesi", 2)) {
		t.Errorf("the two earlier stretches are not counted: %q", text)
	}
	if strings.Index(text, named(spans[2])) > strings.Index(text, named(spans[4])) {
		t.Errorf("the stretches are not oldest first: %q", text)
	}

	if text := pastPausesText(lang, f, spans[:3]); strings.Contains(text, "öncesinde") {
		t.Errorf("three stretches count some earlier ones: %q", text)
	}
}

// TestTheNoticeNamesPausesThatEndedAndLastedAPoll: a running pause is the
// banner's, and one lifted within a settings poll may never have reached a
// writer - asked on both sides of the minute, and of now.
func TestTheNoticeNamesPausesThatEndedAndLastedAPoll(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	at := now.Add(-3 * time.Hour)
	spans := []panel.PauseSpan{
		{From: at, Until: at.Add(59 * time.Second)},
		{From: at, Until: at.Add(time.Minute)},
		{From: now.Add(-time.Hour), Until: now},
		{From: now.Add(-time.Hour), Until: now.Add(time.Nanosecond)},
	}
	got := endedPauses(spans, now)
	if len(got) != 2 || got[0] != spans[1] || got[1] != spans[2] {
		t.Errorf("named %v; want the minute-long stretch and the one ending now", got)
	}
	// The minute is the services' poll, not a number of its own.
	if shortestReportedPause != settings.DefaultInterval {
		t.Errorf("the notice skips stretches under %s and the services poll every %s",
			shortestReportedPause, settings.DefaultInterval)
	}
}
