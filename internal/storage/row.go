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
	"github.com/cruciblelab/crucible-analytic/internal/privacy"
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
	Time time.Time
	// SiteID is which site this row belongs to (config.site_id) - stamped
	// identically on every row a given collector writes, so one database
	// can hold several sites' data.
	SiteID string
	IP     netip.Addr
	// IPHash is the keyed token written in full mode, and nil in
	// every other mode. Exactly one of IP and IPHash carries a value:
	// a row with both would be storing the address it set out not to.
	IPHash          []byte
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

// RowOptions are the deployment-wide inputs BuildRows needs, as opposed
// to the per-snapshot ones.
//
// A struct rather than more positional parameters. The list had reached
// six, several of them nil-able and two of them maps, and the next
// caller to swap two of those by accident would have got a working
// program that answered a different question - which is the failure mode
// this project has already paid for once.
type RowOptions struct {
	// SiteID is stamped unchanged onto every row. It identifies the
	// collector's own site, not anything derived per-snapshot.
	SiteID string
	// KnownBots maps a JA4 fingerprint to a bot name, for scoring.
	KnownBots map[string]string
	// KnownBotASNs feeds scoring.Score's ASN component. Nil whenever
	// asn_lookup.apply_to_scoring = false (the default); see
	// scoring.Score for why nil alone makes it a no-op.
	KnownBotASNs map[int]struct{}
	// Resolver enriches each row with country/ASN. Nil when
	// asn_lookup.enabled = false, in which case Country/ASN/ASNName stay
	// at their zero values rather than this needing an on/off flag.
	Resolver GeoResolver
	// IPMode decides how much of each address is stored.
	//
	// The zero value masks. That is deliberate: a caller that forgets
	// this field gets the privacy-preserving behaviour rather than the
	// other one, and the mistake shows up as coarser data instead of as
	// personal data on disk.
	IPMode privacy.IPMode
	// IPHashKey keys the token stored in full mode. Ignored otherwise.
	IPHashKey []byte
}

// BuildRows converts RateStore snapshots into storage-ready rows.
//
// Country and ASN are resolved first, so the already-resolved ASN feeds
// straight into scoring.Score without a second lookup. The address is
// masked *after* that, and that ordering is the point: resolving from a
// masked address would return the network's registration rather than the
// visitor's, and nothing in the output would say so - the countries
// would just quietly start being different.
//
// It is a pure function, so the scoring and row-shaping logic is
// testable without a database or a real resolver.
func BuildRows(snapshots []ratestore.Snapshot, flushTime time.Time, opts RowOptions) []Row {
	rows := make([]Row, 0, len(snapshots))
	for _, snap := range snapshots {
		var geo asnlookup.Result
		if opts.Resolver != nil {
			geo = opts.Resolver.Resolve(snap.IP)
		}

		result := scoring.Score(snap.EstimatedRate, snap.JA4, opts.KnownBots, geo.ASN, opts.KnownBotASNs)
		rows = append(rows, Row{
			Time:   flushTime,
			SiteID: opts.SiteID,
			// Last use of the whole address. What reaches the row - and
			// therefore the disk - is the masked network, plus a keyed
			// token in full mode. The raw address goes nowhere.
			IP:              maskedFor(snap.IP, opts),
			IPHash:          tokenFor(snap.IP, opts),
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

// maskedFor and tokenFor decide what the row carries.
//
// The address column always holds the masked network, in both modes,
// because no mode stores a raw address. The token column holds
// whole-address precision, and only in full mode - it is what lets that
// mode tell two visitors inside one /24 apart without the address being
// on disk to tell them apart with.
func maskedFor(ip netip.Addr, opts RowOptions) netip.Addr {
	return privacy.MaskIP(ip, opts.IPMode)
}

func tokenFor(ip netip.Addr, opts RowOptions) []byte {
	if !opts.IPMode.Tokenises() {
		return nil
	}
	return privacy.TokenIP(ip, opts.IPHashKey)
}

// storedIP and storedIPHash render the row's two columns for the COPY.
//
// Methods rather than raw fields at the call site because both have to
// become a real SQL NULL when unset, and an invalid netip.Addr is not
// one - handing it straight to pgx would either error or write something
// nobody intended. Doing the conversion here means the writer cannot
// forget it.
func (r Row) storedIP() any {
	if !r.IP.IsValid() {
		return nil
	}
	return r.IP
}

func (r Row) storedIPHash() any {
	if len(r.IPHash) == 0 {
		return nil
	}
	return r.IPHash
}
