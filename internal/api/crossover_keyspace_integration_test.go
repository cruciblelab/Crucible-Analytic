//go:build integration

// P5: what a privacy.ip_storage change does to the crossover view, and
// what the summary says about it.
//
// Against a real TimescaleDB, because the claim is about what a SQL join
// does with two encodings of an address - which is a property of
// PostgreSQL's comparison of bytea values, not of any Go code here.

package api

import (
	"context"
	"testing"
	"time"
)

// Tokens for the full-mode rows. Sixteen bytes, like privacy.TokenIP
// produces; the content is arbitrary, only the distinctness matters.
var (
	tokenA = []byte("0123456789abcdef")
	tokenB = []byte("fedcba9876543210")
)

// A window that spans a mode change reads two disjoint key spaces, and
// the summary reports the shape rather than only the shrunken number.
//
// # The assertion that matters, and why it needs both halves
//
// The join keys on COALESCE(ip_hash, inet_send(ip)). Two rows for the
// *same address*, one written in each mode, therefore do not join: the
// first key is a 16-byte token, the second an encoded network. Nothing
// in the product asserted this before - the behaviour was reasoned
// about in a comment and never measured, which is the state a claim is
// in just before it turns out to be wrong.
//
// So this test seeds one address twice, in the two modes, and asserts
// both halves:
//
//   - the same address in the same mode *does* join (or this test would
//     pass against a join that never matches anything);
//   - the same address in different modes does *not* (the seam).
func TestStore_RealTimescaleDB_AModeChangeSplitsTheCrossoverKeySpace(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	site := "api-crossover-keyspace"

	store := seedBeacon(t, []beaconSeed{
		// Masked-mode visitor: no token on either side.
		{site: site, visitor: "v1", at: base, ip: "203.0.113.40", path: "/"},
		// Full-mode visitor: the same token the collector wrote.
		{site: site, visitor: "v2", at: base, ip: "203.0.113.41", path: "/",
			ipHash: tokenA},
		// The seam: the beacon heard this address in masked mode and the
		// collector recorded it with a token. One address, two keys.
		{site: site, visitor: "v3", at: base, ip: "203.0.113.42", path: "/"},
	})
	seedSnapshotsFor(t, []seedRow{
		{site: site, ip: "203.0.113.40", at: base, rate: 1, score: 5},
		{site: site, ip: "203.0.113.41", at: base, rate: 1, score: 5, ipHash: tokenA},
		{site: site, ip: "203.0.113.42", at: base, rate: 1, score: 5, ipHash: tokenB},
	})

	got, err := store.CrossoverSummary(context.Background(), site,
		base.Add(-time.Minute), base.Add(time.Hour))
	if err != nil {
		t.Fatalf("CrossoverSummary: %v", err)
	}

	// Three addresses on each side, and only two of the three pairs
	// join. The third is the same visitor by any human reading of the
	// data and the join cannot say so.
	if got.IPsSeen != 3 {
		t.Fatalf("IPsSeen = %d, want 3", got.IPsSeen)
	}
	if got.IPsRanJS != 2 {
		t.Errorf("IPsRanJS = %d, want 2.\n"+
			"Expected: the masked pair joins, the full pair joins, and the pair "+
			"written one in each mode does not - a token and an encoded network "+
			"never compare equal. A 3 here would mean the two encodings match, "+
			"which would make full mode indistinguishable from masked; a 0 would "+
			"mean this join never matches anything and the other half of this "+
			"test is not measuring what it claims.", got.IPsRanJS)
	}
	// And the seam shows up twice more, in the two numbers a reader is
	// most likely to act on: the split address is counted as silent, and
	// its beacon half as one the collector never saw.
	if got.IPsSilent != 1 {
		t.Errorf("IPsSilent = %d, want 1 - the split address", got.IPsSilent)
	}
	if got.BeaconOnlyIPs != 1 {
		t.Errorf("BeaconOnlyIPs = %d, want 1.\n"+
			"This is the number the panel explains as \"the collector is probably "+
			"not in the path\", and here the collector saw every one of these "+
			"addresses. That wrong explanation is what the key-space notice "+
			"replaces.", got.BeaconOnlyIPs)
	}

	// The four counts the page draws its notice from.
	keys := got.KeySpaces
	if keys.CollectorTokenised != 2 || keys.CollectorNetworkOnly != 1 {
		t.Errorf("collector keys = %d tokenised / %d network-only, want 2/1",
			keys.CollectorTokenised, keys.CollectorNetworkOnly)
	}
	if keys.BeaconTokenised != 1 || keys.BeaconNetworkOnly != 2 {
		t.Errorf("beacon keys = %d tokenised / %d network-only, want 1/2",
			keys.BeaconTokenised, keys.BeaconNetworkOnly)
	}
}

// A window with one mode in it reports one key space, and that is the
// negative half.
//
// Without it, every assertion above passes against an endpoint that
// hard-codes "yes, there is a seam" - and a notice that is always shown
// is a notice nobody reads. Both modes are covered, because a count
// that only ever looked at ip_hash IS NULL would pass one of them.
func TestStore_RealTimescaleDB_AConsistentWindowReportsOneKeySpace(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)

	for _, tc := range []struct {
		name  string
		token []byte
	}{
		{"masked throughout", nil},
		{"full throughout", tokenA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			site := "api-crossover-onekey-" + tc.name
			store := seedBeacon(t, []beaconSeed{
				{site: site, visitor: "v1", at: base, ip: "203.0.113.50", path: "/",
					ipHash: tc.token},
			})
			seedSnapshotsFor(t, []seedRow{
				{site: site, ip: "203.0.113.50", at: base, rate: 1, score: 5,
					ipHash: tc.token},
			})

			got, err := store.CrossoverSummary(context.Background(), site,
				base.Add(-time.Minute), base.Add(time.Hour))
			if err != nil {
				t.Fatalf("CrossoverSummary: %v", err)
			}
			// The pair joins in either mode: this is the ordinary
			// deployment, and the whole point is that the mode does not
			// change the answer as long as both writers agree on it.
			if got.IPsSeen != 1 || got.IPsRanJS != 1 {
				t.Errorf("seen/ranJS = %d/%d, want 1/1", got.IPsSeen, got.IPsRanJS)
			}

			keys := got.KeySpaces
			tokenised := tc.token != nil
			if (keys.CollectorTokenised > 0) != tokenised ||
				(keys.CollectorNetworkOnly > 0) == tokenised {
				t.Errorf("collector keys = %d tokenised / %d network-only in %s mode",
					keys.CollectorTokenised, keys.CollectorNetworkOnly, tc.name)
			}
			if (keys.BeaconTokenised > 0) != tokenised ||
				(keys.BeaconNetworkOnly > 0) == tokenised {
				t.Errorf("beacon keys = %d tokenised / %d network-only in %s mode",
					keys.BeaconTokenised, keys.BeaconNetworkOnly, tc.name)
			}
		})
	}
}
