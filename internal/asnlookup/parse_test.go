package asnlookup

import (
	"strings"
	"testing"
)

func TestParseDelegatedStats_RealisticFixture(t *testing.T) {
	// A synthetic file shaped exactly like a real RIR delegated-extended
	// stats file: header, ipv4/ipv6/asn records, a summary line, a
	// comment, and a blank line - everything parseDelegatedStats must
	// either accept or skip.
	const fixture = `2|arin|20250101|123456|19830101|20250101|-0400
# comment line, must be ignored
arin|US|ipv4|12.0.0.0|16777216|19830101|allocated|a1b2c3d4

arin|US|ipv4|192.0.2.0|256|20000101|assigned|deadbeef01
arin|DE|ipv4|198.51.100.0|1024|20100615|allocated|deadbeef02
arin|US|asn|7018|1|19850101|allocated|a1b2c3d4
arin|US|ipv6|2600::|20|20120101|allocated|a1b2c3d4
arin|*|ipv4|*|54321|summary
arin|ZZ|ipv4|203.0.113.0|256|20200101|allocated|deadbeef03
`
	entries, err := parseDelegatedStats(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("parseDelegatedStats: %v", err)
	}

	want := []rangeEntry{
		{start: 0x0C000000, end: 0x0C000000 + 16777216 - 1, country: "US"}, // 12.0.0.0/8
		{start: 0xC0000200, end: 0xC0000200 + 256 - 1, country: "US"},      // 192.0.2.0/24
		{start: 0xC6336400, end: 0xC6336400 + 1024 - 1, country: "DE"},     // 198.51.100.0/22
	}
	if len(entries) != len(want) {
		t.Fatalf("got %d entries, want %d: %+v", len(entries), len(want), entries)
	}
	for i, e := range entries {
		if e != want[i] {
			t.Errorf("entries[%d] = %+v, want %+v", i, e, want[i])
		}
	}
}

func TestParseDelegatedStats_EmptyInput(t *testing.T) {
	entries, err := parseDelegatedStats(strings.NewReader(""))
	if err != nil {
		t.Fatalf("parseDelegatedStats: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("entries = %+v, want empty", entries)
	}
}

func TestParseDelegatedStats_MalformedLinesAreSkippedNotFatal(t *testing.T) {
	const fixture = `arin|US|ipv4|not-an-ip|256|20000101|allocated|x
arin|US|ipv4|192.0.2.0|not-a-number|20000101|allocated|x
arin|US|ipv4|192.0.2.0|256|20000101
too|few|fields
arin|US|ipv4|198.51.100.0|512|20000101|allocated|x
`
	entries, err := parseDelegatedStats(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("parseDelegatedStats: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want exactly the one well-formed record", entries)
	}
	if entries[0].country != "US" {
		t.Errorf("entries[0].country = %q, want US", entries[0].country)
	}
}

func TestParseDelegatedStats_LowercaseCountryIsUppercased(t *testing.T) {
	entries, err := parseDelegatedStats(strings.NewReader("arin|us|ipv4|192.0.2.0|256|20000101|allocated|x\n"))
	if err != nil {
		t.Fatalf("parseDelegatedStats: %v", err)
	}
	if len(entries) != 1 || entries[0].country != "US" {
		t.Fatalf("entries = %+v, want one entry with country US", entries)
	}
}
