package asnlookup

import (
	"net/netip"
	"sort"
)

// rangeEntry is one parsed IP range: [start, end] inclusive, both ends
// the same address family, plus whatever value that range maps to -
// a country code (string) for the country dataset, an (ASN, org name)
// pair for the ASN dataset. Generic so the identical binary-search
// machinery below (rangeTable/newRangeTable/lookup) isn't duplicated
// between the two datasets, which differ only in payload shape, not in
// how ranges are stored or searched.
type rangeEntry[T any] struct {
	start netip.Addr
	end   netip.Addr
	value T
}

// asnInfo is the payload rangeEntry[asnInfo] carries for the ASN dataset.
type asnInfo struct {
	asn int
	org string
}

// rangeTable is an immutable, binary-searchable snapshot of every parsed
// range for ONE dataset and ONE address family, sorted by start.
// Resolver keeps four of these (country x {v4,v6}, ASN x {v4,v6}) rather
// than mixing address families in one table or datasets in one entry
// type: an IPv4 address can never fall inside an IPv6 range or vice
// versa, and the country/ASN CSVs are independently sourced with their
// own, unrelated range boundaries, so merging them would require the
// same overlap-resolution complexity this package has deliberately
// stayed away from (see the package doc comment). "Immutable" matters
// too: Resolver swaps the active *rangeTable with an atomic.Pointer, so a
// lookup in progress during a refresh always sees one complete,
// consistent table - never a half-rebuilt one.
type rangeTable[T any] struct {
	entries []rangeEntry[T]
}

// newRangeTable sorts entries by start address and returns a table ready
// for lookup. The source data shouldn't produce overlapping ranges within
// one dataset and family (each block of the address space is assigned to
// exactly one country, or one ASN), so this doesn't attempt overlap
// resolution - it assumes the input is already disjoint, same as the
// data source guarantees.
func newRangeTable[T any](entries []rangeEntry[T]) *rangeTable[T] {
	sorted := make([]rangeEntry[T], len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start.Compare(sorted[j].start) < 0 })
	return &rangeTable[T]{entries: sorted}
}

// lookup returns the value registered for ip, if ip falls inside any
// known range. O(log n) via binary search for the rightmost range whose
// start is <= ip, then a single bounds check. ip must be the same address
// family as the entries in this table - Resolver.Resolve is responsible
// for routing to the right table by family before calling this.
func (t *rangeTable[T]) lookup(ip netip.Addr) (value T, found bool) {
	if t == nil || len(t.entries) == 0 {
		var zero T
		return zero, false
	}
	i := sort.Search(len(t.entries), func(i int) bool { return t.entries[i].start.Compare(ip) > 0 }) - 1
	if i < 0 {
		var zero T
		return zero, false
	}
	e := t.entries[i]
	if ip.Compare(e.start) < 0 || ip.Compare(e.end) > 0 {
		var zero T
		return zero, false
	}
	return e.value, true
}
