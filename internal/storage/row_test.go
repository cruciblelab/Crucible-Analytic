package storage

import (
	"net/netip"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/asnlookup"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

// fakeResolver returns a canned asnlookup.Result for every IP, regardless
// of what's asked - enough to prove BuildRows/Flusher wire a GeoResolver
// through correctly without needing a real loaded range table.
type fakeResolver struct {
	result asnlookup.Result
}

func (f fakeResolver) Resolve(ip netip.Addr) asnlookup.Result {
	return f.result
}

func TestBuildRows(t *testing.T) {
	flushTime := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	knownBots := map[string]string{"bad-ja4": "test-bot"}

	snaps := []ratestore.Snapshot{
		{
			IP:       netip.MustParseAddr("203.0.113.1"),
			JA4:      "bad-ja4",
			LastSeen: flushTime,
			WindowStats: ratestore.WindowStats{
				PrevWindowCount: 5,
				CurrWindowCount: 3,
				EstimatedRate:   8,
			},
		},
		{
			IP:       netip.MustParseAddr("203.0.113.2"),
			JA4:      "",
			LastSeen: flushTime,
			WindowStats: ratestore.WindowStats{
				PrevWindowCount: 0,
				CurrWindowCount: 1,
				EstimatedRate:   0.1,
			},
		},
	}

	rows := BuildRows(snaps, knownBots, flushTime, nil, nil)
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}

	r0 := rows[0]
	if r0.IP != snaps[0].IP || r0.JA4 != "bad-ja4" || r0.PrevWindowCount != 5 || r0.CurrWindowCount != 3 || r0.RequestRate != 8 {
		t.Errorf("row 0 = %+v, mismatched fields copied from snapshot", r0)
	}
	if !r0.IsKnownBotJA4 {
		t.Error("row 0: IsKnownBotJA4 = false, want true (JA4 matches knownBots)")
	}
	if r0.BotScore <= 0 {
		t.Errorf("row 0: BotScore = %d, want > 0 for a known-bot JA4 with nonzero rate", r0.BotScore)
	}
	if !r0.Time.Equal(flushTime) {
		t.Errorf("row 0: Time = %v, want %v", r0.Time, flushTime)
	}
	if r0.Country != "" || r0.ASN != 0 || r0.ASNName != "" {
		t.Errorf("row 0: Country/ASN/ASNName = %q/%d/%q, want all zero value with a nil resolver", r0.Country, r0.ASN, r0.ASNName)
	}

	r1 := rows[1]
	if r1.IsKnownBotJA4 {
		t.Error("row 1: IsKnownBotJA4 = true, want false (empty JA4 must never match)")
	}
}

func TestBuildRows_Empty(t *testing.T) {
	rows := BuildRows(nil, nil, time.Now(), nil, nil)
	if len(rows) != 0 {
		t.Errorf("len(rows) = %d, want 0 for no snapshots", len(rows))
	}
}

func TestBuildRows_EnrichesWithResolver(t *testing.T) {
	flushTime := time.Now()
	snaps := []ratestore.Snapshot{
		{IP: netip.MustParseAddr("8.8.8.8"), JA4: "", LastSeen: flushTime},
	}
	resolver := fakeResolver{result: asnlookup.Result{
		IP: netip.MustParseAddr("8.8.8.8"), Country: "US", ASN: 15169, ASNName: "GOOGLE", Found: true,
	}}

	rows := BuildRows(snaps, nil, flushTime, resolver, nil)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	r0 := rows[0]
	if r0.Country != "US" || r0.ASN != 15169 || r0.ASNName != "GOOGLE" {
		t.Errorf("row 0: Country/ASN/ASNName = %q/%d/%q, want US/15169/GOOGLE", r0.Country, r0.ASN, r0.ASNName)
	}
}

func TestBuildRows_ResolverNotFoundLeavesZeroValue(t *testing.T) {
	flushTime := time.Now()
	snaps := []ratestore.Snapshot{
		{IP: netip.MustParseAddr("203.0.113.1"), JA4: "", LastSeen: flushTime},
	}
	// A resolver that ran but found nothing for this IP - distinct from a
	// nil resolver (never even consulted); both leave the same zero
	// value, but this exercises the actual Resolve call path.
	resolver := fakeResolver{result: asnlookup.Result{IP: netip.MustParseAddr("203.0.113.1"), Found: false}}

	rows := BuildRows(snaps, nil, flushTime, resolver, nil)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	r0 := rows[0]
	if r0.Country != "" || r0.ASN != 0 || r0.ASNName != "" {
		t.Errorf("row 0: Country/ASN/ASNName = %q/%d/%q, want all zero value when the resolver found nothing", r0.Country, r0.ASN, r0.ASNName)
	}
}

func TestBuildRows_KnownBotASNAddsScoreBonusAndFlag(t *testing.T) {
	flushTime := time.Now()
	snaps := []ratestore.Snapshot{
		{IP: netip.MustParseAddr("203.0.113.1"), JA4: "", LastSeen: flushTime},
	}
	resolver := fakeResolver{result: asnlookup.Result{ASN: 64512, Found: true}}
	knownBotASNs := map[int]struct{}{64512: {}}

	rows := BuildRows(snaps, nil, flushTime, resolver, knownBotASNs)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	r0 := rows[0]
	if !r0.IsKnownBotASN {
		t.Error("row 0: IsKnownBotASN = false, want true (ASN matches knownBotASNs)")
	}
	if r0.BotScore <= 0 {
		t.Errorf("row 0: BotScore = %d, want > 0 for a known-bot ASN", r0.BotScore)
	}
}

func TestBuildRows_NilKnownBotASNsNoASNBonusEvenWithResolver(t *testing.T) {
	// The shape asn_lookup.apply_to_scoring = false (the default) leaves
	// main.go in: a resolver is still wired for storage enrichment, but
	// KnownBotASNs stays nil, so ASN must never affect BotScore.
	flushTime := time.Now()
	snaps := []ratestore.Snapshot{
		{IP: netip.MustParseAddr("203.0.113.1"), JA4: "", LastSeen: flushTime},
	}
	resolver := fakeResolver{result: asnlookup.Result{ASN: 64512, Found: true}}

	rows := BuildRows(snaps, nil, flushTime, resolver, nil)
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	r0 := rows[0]
	if r0.IsKnownBotASN || r0.BotScore != 0 {
		t.Errorf("row 0: IsKnownBotASN = %v, BotScore = %d, want false/0 with nil knownBotASNs", r0.IsKnownBotASN, r0.BotScore)
	}
}
