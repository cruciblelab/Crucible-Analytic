package asnlookup

import (
	"net/netip"
	"testing"
)

func TestRangeTable_LookupWithinBounds(t *testing.T) {
	table := newRangeTable([]rangeEntry{
		{start: 100, end: 199, country: "US"},
		{start: 300, end: 399, country: "DE"},
	})

	tests := []struct {
		name        string
		ip          uint32
		wantCountry string
		wantFound   bool
	}{
		{"start boundary of first range", 100, "US", true},
		{"end boundary of first range", 199, "US", true},
		{"middle of first range", 150, "US", true},
		{"start boundary of second range", 300, "DE", true},
		{"end boundary of second range", 399, "DE", true},
		{"gap between ranges", 250, "", false},
		{"before first range", 50, "", false},
		{"after last range", 500, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			country, found := table.lookup(tt.ip)
			if found != tt.wantFound || country != tt.wantCountry {
				t.Errorf("lookup(%d) = (%q, %v), want (%q, %v)", tt.ip, country, found, tt.wantCountry, tt.wantFound)
			}
		})
	}
}

func TestRangeTable_UnsortedInputIsSorted(t *testing.T) {
	table := newRangeTable([]rangeEntry{
		{start: 300, end: 399, country: "DE"},
		{start: 100, end: 199, country: "US"},
	})
	if country, found := table.lookup(150); !found || country != "US" {
		t.Errorf("lookup(150) = (%q, %v), want (US, true)", country, found)
	}
	if country, found := table.lookup(350); !found || country != "DE" {
		t.Errorf("lookup(350) = (%q, %v), want (DE, true)", country, found)
	}
}

func TestRangeTable_EmptyTable(t *testing.T) {
	table := newRangeTable(nil)
	if _, found := table.lookup(100); found {
		t.Error("lookup on an empty table found a result, want none")
	}
}

func TestRangeTable_NilTable(t *testing.T) {
	var table *rangeTable
	if _, found := table.lookup(100); found {
		t.Error("lookup on a nil *rangeTable found a result, want none")
	}
}

func TestRangeTable_SingleEntry(t *testing.T) {
	table := newRangeTable([]rangeEntry{{start: 10, end: 20, country: "FR"}})
	if country, found := table.lookup(15); !found || country != "FR" {
		t.Errorf("lookup(15) = (%q, %v), want (FR, true)", country, found)
	}
	if _, found := table.lookup(9); found {
		t.Error("lookup(9) found a result, want none (below the only range)")
	}
	if _, found := table.lookup(21); found {
		t.Error("lookup(21) found a result, want none (above the only range)")
	}
}

func TestIPv4ToUint32(t *testing.T) {
	tests := []struct {
		addr   string
		want   uint32
		wantOK bool
	}{
		{"0.0.0.1", 1, true},
		{"12.0.0.0", 0x0C000000, true},
		{"255.255.255.255", 0xFFFFFFFF, true},
		{"::ffff:12.0.0.0", 0x0C000000, true}, // IPv4-in-IPv6 form
		{"2600::1", 0, false},                 // real IPv6, not representable as IPv4
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			addr := netip.MustParseAddr(tt.addr)
			got, ok := ipv4ToUint32(addr)
			if ok != tt.wantOK || (ok && got != tt.want) {
				t.Errorf("ipv4ToUint32(%s) = (%d, %v), want (%d, %v)", tt.addr, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
