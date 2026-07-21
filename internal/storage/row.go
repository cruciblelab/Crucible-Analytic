// Package storage persists periodic per-IP activity summaries to
// TimescaleDB. It reads from a ratestore.RateStore snapshot, scores each
// entry via the scoring package, and batch-writes the result - the
// "Cache/Skor -> TimescaleDB" step of the collector pipeline.
package storage

import (
	"net/netip"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
	"github.com/cruciblelab/crucible-analytic/internal/scoring"
)

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
}

// BuildRows converts RateStore snapshots into storage-ready rows,
// computing each one's bot score along the way. It's a pure function so
// the scoring/row-shaping logic is unit-testable without a live database.
func BuildRows(snapshots []ratestore.Snapshot, knownBots map[string]string, flushTime time.Time) []Row {
	rows := make([]Row, 0, len(snapshots))
	for _, snap := range snapshots {
		result := scoring.Score(snap.EstimatedRate, snap.JA4, knownBots)
		rows = append(rows, Row{
			Time:            flushTime,
			IP:              snap.IP,
			JA4:             snap.JA4,
			PrevWindowCount: snap.PrevWindowCount,
			CurrWindowCount: snap.CurrWindowCount,
			RequestRate:     snap.EstimatedRate,
			BotScore:        int16(result.Score),
			IsKnownBotJA4:   result.IsKnownBotJA4,
		})
	}
	return rows
}
