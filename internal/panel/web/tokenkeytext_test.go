package web

import (
	"strings"
	"testing"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/panel"
	"github.com/cruciblelab/crucible-analytic/internal/panel/ui"
	"github.com/cruciblelab/crucible-analytic/internal/tokenkey"
)

// The refusal on privacy.ip_storage = "full" says which service said no,
// and which of the two things is wrong with it.
//
// # Why this is worth its own test
//
// Because the sentence it replaces was already correct and already
// useless. "The configuration it needs is not there yet - the setting's
// own description says what has to be in place first" is true of every
// precondition, and the description it points at says to put the same key
// in both services' config files. On a deployment where the collector
// already has one and the beacon's build is too old to report it, that
// advice sends the reader to edit a file that is correct.
//
// A reader sent to check the thing that is already right will look, find
// nothing wrong, and stop believing the page. That is the failure the
// branch this sits inside was written for, one step further in.
//
// # Both languages, because the page is served in both
//
// And with the words taken from the catalogue rather than spelled here.
// A second spelling passes while the two drift, which is the class of
// defect this project has already paid for in a fixture that hand-wrote
// a wire format.
func TestTheTokenKeyRefusalNamesTheServiceAndTheFix(t *testing.T) {
	catalogs, err := ui.LoadCatalogs()
	if err != nil {
		t.Fatal(err)
	}
	languages := catalogs.Languages()
	if len(languages) < 2 {
		t.Fatalf("%d language(s) loaded; this test is about both", len(languages))
	}

	for _, lang := range languages {
		t.Run(lang.Code, func(t *testing.T) {
			// ---- nothing has reported ----
			//
			// Its own sentence, because nothing is misconfigured: a fresh
			// install has not started its services yet, and telling that
			// installer to go and fix a key would be inventing a fault.
			silent := tokenKeyText(lang, panel.TokenKeyUnready{})
			if want := lang.T("ayarlar.hata.jeton_anahtari_sessiz"); silent != want {
				t.Errorf("with nobody reporting:\n got  %q\n want %q", silent, want)
			}

			// ---- a service with no key, and the same service too old
			// to answer ----
			//
			// The *same* service in both, and that is the whole
			// measurement rather than a tidiness. The first version
			// named collector in one and beacon_writer in the other, so
			// the two sentences differed by the service name whatever
			// the code did with the state - and a mutation collapsing
			// the two states into one message survived it. An assertion
			// whose only distinguishing input is held constant is an
			// assertion nobody wrote.
			const service = "collector"
			absent := tokenKeyText(lang, panel.TokenKeyUnready{Missing: []tokenkey.Report{
				{Service: service, State: heartbeat.TokenKeyAbsent},
			}})
			unknown := tokenKeyText(lang, panel.TokenKeyUnready{Missing: []tokenkey.Report{
				{Service: service, State: heartbeat.TokenKeyUnknown},
			}})

			for _, text := range []string{absent, unknown} {
				if !strings.Contains(text, service) {
					t.Errorf("the refusal does not name the service: %q", text)
				}
			}
			if !strings.Contains(absent, "ip_hash_key") {
				t.Errorf("the refusal does not name the setting to write: %q", absent)
			}
			// The two have to read differently, and that is the whole
			// point of keeping the states apart: one is "edit a file",
			// the other is "upgrade a binary". A sentence that collapsed
			// them would send half its readers to the wrong place.
			if absent == unknown {
				t.Errorf("a service with no key and a service too old to say produce the "+
					"same sentence, so the page cannot tell the reader which fix to "+
					"apply:\n%s", absent)
			}

			// ---- and both at once ----
			//
			// Each service named once, because a deployment with two
			// writers in two different states is the case an operator
			// most needs spelled out.
			both := tokenKeyText(lang, panel.TokenKeyUnready{Missing: []tokenkey.Report{
				{Service: "collector", State: heartbeat.TokenKeyAbsent},
				{Service: "beacon_writer", State: heartbeat.TokenKeyUnknown},
			}})
			for _, name := range []string{"collector", "beacon_writer"} {
				if strings.Count(both, name) != 1 {
					t.Errorf("%q appears %d times in %q, want once",
						name, strings.Count(both, name), both)
				}
			}

			// No language falls back to a marked key, which is what an
			// untranslated message renders as - a page showing a
			// bracketed identifier to a customer.
			for _, text := range []string{silent, absent, unknown, both} {
				if strings.Contains(text, "ayarlar.hata.jeton_anahtari") {
					t.Errorf("%s has no words for this refusal; the page would show a raw "+
						"key: %q", lang.Code, text)
				}
			}
		})
	}
}
