package panel

import (
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/profile"
)

// TestAReportedProfileBecomesALevelOrNothing.
//
// The one place where what a service says becomes what the panel
// believes. A wrong answer here is not a crash: it is a sentence under a
// section telling a customer their data is not being collected when it
// is, or drawing a confident zero when it is not.
//
// The unknown cases matter more than the known ones. A service newer
// than this panel reports a profile id this build has never heard of,
// and the only safe reading is "no answer" - which the caller turns into
// the behaviour the page had before any of this existed.
func TestAReportedProfileBecomesALevelOrNothing(t *testing.T) {
	// Derived from the offered set rather than written out, so a fourth
	// profile is covered the day it is added.
	for _, p := range profile.All() {
		t.Run(p.ID, func(t *testing.T) {
			level, ok := levelOf(p.ID)
			if !ok {
				t.Fatalf("%q is an offered profile and levelOf does not know it.\n"+
					"Every section that needs a level would then read as "+
					"'unknown' on a deployment running it", p.ID)
			}
			if level != p.Level {
				t.Errorf("%q maps to %q, and the profile says %q",
					p.ID, level, p.Level)
			}
			if !level.Known() {
				t.Errorf("%q maps to %q, which reports itself unknown - so the "+
					"caller discards an answer this function just gave it",
					p.ID, level)
			}
		})
	}

	for _, tc := range []struct {
		name string
		id   string
		why  string
	}{
		{
			name: "bos", id: "",
			why: "no service has reported one: a fresh install, a service " +
				"that is down, or a binary older than the profile column",
		},
		{
			name: "bilinmeyen", id: "devasa",
			why: "a service newer than this panel. Guessing at it would tell " +
				"a customer their data is missing on the day they upgraded " +
				"the collector first",
		},
		{
			name: "seviye adi profil adi degil", id: "ulke",
			why: "'ulke' is a Level, not a profile id. The two vocabularies " +
				"are next to each other and reading one as the other is the " +
				"mistake this test exists for",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			level, ok := levelOf(tc.id)
			if ok {
				t.Errorf("levelOf(%q) answered %q.\n%s", tc.id, level, tc.why)
			}
			if level.Known() {
				t.Errorf("levelOf(%q) returned %q, which reports itself known",
					tc.id, level)
			}
		})
	}
}
