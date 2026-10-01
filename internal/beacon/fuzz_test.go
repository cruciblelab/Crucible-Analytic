package beacon

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Bytes a browser sent, turned into a row.
//
// # Why this parser and not the JSON decoder
//
// encoding/json is not ours and is fuzzed by people who maintain it.
// What is ours is everything after it: Validate decides whether the
// bytes describe an event at all, and BuildRow turns whatever survives
// into the struct that goes to the database.
//
// # The property, and why it is the one that matters
//
// sanitizeText's own comment says it:
//
//	PostgreSQL TEXT cannot hold a NUL byte or invalid UTF-8 at all, and
//	rejects the whole statement when handed one. Because rows are written
//	in batches, one hostile payload would otherwise take down the entire
//	batch it landed in - every other visitor's events included.
//
// So the claim being defended is not "the parser does not crash". It is
// **a malformed event degrades only itself**, and a table-driven test
// cannot establish that: it checks the strings somebody thought of. A
// single field that reaches the row unsanitised is one POST that stops
// every other visitor's events from being written.
//
// Every string in the Row is therefore checked, by reflection rather
// than by name. A test naming the fields it knows about is a test that
// stays green when a field is added - and a new field is exactly when
// somebody forgets to sanitise one.
//
// *Bir alanı adıyla sınayan test, eklenen alanı hiç görmez.*
func FuzzAnEventFromABrowserCannotPoisonTheBatch(f *testing.F) {
	// What the snippet actually sends, so the mutator starts from
	// something that reaches the whole of BuildRow rather than bouncing
	// off Validate. The same reason ja4's corpus starts from real
	// captures.
	f.Add([]byte(`{"site":"magaza","type":"pageview","url":"/urunler?utm_source=google"}`))
	f.Add([]byte(`{"site":"magaza","type":"event","name":"sepete_ekle","url":"/sepet",` +
		`"title":"Sepet","screen_w":1920,"screen_h":1080,"language":"tr-TR"}`))
	f.Add([]byte(`{"site":"magaza","type":"pageview","url":"/","referrer":"https://google.com/search?q=x"}`))

	// And the shapes a browser does not send.
	f.Add([]byte(`{"site":"s","type":"pageview","url":"/\u0000"}`))                    // NUL in a path
	f.Add([]byte(`{"site":"s","type":"pageview","url":"https://evil.example/x?y=1"}`)) // absolute URL
	f.Add([]byte(`{"site":"s","type":"pageview","url":"://"}`))                        // unparseable
	f.Add([]byte(`{"site":"s","type":"event","name":"\ud800","url":"/"}`))             // lone surrogate
	f.Add([]byte(`{"site":"s","type":"pageview","url":"/","screen_w":-2147483648}`))   // negative
	f.Add([]byte(`{"site":"s","type":"pageview","url":"/","screen_w":9223372036854775807}`))
	f.Add([]byte(`{"site":"s","type":"pageview","url":"/","referrer":"//host/path"}`))
	f.Add([]byte(`{"site":"s","type":"pageview","url":"/","title":"` +
		strings.Repeat("ü", 4096) + `"}`)) // past the cap, in multi-byte runes
	f.Add([]byte(`{"site":"s","type":"pageview","url":"/%zz"}`))       // bad percent-escape
	f.Add([]byte(`{"site":"s","type":"pageview","url":"/?a=%ff%fe"}`)) // invalid UTF-8 via escapes

	// Two URLs that parse with an empty path, which is not the same as
	// an empty URL: the early return in splitURL never sees these, so
	// normalizePath is the only thing that turns them into "/". Added
	// after a mutation - normalizePath("") returning "" instead of "/" -
	// walked past the whole corpus above.
	f.Add([]byte(`{"site":"s","type":"pageview","url":"?a=1"}`))
	f.Add([]byte(`{"site":"s","type":"pageview","url":"https://magaza.example"}`))

	// The shape the nightly run found on 2026-09-29 (failing input
	// e036acc39268bbcf): a path of the full cap without its slash, cut
	// first and prefixed after, came out one rune over.
	f.Add([]byte(`{"site":"s","type":"pageview","url":"` + strings.Repeat("a", maxPathLen) + `"}`))

	// And what that finding led to. Three campaign values of four-byte
	// characters - varied, so nothing compresses them - made an index
	// entry PostgreSQL refuses, and the refusal took the whole batch.
	// Each was inside its rune cap; the bound they broke is counted in
	// bytes (see maxCampaignValueBytes).
	f.Add([]byte(`{"site":"s","type":"pageview","url":"/?utm_source=` + fourByteRunes(0, maxCampaignValueLen) +
		`&utm_medium=` + fourByteRunes(1, maxCampaignValueLen) +
		`&utm_campaign=` + fourByteRunes(2, maxCampaignValueLen) + `"}`))

	f.Fuzz(func(t *testing.T, body []byte) {
		var e Event
		// Unknown fields ignored, as the server does; see handleEvent
		// for why strict decoding would lose events during a rollout.
		if err := json.Unmarshal(body, &e); err != nil {
			return
		}
		if err := e.Validate(); err != nil {
			return
		}

		// The enrichment is the server's own, so it is fixed here: the
		// point is what the *payload* can do. Anything hostile arriving
		// through enrichment comes from asnlookup, which has its own
		// target.
		row := BuildRow(e, Enrichment{
			Time:      time.Unix(0, 0).UTC(),
			VisitorID: "sabit",
			Country:   "TR",
		}, DefaultCampaignPolicy())

		checkStorable(t, "Row", reflect.ValueOf(row), map[string]int{
			"EventName": maxNameLen,
			"Path":      maxPathLen,
			"Query":     maxQueryLen,
			"Title":     maxTitleLen,
			"Language":  maxLanguageLen,
		})

		// In bytes, for the values an index keys on. A rune cap says
		// nothing about an index entry, whose limit PostgreSQL counts in
		// bytes; which columns are indexed is the database's answer, and
		// TestWriter_RealTimescaleDB_AMaximalRowFitsEveryIndex asks it.
		// Every campaign value is held to the bound here, not only the
		// three indexed today, because they share one extraction and an
		// index on a fourth is one line of schema away.
		for name, v := range map[string]string{
			"Source": row.Campaign.Source, "Medium": row.Campaign.Medium,
			"Name": row.Campaign.Name, "Term": row.Campaign.Term,
			"Content": row.Campaign.Content, "Ref": row.Campaign.Ref,
			"ClickID": row.Campaign.ClickID,
		} {
			if len(v) > maxCampaignValueBytes {
				t.Fatalf("Campaign.%s is %d bytes, past %d. The campaign index refuses an "+
					"entry over 2704 bytes, and refuses the whole batch with it",
					name, len(v), maxCampaignValueBytes)
			}
		}

		// Path is the one field with a shape as well as a bound. Two
		// spellings of one page are two rows, so "/" and "" and
		// "pricing" must not be three different pages.
		if !strings.HasPrefix(row.Path, "/") {
			t.Fatalf("Path is %q, which does not start with a slash. The same "+
				"page then groups under two names", row.Path)
		}

		// Inside INTEGER, which is what the column is.
		if row.ScreenW < 0 || row.ScreenW > maxScreenPx {
			t.Fatalf("ScreenW is %d, outside [0,%d]", row.ScreenW, maxScreenPx)
		}
		if row.ScreenH < 0 || row.ScreenH > maxScreenPx {
			t.Fatalf("ScreenH is %d, outside [0,%d]", row.ScreenH, maxScreenPx)
		}
	})
}

// checkStorable walks a value and asserts that every string in it could
// be written to a PostgreSQL TEXT column.
//
// Reflection rather than a list of field names, deliberately. The Row
// has twenty-odd fields and one of them is a nested struct; a test that
// named them would pass on the day a twenty-first was added without
// sanitising, which is the day the check was for.
//
// caps names the fields that also have a length bound. A field absent
// from the map is checked for validity but not for length - Campaign's
// members are bounded by the query cap as a whole rather than
// individually, and VisitorID and Country come from the server.
func checkStorable(t *testing.T, path string, v reflect.Value, caps map[string]int) {
	t.Helper()
	switch v.Kind() {
	case reflect.String:
		s := v.String()
		if !utf8.ValidString(s) {
			t.Fatalf("%s is not valid UTF-8: %q.\n"+
				"PostgreSQL refuses the statement, so this one event stops "+
				"every other visitor's events in the same batch from being "+
				"written", path, s)
		}
		if strings.ContainsRune(s, 0) {
			t.Fatalf("%s contains a NUL byte: %q.\nSame consequence: the whole "+
				"batch is refused", path, s)
		}
		if n, ok := caps[fieldName(path)]; ok {
			// Not "the write fails": these columns are unbounded TEXT, and
			// this message said so until a finding put it to the database
			// (2026-10-01: a 1025-rune path wrote). The cap is a promise
			// about every row, and what makes a write fail is an index
			// entry's size - which the byte check below and the
			// integration test hold.
			if got := utf8.RuneCountInString(s); got > n {
				t.Fatalf("%s is %d runes, past its cap of %d. Every row is promised "+
					"the cap, and a field that can break it is a field nobody is bounding",
					path, got, n)
			}
		}
	case reflect.Struct:
		// time.Time has unexported fields and no strings that reach a
		// TEXT column; walking into it would report on its internals.
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return
		}
		for i := range v.NumField() {
			if !v.Type().Field(i).IsExported() {
				continue
			}
			checkStorable(t, path+"."+v.Type().Field(i).Name, v.Field(i), caps)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			checkStorable(t, path, v.Index(i), caps)
		}
	}
}

// fieldName is the last segment of a dotted path.
func fieldName(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[i+1:]
	}
	return path
}

// fourByteRunes is n runes that each take four bytes in UTF-8 and do not
// repeat, so neither a rune cap nor compression hides how large they are.
// One character repeated is the other thing a test reaches for, and it
// is the case that hides the limit: PostgreSQL compresses a long index
// value before measuring it, and 256 copies of one rune compress to
// almost nothing (measured: the same three values that fail varied were
// written repeated). seed varies the run, so three values are three.
//
// CJK Extension B: every code point in it is assigned, and none is one
// textsafe strips.
func fourByteRunes(seed, n int) string {
	const first, span = 0x20000, 0xA6E0
	var b strings.Builder
	for i := range n {
		b.WriteRune(rune(first + (seed*7919+i*104729)%span))
	}
	return b.String()
}

// TestEveryStringInARowIsWalkedByTheFuzzTarget.
//
// The reflection above is only worth having if it actually reaches every
// string. A bug in checkStorable - an unhandled Kind, a wrong recursion -
// would make the fuzz target green by inspecting nothing, and a fuzz
// target that inspects nothing runs millions of times and proves
// nothing.
//
// So: a Row with a NUL planted in every string field in turn, and each
// one has to be caught.
func TestEveryStringInARowIsWalkedByTheFuzzTarget(t *testing.T) {
	// Derived from the type, not listed. A field added to Row is a field
	// this test starts planting in without anybody editing it.
	var planted int
	rt := reflect.TypeOf(Row{})
	for i := range rt.NumField() {
		field := rt.Field(i)
		switch field.Type.Kind() {
		case reflect.String:
			row := Row{}
			reflect.ValueOf(&row).Elem().Field(i).SetString("a\x00b")
			planted++
			expectCaught(t, "Row."+field.Name, row)
		case reflect.Struct:
			if field.Type == reflect.TypeOf(time.Time{}) {
				continue
			}
			for j := range field.Type.NumField() {
				if field.Type.Field(j).Type.Kind() != reflect.String {
					continue
				}
				row := Row{}
				reflect.ValueOf(&row).Elem().Field(i).Field(j).SetString("a\x00b")
				planted++
				expectCaught(t, "Row."+field.Name+"."+field.Type.Field(j).Name, row)
			}
		}
	}
	if planted == 0 {
		t.Fatal("no string field was planted, so this test asserted nothing")
	}
	t.Logf("%d string fields in Row, each one planted and caught", planted)
}

// expectCaught runs checkStorable against a row that must fail, and
// fails this test if it does not.
func expectCaught(t *testing.T, where string, row Row) {
	t.Helper()
	// checkStorable calls Fatalf, which ends the goroutine it runs on,
	// so it is run on one of its own with its own *testing.T.
	caught := make(chan bool, 1)
	go func() {
		sub := &testing.T{}
		defer func() {
			_ = recover()
			caught <- sub.Failed()
		}()
		checkStorable(sub, "Row", reflect.ValueOf(row), nil)
		caught <- sub.Failed()
	}()
	if !<-caught {
		t.Errorf("a NUL planted in %s was not caught. Every fuzz run since is "+
			"a run that inspected less than it says it does", where)
	}
}
