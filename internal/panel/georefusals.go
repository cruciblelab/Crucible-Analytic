package panel

import (
	"context"
	"fmt"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
)

// GeoRefusals is what the collector's country and ASN rules have turned
// away since it last started (B3f, catalog #12).
//
// The rules refuse before anything is recorded, so a rule already in
// force leaves no row anywhere - this count, from the collector's own
// heartbeat, is its only trace. Known is false when no collector has
// reported the counter: none has started, or its build is older than the
// counter, and either way "0" would be a guess.
type GeoRefusals struct {
	Known   bool
	Refused int64
	// Since is when that collector started, on the database's clock: the
	// count is since then, not over any fixed window.
	Since time.Time
}

// GeoRefusals reads the collector's count.
func (s *Store) GeoRefusals(ctx context.Context) (GeoRefusals, error) {
	beats, err := heartbeat.Read(ctx, s.pool)
	if err != nil {
		return GeoRefusals{}, fmt.Errorf("panel: reading the collector's refusals: %w", err)
	}
	collector := s.serviceRoles().Collector
	for _, b := range beats {
		if b.Service != collector {
			continue
		}
		n, ok := b.Counters[heartbeat.CounterGeoBlocked]
		if !ok {
			continue
		}
		return GeoRefusals{Known: true, Refused: n, Since: b.StartedAt}, nil
	}
	return GeoRefusals{}, nil
}
