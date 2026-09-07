package panel

import (
	"context"
	"fmt"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
	"github.com/cruciblelab/crucible-analytic/internal/profile"
)

// What the running services say they are gathering.
//
// # Why the panel asks the services rather than the configuration
//
// Because it cannot read the configuration and must not gain the
// ability: collector.toml holds that service's database password, and
// the panel's whole design is that it never sees one. So the honest
// source is the service's own declaration, which arrives in every
// heartbeat.
//
// It is also the more truthful source. A file edited without a restart
// describes nothing yet; the running process describes what is actually
// being written to the tables the panel is drawing.
//
// # Why the range tables are not the source
//
// They look like one - ip_country_ranges is empty on a deployment with
// the lookup off - and they are not. The resolver loads its ranges from
// downloaded CSVs into memory and writes those tables as a copy
// afterwards, and a deployment running the beacon alongside a collector
// sets SkipRangePersistence so the copy is never written at all. An
// empty table there means "nobody wrote it", which is a different
// sentence.

// Collecting is the IP-intelligence level each data source reports.
//
// The zero value of a field means no service of that kind has said, and
// profile.Level.Known is how a caller tells that apart from a service
// that reports collecting nothing.
type Collecting struct {
	// Collector is what fills traffic_snapshots.
	Collector profile.Level
	// Beacon is what fills beacon_events, and it is separate because the
	// two are separately configured. A deployment can run the collector
	// at Tam Crucible and the beacon with the lookup off.
	Beacon profile.Level
}

// Heartbeat service names, as the three services write them.
const (
	serviceCollector = "collector"
	serviceBeacon    = "beacon"
)

// Collecting reads what each source is gathering.
//
// Through heartbeat.Read rather than its own query, because that
// function already carries the one thing a second query would get wrong:
// the profile column arrived with schema 8, and a panel newer than the
// database has to ask before it selects it.
func (s *Store) Collecting(ctx context.Context) (Collecting, error) {
	beats, err := heartbeat.Read(ctx, s.pool)
	if err != nil {
		return Collecting{}, fmt.Errorf("panel: reading what the services collect: %w", err)
	}
	var out Collecting
	for _, b := range beats {
		level, ok := levelOf(b.Profile)
		if !ok {
			continue
		}
		switch b.Service {
		case serviceCollector:
			out.Collector = level
		case serviceBeacon:
			out.Beacon = level
		}
	}
	return out, nil
}

// levelOf turns a reported profile id into a level.
//
// False for an id this build does not know, which is a service newer
// than this panel. Guessing at it would be the one mistake this whole
// file exists to avoid: a panel that decided an unrecognised profile
// collects nothing would tell a customer their data is missing on the
// day they upgraded the collector first.
// The empty id is not special-cased and does not need to be: no profile
// has an empty id, so the lookup below answers it. That guard was here,
// a mutation deleted it, and nothing went red - which was the truth
// rather than a missing test.
func levelOf(id string) (profile.Level, bool) {
	p, ok := profile.ByID(id)
	if !ok {
		return "", false
	}
	return p.Level, true
}
