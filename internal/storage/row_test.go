package storage

import (
	"net/netip"
	"testing"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/ratestore"
)

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

	rows := BuildRows(snaps, knownBots, flushTime)
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

	r1 := rows[1]
	if r1.IsKnownBotJA4 {
		t.Error("row 1: IsKnownBotJA4 = true, want false (empty JA4 must never match)")
	}
}

func TestBuildRows_Empty(t *testing.T) {
	rows := BuildRows(nil, nil, time.Now())
	if len(rows) != 0 {
		t.Errorf("len(rows) = %d, want 0 for no snapshots", len(rows))
	}
}
