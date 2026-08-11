package limiter

import "testing"

func TestNewGeoBlocklist_EmptyInputsReturnNil(t *testing.T) {
	if b := NewGeoBlocklist(nil, nil); b != nil {
		t.Errorf("NewGeoBlocklist(nil, nil) = %v, want nil", b)
	}
	if b := NewGeoBlocklist([]string{}, []int{}); b != nil {
		t.Errorf("NewGeoBlocklist(empty, empty) = %v, want nil", b)
	}
}

func TestGeoBlocklist_CountryMatch(t *testing.T) {
	b := NewGeoBlocklist([]string{"CN", "RU"}, nil)
	if !b.Blocked("CN", 0) {
		t.Error("Blocked(CN, 0) = false, want true")
	}
	if !b.Blocked("RU", 12345) {
		t.Error("Blocked(RU, 12345) = false, want true (country match alone is enough)")
	}
	if b.Blocked("US", 0) {
		t.Error("Blocked(US, 0) = true, want false")
	}
}

func TestGeoBlocklist_CountryMatchIsCaseInsensitive(t *testing.T) {
	b := NewGeoBlocklist([]string{"cn"}, nil)
	if !b.Blocked("CN", 0) {
		t.Error("Blocked(CN, 0) = false, want true (blocklist entry was lowercase)")
	}

	b2 := NewGeoBlocklist([]string{"CN"}, nil)
	if !b2.Blocked("cn", 0) {
		t.Error("Blocked(cn, 0) = false, want true (queried value was lowercase)")
	}
}

func TestGeoBlocklist_ASNMatch(t *testing.T) {
	b := NewGeoBlocklist(nil, []int{64512, 64513})
	if !b.Blocked("", 64512) {
		t.Error("Blocked(\"\", 64512) = false, want true")
	}
	if !b.Blocked("US", 64513) {
		t.Error("Blocked(US, 64513) = false, want true (ASN match alone is enough)")
	}
	if b.Blocked("US", 64514) {
		t.Error("Blocked(US, 64514) = true, want false")
	}
}

func TestGeoBlocklist_UnresolvedValuesNeverMatch(t *testing.T) {
	// A blocklist that would match "" or 0 if they were treated as real
	// values would be a bug: "" / 0 mean "not resolved" (asnlookup's own
	// zero-value convention), not "matched an empty rule".
	b := NewGeoBlocklist([]string{"CN"}, []int{64512})
	if b.Blocked("", 0) {
		t.Error("Blocked(\"\", 0) = true, want false - unresolved country/ASN must never match")
	}
}

func TestGeoBlocklist_NilBlocklistNeverBlocks(t *testing.T) {
	var b *GeoBlocklist
	if b.Blocked("CN", 64512) {
		t.Error("nil GeoBlocklist blocked a lookup, want it to always report false")
	}
}
