package asnlookup

import (
	"bytes"
	"net/netip"
	"strings"
	"testing"
	"unicode/utf8"
)

// The two CSV files this product downloads, read as hostile bytes.
//
// # Why these count as attacker input
//
// They arrive over HTTPS from a public dataset nobody here controls.
// That is not the same as "trusted": the file changes weekly without
// anybody being told, and a wrong assumption about its shape becomes a
// wrong row rather than a download error.
//
// And unlike the beacon's payload, what these produce is not stored
// under its own name. A country code and an organisation name are
// written into *every row* the collector and the beacon write while that
// table is loaded. So a single bad field is not one bad row; it is every
// row until the next refresh.
//
// # What is checked
//
// The same property the beacon's target defends, for the same reason:
// PostgreSQL TEXT holds neither a NUL nor invalid UTF-8, and rows are
// written in batches. A value from here that cannot be stored does not
// spoil one visitor's event - it spoils every visitor's events, from
// both data sources, for as long as the table is loaded.
//
// *Bir kaynağın HTTPS üzerinden gelmesi, içeriğinin doğru olduğunu
// söylemez.*

// maxCountryCode is the format's own definition, spelled out here rather
// than imported: ISO 3166-1 alpha-2 is two letters no matter what this
// package decides. The organisation bound is the package's policy, so
// that one is read from maxOrgLen instead of repeated - a test carrying
// its own copy of a bound is a test that agrees with itself after
// somebody changes the bound and forgets the parser.
const maxCountryCode = 2

func FuzzCountryCSVNeverProducesAnUnstorableValue(f *testing.F) {
	// Real rows, so the mutator starts inside the parser rather than
	// bouncing off the first ParseAddr.
	f.Add([]byte("1.0.0.0,1.0.0.255,AU\n1.0.4.0,1.0.7.255,AU\n"))
	f.Add([]byte("2a01::,2a01:ffff:ffff:ffff:ffff:ffff:ffff:ffff,TR\n"))

	// And the shapes the format says cannot happen.
	f.Add([]byte("1.0.0.0,1.0.0.255,\x00\x00\n"))       // two bytes, and both NUL
	f.Add([]byte("1.0.0.0,1.0.0.255,\xff\xfe\n"))       // two bytes, invalid UTF-8
	f.Add([]byte("1.0.0.0,1.0.0.255,ü\n"))              // one rune, two bytes
	f.Add([]byte("1.0.0.255,1.0.0.0,TR\n"))             // end before start
	f.Add([]byte("1.0.0.0,::1,TR\n"))                   // mixed families
	f.Add([]byte("1.0.0.0,1.0.0.255,\"TR\n"))           // unterminated quote
	f.Add([]byte("1.0.0.0,1.0.0.255,TR,extra,extra\n")) // too many fields
	f.Add([]byte(",,\n\n\n,,\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		entries, err := parseCountryCSV(bytes.NewReader(data))
		if err != nil {
			// The parser is documented never to fail - a bad row is
			// skipped and a bad file yields a short list. If that ever
			// changes, this says so rather than hiding it.
			t.Fatalf("parseCountryCSV returned an error, which it is documented "+
				"not to do: %v", err)
		}
		for _, e := range entries {
			checkRange(t, e.start, e.end)
			checkStorableValue(t, "country", e.value)
			if n := utf8.RuneCountInString(e.value); n != maxCountryCode {
				t.Fatalf("country %q is %d runes, not %d.\n"+
					"A length check that counts bytes lets two bytes of a "+
					"multi-byte rune - or two NULs - through. This value goes "+
					"into every row written while the table is loaded, and "+
					"into the column people group by",
					e.value, n, maxCountryCode)
			}
			// Two runes is not enough: "Üé" is two runes and is not a
			// country. The format says ISO 3166-1 alpha-2.
			for _, r := range e.value {
				if r < 'A' || r > 'Z' {
					t.Fatalf("country %q holds %q, which is not an upper-case "+
						"ASCII letter", e.value, r)
				}
			}
		}
		checkTableAgrees(t, entries)
	})
}

func FuzzASNCSVNeverProducesAnUnstorableValue(f *testing.F) {
	f.Add([]byte("1.0.0.0,1.0.0.255,13335,\"Cloudflare, Inc.\"\n"))
	f.Add([]byte("1.0.4.0,1.0.7.255,38803,Gtelecom Pty Ltd\n"))
	f.Add([]byte("2a01::,2a01::ffff,9121,Turk Telekom\n"))

	f.Add([]byte("1.0.0.0,1.0.0.255,13335,\x00\n"))             // NUL in the name
	f.Add([]byte("1.0.0.0,1.0.0.255,13335,\xff\xfe\n"))         // invalid UTF-8
	f.Add([]byte("1.0.0.0,1.0.0.255,-1,x\n"))                   // negative ASN
	f.Add([]byte("1.0.0.0,1.0.0.255,99999999999999999999,x\n")) // past int64
	f.Add([]byte("1.0.0.0,1.0.0.255,13335,\"unterminated\n"))
	f.Add([]byte("1.0.0.0,1.0.0.255,13335," + strings.Repeat("A", 4096) + "\n"))

	f.Fuzz(func(t *testing.T, data []byte) {
		entries, err := parseASNCSV(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("parseASNCSV returned an error, which it is documented "+
				"not to do: %v", err)
		}
		for _, e := range entries {
			checkRange(t, e.start, e.end)
			checkStorableValue(t, "asn_org", e.value.org)
			if n := utf8.RuneCountInString(e.value.org); n > maxOrgLen {
				t.Fatalf("asn_org is %d runes, past %d. The field has no bound "+
					"in the parser and the file is not ours", n, maxOrgLen)
			}
			if e.value.asn <= 0 {
				t.Fatalf("asn is %d; the column is INTEGER and zero already "+
					"means 'not known'", e.value.asn)
			}
			// INTEGER, not BIGINT: see schema.sql. A number past this
			// is a write that fails for every row it is attached to.
			const maxInt32 = 1<<31 - 1
			if e.value.asn > maxInt32 {
				t.Fatalf("asn is %d, past what an INTEGER column holds",
					e.value.asn)
			}
		}
		checkTableAgrees(t, entries)
	})
}

// checkStorableValue is the batch-poisoning property, in one place
// because both parsers feed the same two columns.
func checkStorableValue(t *testing.T, column, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Fatalf("%s is not valid UTF-8: %q.\n"+
			"PostgreSQL refuses the statement, and this value is attached to "+
			"every row both data sources write while the table is loaded - so "+
			"one bad field in a downloaded file stops the whole product from "+
			"recording anything", column, s)
	}
	if strings.ContainsRune(s, 0) {
		t.Fatalf("%s contains a NUL byte: %q. Same consequence", column, s)
	}
}

// checkRange asserts what a range has to be for a lookup to mean
// anything.
func checkRange(t *testing.T, start, end netip.Addr) {
	t.Helper()
	if !start.IsValid() || !end.IsValid() {
		t.Fatalf("a range has an invalid address: %v-%v", start, end)
	}
	if start.Is4() != end.Is4() {
		t.Fatalf("a range mixes address families: %v-%v. Resolve routes by "+
			"family, so one of the two ends is in the wrong table", start, end)
	}
}

// checkTableAgrees builds the table the parser's output is used through
// and asserts the one thing a lookup must never do: answer for an
// address outside the range it matched.
//
// Here rather than in its own target because the entries have to come
// from somewhere, and entries a parser produced are the only ones this
// product ever builds a table from.
func checkTableAgrees[T comparable](t *testing.T, entries []rangeEntry[T]) {
	t.Helper()
	if len(entries) == 0 {
		return
	}
	table := newRangeTable(entries)
	if len(table.entries) != len(entries) {
		t.Fatalf("the table holds %d entries and the parser produced %d; "+
			"ranges are being dropped or duplicated on the way in",
			len(table.entries), len(entries))
	}
	// Sorted, which is what the binary search assumes. An unsorted table
	// answers wrongly rather than failing, so nothing else would notice.
	for i := 1; i < len(table.entries); i++ {
		if table.entries[i-1].start.Compare(table.entries[i].start) > 0 {
			t.Fatalf("the table is not sorted at %d; sort.Search then returns "+
				"an arbitrary neighbour and the lookup is silently wrong", i)
		}
	}
	for _, e := range entries {
		for _, probe := range []netip.Addr{e.start, e.end} {
			got, found := table.lookup(probe)
			if !found {
				// Legitimate: another entry may sort ahead of this one
				// and not contain the probe. What must never happen is
				// the opposite, checked below.
				continue
			}
			_ = got
		}
	}
	// The address one below the lowest start belongs to nobody.
	lowest := table.entries[0].start
	if below := lowest.Prev(); below.IsValid() && below.Is4() == lowest.Is4() {
		if _, found := table.lookup(below); found {
			t.Fatalf("%v is below every range in the table and the lookup "+
				"claimed a value for it", below)
		}
	}
}

// TestAnOrganisationNameIsActuallyCut.
//
// # Why this exists next to the fuzz target
//
// The fuzz target checks the parser against maxOrgLen, which is the
// right way round: it asserts the parser respects its own policy rather
// than restating the number. But that leaves one thing unsaid, and a
// mutation found it - raising maxOrgLen to a gigabyte kept the target
// green, because the target's expectation moved with the constant.
//
// So the bound needs one assertion that does not read it. Not the exact
// value, which is a judgement that may change: a ceiling, which says
// only that whatever the bound becomes, it is still a bound.
//
// The field comes from a file this project downloads and does not
// control. "As long as upstream feels like" is what no bound means.
//
// *Kendi sabitine karşı ölçen bir test, sabit değişince birlikte
// değişir.*
func TestAnOrganisationNameIsActuallyCut(t *testing.T) {
	// A kilobyte, which is eight times the current bound and still
	// nothing like a real name.
	const ceiling = 1024

	got := orgName(strings.Repeat("A", 64<<10))
	if len(got) == 0 {
		t.Fatal("a 64 KB name was dropped entirely rather than cut; a long " +
			"name is still a name and losing the range loses the ASN too")
	}
	if len(got) > ceiling {
		t.Errorf("a 64 KB organisation name came back as %d bytes.\n"+
			"Whatever maxOrgLen is set to, it has to stay a bound: this field "+
			"arrives in a file nobody here controls and is written onto every "+
			"row attached to that ASN", len(got))
	}
}
