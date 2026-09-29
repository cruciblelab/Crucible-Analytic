package panel

import (
	"errors"
	"testing"
	"time"
)

// TestAnUntilValueIsAMomentWithinTheLongestChoice: what the store accepts
// for a KindUntil setting (the recording pause, PLAN §4, #3).
//
// The bound is asked from both sides of the longest choice, written out
// rather than read from the definition: a test that took the bound from
// the constant it checks would move with it.
func TestAnUntilValueIsAMomentWithinTheLongestChoice(t *testing.T) {
	now := time.Now()
	week := 7 * 24 * time.Hour
	cases := []struct {
		name  string
		value any
		ok    bool
	}{
		{"empty is not paused", "", true},
		{"an hour ahead", now.Add(time.Hour).Format(time.RFC3339), true},
		{"a minute inside the longest choice", now.Add(week - time.Minute).Format(time.RFC3339), true},
		{"a minute past the longest choice", now.Add(week + time.Minute).Format(time.RFC3339), false},
		{"a year ahead", now.AddDate(1, 0, 0).Format(time.RFC3339), false},
		{"in the past, which is simply not in effect", now.Add(-time.Hour).Format(time.RFC3339), true},
		{"not a timestamp", "yarın akşam", false},
		{"a duration rather than a moment", "6h", false},
		{"not text", 360, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Validate(KeyCollectionPausedUntil, tc.value)
			if tc.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("accepted as %v", got)
				}
				if !errors.Is(err, ErrInvalidSetting) {
					t.Errorf("refused with %v, want an invalid-setting error", err)
				}
			}
		})
	}
}

// TestAnUntilValueIsStoredInUTC: one form in the table whatever zone the
// caller wrote it in, so the services and the page compare like with
// like - and the moment itself does not move.
func TestAnUntilValueIsStoredInUTC(t *testing.T) {
	istanbul := time.FixedZone("TRT", 3*3600)
	at := time.Now().Add(2 * time.Hour).In(istanbul).Truncate(time.Second)
	got, err := Validate(KeyCollectionPausedUntil, at.Format(time.RFC3339))
	if err != nil {
		t.Fatal(err)
	}
	text, _ := got.(string)
	back, err := time.Parse(time.RFC3339, text)
	if err != nil || !back.Equal(at) || back.Location() != time.UTC {
		t.Errorf("stored %q; want the same moment, written in UTC", text)
	}
}

// TestUntilOfSaysWhetherItIsInEffect: the reader both the page and the
// dashboard use, on both sides of the end.
func TestUntilOfSaysWhetherItIsInEffect(t *testing.T) {
	end := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	value := end.Format(time.RFC3339)
	if _, active := UntilOf(value, end.Add(-time.Second)); !active {
		t.Error("a second before the end is not in effect")
	}
	if _, active := UntilOf(value, end); active {
		t.Error("the end itself is still in effect")
	}
	for _, v := range []any{"", "bozuk", nil, 5} {
		if _, active := UntilOf(v, end.Add(-time.Hour)); active {
			t.Errorf("%v reads as in effect", v)
		}
	}
}

// TestThePauseIsAChoiceOfDurationsThatEndsWithinAWeek pins the choices the
// page offers - a week at most, because a pause is a repair and a
// permanent one is removing the site.
func TestThePauseIsAChoiceOfDurationsThatEndsWithinAWeek(t *testing.T) {
	def := registry[KeyCollectionPausedUntil]
	want := []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour, 168 * time.Hour}
	if len(def.Until) != len(want) {
		t.Fatalf("choices = %v, want %v", def.Until, want)
	}
	for i := range want {
		if def.Until[i] != want[i] {
			t.Errorf("choice %d = %s, want %s", i, def.Until[i], want[i])
		}
	}
	if def.Kind != KindUntil || def.Scope != ScopeSite || !def.Live || def.RequiresDeveloperPassword {
		t.Errorf("definition = %+v; a live, per-site, until setting without the password", def)
	}
}
