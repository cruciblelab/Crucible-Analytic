package storage

import (
	"net/netip"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/asnlookup"
	"github.com/cruciblelab/crucible-analytic/internal/privacy"
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

	// Full mode with a key: the stored address is masked either way, and
	// what full mode adds is the token beside it.
	rows := BuildRows(snaps, flushTime, RowOptions{
		SiteID: "test-site", KnownBots: knownBots,
		IPMode: privacy.IPFull, IPHashKey: testHashKey,
	})
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}

	r0 := rows[0]
	if r0.IP != netip.MustParseAddr("203.0.113.0") || r0.JA4 != "bad-ja4" || r0.PrevWindowCount != 5 || r0.CurrWindowCount != 3 || r0.RequestRate != 8 {
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
	rows := BuildRows(nil, time.Now(), RowOptions{SiteID: "test-site"})
	if len(rows) != 0 {
		t.Errorf("len(rows) = %d, want 0 for no snapshots", len(rows))
	}
}

func TestBuildRows_StampsSiteIDOnEveryRow(t *testing.T) {
	flushTime := time.Now()
	snaps := []ratestore.Snapshot{
		{IP: netip.MustParseAddr("203.0.113.1"), LastSeen: flushTime},
		{IP: netip.MustParseAddr("203.0.113.2"), LastSeen: flushTime},
	}

	rows := BuildRows(snaps, flushTime, RowOptions{SiteID: "ahmetteknoloji"})
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	for i, r := range rows {
		if r.SiteID != "ahmetteknoloji" {
			t.Errorf("row %d: SiteID = %q, want ahmetteknoloji (every row carries the collector's own site)", i, r.SiteID)
		}
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

	rows := BuildRows(snaps, flushTime, RowOptions{
		SiteID: "test-site", Resolver: resolver, IPMode: privacy.IPFull, IPHashKey: testHashKey,
	})
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

	rows := BuildRows(snaps, flushTime, RowOptions{
		SiteID: "test-site", Resolver: resolver, IPMode: privacy.IPFull, IPHashKey: testHashKey,
	})
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

	rows := BuildRows(snaps, flushTime, RowOptions{
		SiteID: "test-site", Resolver: resolver, KnownBotASNs: knownBotASNs,
		IPMode: privacy.IPFull, IPHashKey: testHashKey,
	})
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

	rows := BuildRows(snaps, flushTime, RowOptions{
		SiteID: "test-site", Resolver: resolver, IPMode: privacy.IPFull, IPHashKey: testHashKey,
	})
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	r0 := rows[0]
	if r0.IsKnownBotASN || r0.BotScore != 0 {
		t.Errorf("row 0: IsKnownBotASN = %v, BotScore = %d, want false/0 with nil knownBotASNs", r0.IsKnownBotASN, r0.BotScore)
	}
}

// --- IP storage mode (A7) ---

// testHashKey keys the token in the full-mode cases below.
var testHashKey = []byte("otuz-iki-baytlik-test-anahtari!!")

// The zero value masks. A caller that adds a field to RowOptions and
// forgets to set the mode gets coarser data, never personal data on
// disk.
func TestBuildRows_MasksByDefault(t *testing.T) {
	flushTime := time.Now()
	snaps := []ratestore.Snapshot{
		{IP: netip.MustParseAddr("185.23.45.178"), LastSeen: flushTime},
	}

	rows := BuildRows(snaps, flushTime, RowOptions{SiteID: "test-site"})
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].IP != netip.MustParseAddr("185.23.45.0") {
		t.Errorf("IP = %v, want the masked 185.23.45.0; an unset mode stored the whole address", rows[0].IP)
	}
}

// Country and ASN are resolved from the whole address, then the address
// is masked. Resolving from a masked address would answer about the
// network's registration rather than the visitor's, and the countries
// would quietly start being different with nothing to say why.
func TestBuildRows_ResolvesGeographyBeforeMasking(t *testing.T) {
	flushTime := time.Now()
	whole := netip.MustParseAddr("8.8.8.8")
	snaps := []ratestore.Snapshot{{IP: whole, LastSeen: flushTime}}

	var asked []netip.Addr
	resolver := recordingResolver{
		seen: &asked,
		result: asnlookup.Result{
			IP: whole, Country: "US", ASN: 15169, ASNName: "GOOGLE", Found: true,
		},
	}

	rows := BuildRows(snaps, flushTime, RowOptions{
		SiteID: "test-site", Resolver: resolver, IPMode: privacy.IPMasked,
	})
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}

	if len(asked) != 1 || asked[0] != whole {
		t.Errorf("the resolver was asked about %v, want the whole address %v", asked, whole)
	}
	if rows[0].IP != netip.MustParseAddr("8.8.8.0") {
		t.Errorf("stored IP = %v, want the masked 8.8.8.0", rows[0].IP)
	}
	if rows[0].Country != "US" || rows[0].ASN != 15169 {
		t.Errorf("masking cost the geography: %q/%d", rows[0].Country, rows[0].ASN)
	}
}

// recordingResolver notes which addresses it was asked about, which is
// how the ordering above is asserted rather than assumed.
type recordingResolver struct {
	result asnlookup.Result
	seen   *[]netip.Addr
}

func (r recordingResolver) Resolve(ip netip.Addr) asnlookup.Result {
	*r.seen = append(*r.seen, ip)
	return r.result
}

// No mode writes a raw address. Full mode differs from masked by the
// token beside the address, never by the address itself - which is the
// rule the whole design turns on and the one somebody would break by
// "restoring" full mode to what its name suggests.
func TestBuildRows_NoModeStoresARawAddress(t *testing.T) {
	flushTime := time.Now()
	whole := netip.MustParseAddr("185.23.45.178")
	snaps := []ratestore.Snapshot{{IP: whole, LastSeen: flushTime}}

	for _, tc := range []struct {
		mode      privacy.IPMode
		key       []byte
		wantToken bool
	}{
		{privacy.IPMasked, nil, false},
		{privacy.IPFull, testHashKey, true},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			rows := BuildRows(snaps, flushTime, RowOptions{
				SiteID: "test-site", IPMode: tc.mode, IPHashKey: tc.key,
			})
			if rows[0].IP == whole {
				t.Errorf("%s stored the raw address", tc.mode)
			}
			if rows[0].IP != netip.MustParseAddr("185.23.45.0") {
				t.Errorf("%s stored %v, want the masked network", tc.mode, rows[0].IP)
			}
			if got := len(rows[0].IPHash) > 0; got != tc.wantToken {
				t.Errorf("%s wrote a token: %v, want %v", tc.mode, got, tc.wantToken)
			}
		})
	}
}
