package asnlookup

import (
	"net/netip"
	"strings"
	"testing"
)

func TestParseCountryCSV_RealisticFixture(t *testing.T) {
	// Shaped exactly like a real downloaded user-country-ipv4.csv sample
	// (verified against real data before writing this parser): no header,
	// plain comma-separated, one range per line.
	const fixture = `1.0.0.0,1.0.0.255,AU
1.0.1.0,1.0.3.255,CN
1.0.4.0,1.0.7.255,AU
`
	entries, err := parseCountryCSV(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("parseCountryCSV: %v", err)
	}

	want := []rangeEntry[string]{
		{start: netip.MustParseAddr("1.0.0.0"), end: netip.MustParseAddr("1.0.0.255"), value: "AU"},
		{start: netip.MustParseAddr("1.0.1.0"), end: netip.MustParseAddr("1.0.3.255"), value: "CN"},
		{start: netip.MustParseAddr("1.0.4.0"), end: netip.MustParseAddr("1.0.7.255"), value: "AU"},
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

func TestParseCountryCSV_IPv6Fixture(t *testing.T) {
	// Shaped like a real user-country-ipv6.csv sample: same 3-column
	// format, full/expanded IPv6 addresses.
	const fixture = "2001:200::,2001:200:5ff:ffff:ffff:ffff:ffff:ffff,JP\n"
	entries, err := parseCountryCSV(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("parseCountryCSV: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want exactly 1", entries)
	}
	want := rangeEntry[string]{
		start: netip.MustParseAddr("2001:200::"),
		end:   netip.MustParseAddr("2001:200:5ff:ffff:ffff:ffff:ffff:ffff"),
		value: "JP",
	}
	if entries[0] != want {
		t.Errorf("entries[0] = %+v, want %+v", entries[0], want)
	}
}

func TestParseCountryCSV_QuotedFieldIsHandledCorrectly(t *testing.T) {
	// Country codes never need CSV quoting in practice, but this uses a
	// real encoding/csv reader specifically so that IF a field ever did
	// contain a comma (as the sibling ASN dataset's org-name field
	// routinely does), it wouldn't silently corrupt the row the way a
	// naive strings.Split(",") would.
	const fixture = "1.0.0.0,1.0.0.255,\"AU\"\n"
	entries, err := parseCountryCSV(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("parseCountryCSV: %v", err)
	}
	if len(entries) != 1 || entries[0].value != "AU" {
		t.Fatalf("entries = %+v, want one entry with country AU", entries)
	}
}

func TestParseCountryCSV_EmptyInput(t *testing.T) {
	entries, err := parseCountryCSV(strings.NewReader(""))
	if err != nil {
		t.Fatalf("parseCountryCSV: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("entries = %+v, want empty", entries)
	}
}

func TestParseCountryCSV_MalformedLinesAreSkippedNotFatal(t *testing.T) {
	const fixture = `not-an-ip,1.0.0.255,AU
1.0.0.0,also-not-an-ip,AU
1.0.0.0,1.0.0.255,USA
too,few
1.0.4.0,1.0.7.255,CN
`
	entries, err := parseCountryCSV(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("parseCountryCSV: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want exactly the one well-formed record", entries)
	}
	if entries[0].value != "CN" {
		t.Errorf("entries[0].value = %q, want CN", entries[0].value)
	}
}

func TestParseCountryCSV_LowercaseCountryIsUppercased(t *testing.T) {
	entries, err := parseCountryCSV(strings.NewReader("1.0.0.0,1.0.0.255,au\n"))
	if err != nil {
		t.Fatalf("parseCountryCSV: %v", err)
	}
	if len(entries) != 1 || entries[0].value != "AU" {
		t.Fatalf("entries = %+v, want one entry with country AU", entries)
	}
}

func TestParseCountryCSV_MixedFamilyRowIsSkipped(t *testing.T) {
	entries, err := parseCountryCSV(strings.NewReader("1.0.0.0,2001:db8::1,AU\n"))
	if err != nil {
		t.Fatalf("parseCountryCSV: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("entries = %+v, want empty (start/end address families must match)", entries)
	}
}

func TestParseASNCSV_RealisticFixture(t *testing.T) {
	// Shaped exactly like a real downloaded origin-asn-ipv4.csv sample
	// (verified against real data before writing this parser): no header,
	// 4 columns, org names quoted when they contain a comma.
	const fixture = `1.0.0.0,1.0.0.255,13335,"Cloudflare, Inc."
1.0.4.0,1.0.7.255,38803,Gtelecom Pty Ltd
1.0.64.0,1.0.127.255,18144,"Enecom,Inc."
`
	entries, err := parseASNCSV(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("parseASNCSV: %v", err)
	}

	want := []rangeEntry[asnInfo]{
		{start: netip.MustParseAddr("1.0.0.0"), end: netip.MustParseAddr("1.0.0.255"), value: asnInfo{asn: 13335, org: "Cloudflare, Inc."}},
		{start: netip.MustParseAddr("1.0.4.0"), end: netip.MustParseAddr("1.0.7.255"), value: asnInfo{asn: 38803, org: "Gtelecom Pty Ltd"}},
		{start: netip.MustParseAddr("1.0.64.0"), end: netip.MustParseAddr("1.0.127.255"), value: asnInfo{asn: 18144, org: "Enecom,Inc."}},
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

func TestParseASNCSV_IPv6Fixture(t *testing.T) {
	// Shaped like a real origin-asn-ipv6.csv sample: same 4-column format,
	// full/expanded IPv6 addresses.
	const fixture = "2001:4860::,2001:4860:ffff:ffff:ffff:ffff:ffff:ffff,15169,GOOGLE\n"
	entries, err := parseASNCSV(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("parseASNCSV: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want exactly 1", entries)
	}
	want := rangeEntry[asnInfo]{
		start: netip.MustParseAddr("2001:4860::"),
		end:   netip.MustParseAddr("2001:4860:ffff:ffff:ffff:ffff:ffff:ffff"),
		value: asnInfo{asn: 15169, org: "GOOGLE"},
	}
	if entries[0] != want {
		t.Errorf("entries[0] = %+v, want %+v", entries[0], want)
	}
}

func TestParseASNCSV_QuotedOrgNameWithCommaIsHandledCorrectly(t *testing.T) {
	const fixture = "1.0.0.0,1.0.0.255,13335,\"Cloudflare, Inc.\"\n"
	entries, err := parseASNCSV(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("parseASNCSV: %v", err)
	}
	if len(entries) != 1 || entries[0].value.org != "Cloudflare, Inc." {
		t.Fatalf("entries = %+v, want one entry with org \"Cloudflare, Inc.\"", entries)
	}
}

func TestParseASNCSV_EmptyInput(t *testing.T) {
	entries, err := parseASNCSV(strings.NewReader(""))
	if err != nil {
		t.Fatalf("parseASNCSV: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("entries = %+v, want empty", entries)
	}
}

func TestParseASNCSV_MalformedLinesAreSkippedNotFatal(t *testing.T) {
	const fixture = `not-an-ip,1.0.0.255,13335,Cloudflare
1.0.0.0,also-not-an-ip,13335,Cloudflare
1.0.0.0,1.0.0.255,not-a-number,Cloudflare
1.0.0.0,1.0.0.255,0,Cloudflare
1.0.0.0,1.0.0.255,13335,
too,few,fields
1.0.4.0,1.0.7.255,38803,Gtelecom Pty Ltd
`
	entries, err := parseASNCSV(strings.NewReader(fixture))
	if err != nil {
		t.Fatalf("parseASNCSV: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want exactly the one well-formed record", entries)
	}
	if entries[0].value.asn != 38803 || entries[0].value.org != "Gtelecom Pty Ltd" {
		t.Errorf("entries[0].value = %+v, want {asn: 38803, org: Gtelecom Pty Ltd}", entries[0].value)
	}
}

func TestParseASNCSV_MixedFamilyRowIsSkipped(t *testing.T) {
	entries, err := parseASNCSV(strings.NewReader("1.0.0.0,2001:db8::1,13335,Cloudflare\n"))
	if err != nil {
		t.Fatalf("parseASNCSV: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("entries = %+v, want empty (start/end address families must match)", entries)
	}
}

func TestParseASNCSV_NegativeASNIsSkipped(t *testing.T) {
	entries, err := parseASNCSV(strings.NewReader("1.0.0.0,1.0.0.255,-1,Cloudflare\n"))
	if err != nil {
		t.Fatalf("parseASNCSV: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("entries = %+v, want empty (ASN must be positive)", entries)
	}
}
