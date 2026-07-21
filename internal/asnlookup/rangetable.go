package asnlookup

import (
	"encoding/binary"
	"net/netip"
	"sort"
)

// rangeEntry is one IPv4 allocation record: [start, end] inclusive,
// addresses as plain uint32, plus the country it's registered to.
type rangeEntry struct {
	start   uint32
	end     uint32
	country string
}

// rangeTable is an immutable, binary-searchable snapshot of every parsed
// IPv4 range, sorted by start. "Immutable" matters: Resolver swaps the
// active *rangeTable with an atomic.Pointer, so a lookup in progress
// during a refresh always sees one complete, consistent table - never a
// half-rebuilt one.
type rangeTable struct {
	entries []rangeEntry
}

// newRangeTable sorts entries by start address and returns a table ready
// for lookup. Real RIR delegated-stats data shouldn't produce overlapping
// ranges (each block of the address space is delegated to exactly one
// RIR), so this doesn't attempt overlap resolution - it assumes the input
// is already disjoint, same as the data source guarantees.
func newRangeTable(entries []rangeEntry) *rangeTable {
	sorted := make([]rangeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start < sorted[j].start })
	return &rangeTable{entries: sorted}
}

// lookup returns the country registered for ip, if ip falls inside any
// known range. O(log n) via binary search for the rightmost range whose
// start is <= ip, then a single bounds check.
func (t *rangeTable) lookup(ip uint32) (country string, found bool) {
	if t == nil || len(t.entries) == 0 {
		return "", false
	}
	i := sort.Search(len(t.entries), func(i int) bool { return t.entries[i].start > ip }) - 1
	if i < 0 {
		return "", false
	}
	e := t.entries[i]
	if ip < e.start || ip > e.end {
		return "", false
	}
	return e.country, true
}

// ipv4ToUint32 converts an IPv4 (or IPv4-in-IPv6) address to its plain
// uint32 form. IPv6 addresses (that aren't an IPv4-mapped form) return
// false - this phase only covers IPv4, see the package doc comment.
func ipv4ToUint32(ip netip.Addr) (uint32, bool) {
	ip = ip.Unmap()
	if !ip.Is4() {
		return 0, false
	}
	b := ip.As4()
	return binary.BigEndian.Uint32(b[:]), true
}
