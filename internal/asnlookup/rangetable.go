package asnlookup

import (
	"net/netip"
	"sort"
)

// rangeEntry is one parsed IP range: [start, end] inclusive, both ends
// the same address family, plus the country it's registered to.
type rangeEntry struct {
	start   netip.Addr
	end     netip.Addr
	country string
}

// rangeTable is an immutable, binary-searchable snapshot of every parsed
// range for ONE address family, sorted by start. Resolver keeps one of
// these per family (see table4/table6) rather than mixing both in a
// single table: an IPv4 address can never fall inside an IPv6 range or
// vice versa, and the source CSVs are already split into separate ipv4/
// ipv6 files, so there's no meaningful cross-family ordering to define in
// the first place. "Immutable" matters too: Resolver swaps the active
// *rangeTable with an atomic.Pointer, so a lookup in progress during a
// refresh always sees one complete, consistent table - never a
// half-rebuilt one.
type rangeTable struct {
	entries []rangeEntry
}

// newRangeTable sorts entries by start address and returns a table ready
// for lookup. The source data shouldn't produce overlapping ranges within
// one family (each block of the address space is assigned to exactly one
// country), so this doesn't attempt overlap resolution - it assumes the
// input is already disjoint, same as the data source guarantees.
func newRangeTable(entries []rangeEntry) *rangeTable {
	sorted := make([]rangeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start.Compare(sorted[j].start) < 0 })
	return &rangeTable{entries: sorted}
}

// lookup returns the country registered for ip, if ip falls inside any
// known range. O(log n) via binary search for the rightmost range whose
// start is <= ip, then a single bounds check. ip must be the same address
// family as the entries in this table - Resolver.Resolve is responsible
// for routing to the right table by family before calling this.
func (t *rangeTable) lookup(ip netip.Addr) (country string, found bool) {
	if t == nil || len(t.entries) == 0 {
		return "", false
	}
	i := sort.Search(len(t.entries), func(i int) bool { return t.entries[i].start.Compare(ip) > 0 }) - 1
	if i < 0 {
		return "", false
	}
	e := t.entries[i]
	if ip.Compare(e.start) < 0 || ip.Compare(e.end) > 0 {
		return "", false
	}
	return e.country, true
}
