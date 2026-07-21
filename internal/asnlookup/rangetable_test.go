package asnlookup

import (
	"net/netip"
	"testing"
)

func TestRangeTable_LookupWithinBounds(t *testing.T) {
	table := newRangeTable([]rangeEntry{
		{start: netip.MustParseAddr("192.0.2.0"), end: netip.MustParseAddr("192.0.2.99"), country: "US"},
		{start: netip.MustParseAddr("198.51.100.0"), end: netip.MustParseAddr("198.51.100.99"), country: "DE"},
	})

	tests := []struct {
		name        string
		ip          string
		wantCountry string
		wantFound   bool
	}{
		{"start boundary of first range", "192.0.2.0", "US", true},
		{"end boundary of first range", "192.0.2.99", "US", true},
		{"middle of first range", "192.0.2.50", "US", true},
		{"start boundary of second range", "198.51.100.0", "DE", true},
		{"end boundary of second range", "198.51.100.99", "DE", true},
		{"gap between ranges", "203.0.113.1", "", false},
		{"before first range", "192.0.1.1", "", false},
		{"after last range", "198.51.101.1", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			country, found := table.lookup(netip.MustParseAddr(tt.ip))
			if found != tt.wantFound || country != tt.wantCountry {
				t.Errorf("lookup(%s) = (%q, %v), want (%q, %v)", tt.ip, country, found, tt.wantCountry, tt.wantFound)
			}
		})
	}
}

func TestRangeTable_IPv6LookupWithinBounds(t *testing.T) {
	table := newRangeTable([]rangeEntry{
		{
			start:   netip.MustParseAddr("2001:db8::"),
			end:     netip.MustParseAddr("2001:db8::ffff"),
			country: "JP",
		},
	})

	if country, found := table.lookup(netip.MustParseAddr("2001:db8::1234")); !found || country != "JP" {
		t.Errorf("lookup(2001:db8::1234) = (%q, %v), want (JP, true)", country, found)
	}
	if _, found := table.lookup(netip.MustParseAddr("2001:db8::1:0")); found {
		t.Error("lookup(2001:db8::1:0) found a result, want none (just past the range's end)")
	}
	if _, found := table.lookup(netip.MustParseAddr("2001:db7::1")); found {
		t.Error("lookup(2001:db7::1) found a result, want none (before the range)")
	}
}

func TestRangeTable_UnsortedInputIsSorted(t *testing.T) {
	table := newRangeTable([]rangeEntry{
		{start: netip.MustParseAddr("198.51.100.0"), end: netip.MustParseAddr("198.51.100.99"), country: "DE"},
		{start: netip.MustParseAddr("192.0.2.0"), end: netip.MustParseAddr("192.0.2.99"), country: "US"},
	})
	if country, found := table.lookup(netip.MustParseAddr("192.0.2.50")); !found || country != "US" {
		t.Errorf("lookup(192.0.2.50) = (%q, %v), want (US, true)", country, found)
	}
	if country, found := table.lookup(netip.MustParseAddr("198.51.100.50")); !found || country != "DE" {
		t.Errorf("lookup(198.51.100.50) = (%q, %v), want (DE, true)", country, found)
	}
}

func TestRangeTable_EmptyTable(t *testing.T) {
	table := newRangeTable(nil)
	if _, found := table.lookup(netip.MustParseAddr("192.0.2.1")); found {
		t.Error("lookup on an empty table found a result, want none")
	}
}

func TestRangeTable_NilTable(t *testing.T) {
	var table *rangeTable
	if _, found := table.lookup(netip.MustParseAddr("192.0.2.1")); found {
		t.Error("lookup on a nil *rangeTable found a result, want none")
	}
}

func TestRangeTable_SingleEntry(t *testing.T) {
	table := newRangeTable([]rangeEntry{
		{start: netip.MustParseAddr("192.0.2.10"), end: netip.MustParseAddr("192.0.2.20"), country: "FR"},
	})
	if country, found := table.lookup(netip.MustParseAddr("192.0.2.15")); !found || country != "FR" {
		t.Errorf("lookup(192.0.2.15) = (%q, %v), want (FR, true)", country, found)
	}
	if _, found := table.lookup(netip.MustParseAddr("192.0.2.9")); found {
		t.Error("lookup(192.0.2.9) found a result, want none (below the only range)")
	}
	if _, found := table.lookup(netip.MustParseAddr("192.0.2.21")); found {
		t.Error("lookup(192.0.2.21) found a result, want none (above the only range)")
	}
}
