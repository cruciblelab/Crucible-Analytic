// Package storage persists periodic per-IP activity summaries to
// TimescaleDB. It reads from a ratestore.RateStore snapshot, scores each
// entry via the scoring package, optionally enriches it with
// country/ASN via a GeoResolver, and batch-writes the result - the
// "Cache/Skor -> TimescaleDB" step of the collector pipeline.
package storage

import (
	"net/netip"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/asnlookup"
	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
	"github.com/cruciblelab/crucible-analytic/internal/scoring"
)

// GeoResolver resolves an IP to country/ASN info for enriching rows before
// persistence. *asnlookup.Resolver implements this; a narrow interface
// (rather than a direct dependency on *asnlookup.Resolver) so BuildRows
// tests can substitute a fake instead of needing real loaded range
// tables, the same reasoning behind RowWriter and ratestore.RateStore
// being interfaces here too.
type GeoResolver interface {
	Resolve(ip netip.Addr) asnlookup.Result
}

// Row is one flush's summary record for a single IP: a RateStore snapshot
// plus the score derived from it, ready to persist. Field set matches
// schema.sql's traffic_snapshots table.
type Row struct {
	Time            time.Time
	IP              netip.Addr
	JA4             string
	PrevWindowCount int
	CurrWindowCount int
	RequestRate     float64
	BotScore        int16
	IsKnownBotJA4   bool
	// Country, ASN, and ASNName are independently best-effort: "" / 0
	// means that half wasn't resolved (or resolver is nil, i.e.
	// asn_lookup.enabled = false), not that it was checked and came back
	// empty - the same zero-value-means-unresolved convention
	// asnlookup.Result itself uses.
	Country string
	ASN     int
	ASNName string
	// IsKnownBotASN mirrors IsKnownBotJA4, but for ASN: true when ASN
	// matched knownBotASNs. Always false when knownBotASNs is nil/empty
	// (asn_lookup.apply_to_scoring = false, the default) or resolver is
	// nil - see scoring.Score.
	IsKnownBotASN bool
}

// BuildRows converts RateStore snapshots into storage-ready rows,
// resolving each one's country/ASN first (if resolver is non-nil) so that
// already-resolved ASN can feed straight into scoring.Score - no second
// Resolve() call needed for the scoring signal on top of the one storage
// enrichment already makes. It's a pure function so the
// scoring/row-shaping logic is unit-testable without a live database or a
// real resolver. resolver may be nil - when asn_lookup.enabled = false,
// main.go passes no resolver at all, and every row's Country/ASN/ASNName
// simply stay at their zero value rather than this function needing a
// separate on/off flag of its own. knownBotASNs is nil whenever
// asn_lookup.apply_to_scoring = false (the default); see scoring.Score for
// why that alone is enough to make the ASN scoring component a no-op,
// without BuildRows needing its own separate check.
func BuildRows(snapshots []ratestore.Snapshot, knownBots map[string]string, flushTime time.Time, resolver GeoResolver, knownBotASNs map[int]struct{}) []Row {
	rows := make([]Row, 0, len(snapshots))
	for _, snap := range snapshots {
		var geo asnlookup.Result
		if resolver != nil {
			geo = resolver.Resolve(snap.IP)
		}

		result := scoring.Score(snap.EstimatedRate, snap.JA4, knownBots, geo.ASN, knownBotASNs)
		rows = append(rows, Row{
			Time:            flushTime,
			IP:              snap.IP,
			JA4:             snap.JA4,
			PrevWindowCount: snap.PrevWindowCount,
			CurrWindowCount: snap.CurrWindowCount,
			RequestRate:     snap.EstimatedRate,
			BotScore:        int16(result.Score),
			IsKnownBotJA4:   result.IsKnownBotJA4,
			Country:         geo.Country,
			ASN:             geo.ASN,
			ASNName:         geo.ASNName,
			IsKnownBotASN:   result.IsKnownBotASN,
		})
	}
	return rows
}
