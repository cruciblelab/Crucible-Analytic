package botdata

import (
	"os"
	"path/filepath"
	"testing"
	"unicode/utf8"
)

// The JSON this product downloads from a community archive.
//
// # What this parser can get wrong, and why it is not a crash
//
// The other two targets in this group defend a write: a value that
// PostgreSQL refuses takes down the batch it was in. Nothing here
// reaches a TEXT column - a label decides a boolean, is_known_bot_ja4 -
// so the failure this one is about is different and worse to explain to
// a customer.
//
// This package's own comments name both halves of it:
//
//	An empty-string key here would label all non-TLS and unparseable
//	traffic as a known bot.
//
//	The archive is community-submitted and includes reference
//	fingerprints for real browsers; keeping those would make the panel
//	call every ordinary visitor a known bot.
//
// Neither is a crash, an error, or anything an operator would see. The
// product keeps working and every number in it is wrong - which is the
// one outcome an analytics tool cannot recover from, because the
// customer's only way to notice is to already distrust the answer.
//
// So what is checked is not "it survived". It is: nothing that must
// never be in the map is in the map, whatever the source sends.
//
// *Çökmeyen bir ayrıştırıcı, doğru cevap verdiği anlamına gelmez.*
func FuzzTheArchiveCannotLabelABrowserAsABot(f *testing.F) {
	// The shape the source actually sends.
	f.Add([]byte(`[{"ja4":"t13d1516h2_8daaf6152771_b186095e22b6","bot_type":"crawler",` +
		`"label":"Googlebot","user_agent":"Mozilla/5.0 (compatible; Googlebot/2.1)",` +
		`"submission_count":42}]`))
	// Both wrappers this parser accepts, because a source nobody here
	// controls is free to start wrapping its payload.
	f.Add([]byte(`{"entries":[{"ja4":"t13d","bot_type":"scraper","label":"x"}]}`))
	f.Add([]byte(`{"data":[{"ja4":"t13d","bot_type":"scraper","label":"x"}]}`))

	// And the shapes that would poison the answer.
	f.Add([]byte(`[{"ja4":"","bot_type":"crawler","label":"everything"}]`))
	f.Add([]byte(`[{"ja4":"t13d","bot_type":"browser","label":"Chrome"}]`))
	f.Add([]byte(`[{"ja4":"t13d","bot_type":"BROWSER","label":"Chrome"}]`))
	f.Add([]byte(`[{"ja4":"t13d","bot_type":"browser ","label":"Chrome"}]`))
	f.Add([]byte(`[{"ja4":"t13d","bot_type":"crawler"},{"ja4":"t13d","bot_type":"browser"}]`))
	f.Add([]byte(`[{"ja4":"t13d","bot_type":"","label":"","name":""}]`))
	f.Add([]byte(`[{"ja4":"t13d","bot_type":"crawler","submission_count":-1}]`))
	f.Add([]byte(`{"entries":[],"data":[]}`))
	f.Add([]byte(`null`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		entries, dropped, err := parseArchive(raw)
		if err != nil {
			return
		}
		if dropped < 0 {
			t.Fatalf("dropped is %d; the page shows it as a count", dropped)
		}

		for _, e := range entries {
			if e.JA4 == "" {
				t.Fatalf("an entry has an empty JA4. That key matches every "+
					"connection with no fingerprint - all non-TLS and every "+
					"handshake this build could not parse - and labels the "+
					"lot as a known bot: %+v", e)
			}
			for _, bt := range e.BotTypes {
				if bt == browserType {
					t.Fatalf("%q survived the filter carrying bot type %q. "+
						"Every real visitor whose TLS stack matches this "+
						"fingerprint is now a known bot in the panel, and "+
						"nothing in the product says so", e.JA4, bt)
				}
			}
		}

		// The map is what the collector consults, and it is built from
		// the entries by a second function. A guard on the entries that
		// is not also true of the map would be a guard on the wrong
		// object.
		labels := labelsOf(entries)
		if _, ok := labels[""]; ok {
			t.Fatal("the lookup map has an empty key, so every connection " +
				"without a fingerprint is a known bot")
		}
		if len(labels) > len(entries) {
			t.Fatalf("the map has %d keys from %d entries", len(labels), len(entries))
		}
	})
}

// FuzzAFileThisBuildWroteIsAFileThisBuildCanRead.
//
// # Why a round trip is worth fuzzing at all
//
// Save writes the file the collector reads at every restart, and Load's
// own comment says an unreadable file is an error rather than "no data",
// deliberately - so a value that Save can write and Load cannot read
// does not degrade the bot labels, it stops the collector from starting.
//
// The pair is written by one person on one afternoon and read forever
// afterwards, which is exactly the shape where a field that needs
// escaping is discovered by a customer.
//
// A round trip cannot see anything that breaks both sides together - so
// what is compared is the map, whose keys and values came from the
// arbitrary bytes rather than from this package.
func FuzzAFileThisBuildWroteIsAFileThisBuildCanRead(f *testing.F) {
	f.Add(`t13d1516h2_8daaf6152771_b186095e22b6`, `Googlebot`)
	f.Add(`t13d`, `"quoted"`)
	f.Add(`t13d`, "tab\tand\nnewline")
	f.Add(`t13d`, `ünïcödé ve Türkçe`)
	f.Add(`t13d`, "\x00 as text")
	f.Add("a\\b", `back\slash`)

	f.Fuzz(func(t *testing.T, ja4, label string) {
		if ja4 == "" {
			// labelsOf drops it on purpose, so there would be nothing to
			// compare. That drop has its own check above.
			return
		}
		// Invalid UTF-8 is skipped, and the first spelling of this guard
		// was wrong in a way worth writing down: it asked json.Valid,
		// which does not check UTF-8 at all. The fuzzer then handed it
		// "\xc0", the guard said yes, and the round trip failed on
		// encoding/json replacing the byte with U+FFFD.
		//
		// That replacement is documented behaviour of the encoder, not a
		// defect in this package - a Go string may hold bytes that are
		// not UTF-8 and JSON has nowhere to put them. What was defective
		// was the question the test asked.
		if !utf8.ValidString(ja4) || !utf8.ValidString(label) {
			return
		}

		path := filepath.Join(t.TempDir(), "botdata.json")
		want := Set{Labels: map[string]string{ja4: label}, Source: "test"}
		if err := Save(path, want); err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, err := Load(path)
		if err != nil {
			t.Fatalf("Load could not read the file Save just wrote: %v\n"+
				"The collector treats this as a startup error, not as "+
				"missing data", err)
		}
		if len(got.Labels) != 1 || got.Labels[ja4] != label {
			t.Fatalf("wrote %q=%q and read back %#v", ja4, label, got.Labels)
		}
	})
}

// FuzzAFileOnDiskCannotLabelEveryVisitorABot.
//
// # Why Load needs its own target
//
// A mutation found this: removing labelsOf's empty-fingerprint guard
// changed nothing that FuzzTheArchiveCannotLabelABrowserAsABot could
// see, because filterArchive drops those entries before labelsOf is ever
// reached on that path.
//
// The guard is not redundant, though. labelsOf has a second caller, and
// its input does not come from the archive at all - Load reads a file
// off the disk. That file was written by some build of this product, on
// some day, and it is a plain JSON file in a directory an operator can
// edit. Nothing between it and the collector's lookup map re-runs
// filterArchive.
//
// So the two callers need two targets, and the survivor said which one
// was missing.
//
// *Bir korumanın gereksiz görünmesi, onu deneyen testin yanlış yoldan
// geldiği anlamına gelebilir.*
func FuzzAFileOnDiskCannotLabelEveryVisitorABot(f *testing.F) {
	f.Add([]byte(`{"entries":[{"ja4":"t13d","label":"Googlebot"}]}`))
	f.Add([]byte(`{"entries":[{"ja4":"","label":"everything"}]}`))
	f.Add([]byte(`{"entries":[{"ja4":"","label":""},{"ja4":"t13d","label":"x"}]}`))
	f.Add([]byte(`{"entries":[{"ja4":"t13d","bot_types":["browser"],"label":"Chrome"}]}`))
	f.Add([]byte(`{"retrieved_at":"not a time","entries":[]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`[]`))

	f.Fuzz(func(t *testing.T, raw []byte) {
		path := filepath.Join(t.TempDir(), "botdata.json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		set, err := Load(path)
		if err != nil {
			// Load's contract: an unreadable file is an error rather
			// than "no data", so the collector stops instead of silently
			// labelling nothing. Nothing to check past that.
			return
		}
		if set.Labels == nil {
			t.Fatal("Load returned a nil map with no error; Set.Labels is " +
				"documented never to be nil and every caller reads it directly")
		}
		if _, ok := set.Labels[""]; ok {
			t.Fatalf("a file on disk put an empty fingerprint in the lookup "+
				"map. That key matches every connection with no JA4 - all "+
				"non-TLS traffic and every handshake this build could not "+
				"parse - so the panel calls the lot known bots: %q", raw)
		}
	})
}
