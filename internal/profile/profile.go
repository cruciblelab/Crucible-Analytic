// Package profile turns "how much of this machine may the collector
// have?" into a named choice, and refuses the ones that would not fit.
//
// # What a profile is not
//
// It is not a mode. Nothing in this system asks "are we in Dengeli?" and
// branches on the answer, because that would be two sources of truth for
// one behaviour: the profile and the settings it was supposed to
// represent, free to disagree the moment somebody edits one of them.
//
// A profile is a named set of values, and the name is *derived* from the
// values rather than stored beside them. Match does that derivation.
// The consequence is the useful part: an installation whose values were
// hand-edited afterwards reports itself as Özel, or as the profile it
// came from with a note, without anybody having to remember to update a
// label.
//
// # What the numbers here are
//
// Measured, on the real datasets, and written down with their date and
// their spread rather than rounded into confidence:
//
//   - The IP-intelligence tables hold 59.1 MB with the country dataset
//     alone and 136.1 MB with the ASN half as well. Those two figures
//     came out identical to a tenth of a megabyte across five runs.
//   - The refresh peak is 1.5 to 2 times the held figure - 102-111 MB
//     and 192-216 MB across the same runs - because each parser builds a
//     slice of every range in a file before any table is swapped in. The
//     peak moves with GC timing, so it is a ratio here and not a number.
//   - The rate store costs about 150 bytes per tracked address, measured
//     at 142, 132 and 164 bytes across ten thousand, a hundred thousand
//     and five hundred thousand addresses.
//
// # Why the rate store is in the budget at all
//
// Because it can be the larger half, and because its size is a
// consequence of two settings rather than of traffic. The map holds one
// entry per address seen inside the idle TTL - but only for *admitted*
// connections: internal/proxy checks the geo-blocklist, then the
// limiter, and reaches RecordRequest only for a connection that passed
// both. A rejected or degraded connection is never recorded.
//
// That was worth checking rather than assuming, and the answer is the
// difference between a bounded map and a memory-exhaustion vector: an
// attacker holding an IPv6 /64 can present billions of distinct source
// addresses, and an unbounded map keyed by them, in the process that
// stands in front of the customer's site, would be a way to kill the
// site with well-formed traffic. It is bounded, by construction, at
// requests-per-second times the TTL.
package profile

import (
	"fmt"

	"github.com/cruciblelab/crucible-analytic/internal/memlimit"
)

// Level is how much IP intelligence a deployment loads. It is the axis
// that decides the memory a profile costs.
//
// Deliberately the only such axis, which is a correction to the plan
// rather than a shortcut. A2's table also listed JA4 fingerprinting and
// the beacon, and neither belongs: fingerprinting cannot be turned off
// (reading the ClientHello is what passthrough mode is *for*) and costs
// nothing resident, being a per-connection parse; the beacon is a
// separate process, so running it or not is a deployment decision and
// not a collector setting. Building switches for both, to make a table
// true that was written before anything was measured, would have added
// two controls that save no memory.
type Level string

const (
	// LevelOff loads neither dataset. Country and ASN are both absent
	// from stored rows and from the panel.
	LevelOff Level = "kapali"
	// LevelCountry loads the country dataset only - see
	// asnlookup.Resolver.CountryOnly.
	LevelCountry Level = "ulke"
	// LevelFull loads both.
	LevelFull Level = "tam"
)

// Profile is one named choice.
type Profile struct {
	ID    string
	Label string
	Level Level
	// Held is what the IP-intelligence tables occupy once loaded.
	Held uint64
	// Peak is what a refresh reaches while parsing.
	Peak uint64
	// Floor is the container size at which a process doing nothing but
	// this profile's refresh survives reliably - measured by running it,
	// not derived from Peak.
	//
	// Peak turned out to be the wrong basis for a budget. It is sampled,
	// it moves with GC timing, and a process does not die when its heap
	// touches a number: it dies when the kernel cannot satisfy an
	// allocation, which depends on what the runtime has already reserved
	// and on how hard the collector is willing to work before giving up.
	// The only faithful answer is to run it under a limit and see.
	Floor uint64
	// Why is one line for a person choosing between them.
	Why string
}

const mb = 1 << 20

// The offered set, smallest first.
//
// # Floor was measured by killing the process
//
// Each profile's refresh was run against the real datasets inside a
// container, five times at each size, and the floor is the smallest size
// where all five survived:
//
//	Dengeli   96m 0/5   112m 0/5   128m 1/5   160m 5/5
//	Tam                            256m 2/5   320m 5/5
//
// The rows that matter are 128m and 256m. Both survived *sometimes* -
// and the first measurement taken here was a single run at 256m that
// passed, which would have set the budget a hundred megabytes too low
// on the strength of one lucky toss. A profile whose floor is a coin
// flip has no floor.
var all = []Profile{
	{
		ID: "hafif", Label: "Hafif", Level: LevelOff,
		Held: 0, Peak: 0,
		// Nothing is loaded, so this is the Go runtime with a refresh
		// that returns immediately. Not measured against a container the
		// way the other two were, because there is nothing here to
		// measure - it is the floor of any Go process at all.
		Floor: 32 * mb,
		Why: "IP zekâsı kapalı. Ülke ve ASN kırılımı yok; " +
			"parmak izi, skorlama ve trafik sayıları çalışmaya devam eder.",
	},
	{
		ID: "dengeli", Label: "Dengeli", Level: LevelCountry,
		Held: 59 * mb, Peak: 111 * mb, Floor: 160 * mb,
		Why: "Ülke verisi var, ASN yok. Ülke engelleme çalışır; " +
			"ASN kırılımı ve ASN'e dayanan skorlama çalışmaz.",
	},
	{
		ID: "tam", Label: "Tam Crucible", Level: LevelFull,
		Held: 136 * mb, Peak: 216 * mb, Floor: 320 * mb,
		Why: "Her iki veri kümesi de yüklü. Ülke ve ASN kırılımının tamamı.",
	},
}

// All returns the offered profiles, smallest first.
func All() []Profile { return append([]Profile(nil), all...) }

// Match names the profile a set of values *is*.
//
// The derivation the package doc describes: given what a deployment is
// actually configured to do, this says what to call it. Nothing stores
// the answer.
func Match(level Level) (Profile, bool) {
	for _, p := range all {
		if p.Level == level {
			return p, true
		}
	}
	return Profile{}, false
}

// ByID looks a profile up by the id a form submitted.
func ByID(id string) (Profile, bool) {
	for _, p := range all {
		if p.ID == id {
			return p, true
		}
	}
	return Profile{}, false
}

// bytesPerTrackedAddress is the rate store's cost per entry.
//
// Measured at 142, 132 and 164 bytes per address across ten thousand, a
// hundred thousand and five hundred thousand of them; 160 is the top of
// that range rounded up, for the same reason Peak takes the upper end.
const bytesPerTrackedAddress = 160

// RateStoreBytes is what the rate store can hold at its fullest.
//
// Not a guess about traffic: the map only ever holds addresses from
// *admitted* connections, and the limiter admits at most
// requestsPerSecond of them each second, and an entry is dropped once it
// has been idle for ttlSeconds. So the ceiling is the product, and it is
// a property of two settings rather than of how popular the site is.
//
// A zero or negative limit means unlimited, which is a real
// configuration - and one where no ceiling can be computed. The caller
// gets zero and has to decide what to do with "unbounded" rather than
// being handed a number that looks like a bound.
func RateStoreBytes(requestsPerSecond, ttlSeconds int) (bytes uint64, bounded bool) {
	if requestsPerSecond <= 0 || ttlSeconds <= 0 {
		return 0, false
	}
	return uint64(requestsPerSecond) * uint64(ttlSeconds) * bytesPerTrackedAddress, true
}

// collectorOverhead is what the real collector has and the process the
// floor was measured in did not: the pgx pool, the connection buffers,
// the known-bot fingerprint set, the log tree, the heartbeat.
//
// An allowance, not a measurement, and said so plainly rather than
// dressed up. It deliberately does not include the Go runtime, which the
// measured floor already carries - counting it twice would refuse
// profiles that run perfectly well, and a check that cries wolf is one
// people route around.
const collectorOverhead = 48 * mb

// Needs is what this profile wants, in bytes.
//
// Floor rather than Peak. Peak is a heap figure sampled every couple of
// milliseconds; what kills a container is an allocation the kernel
// cannot satisfy, and the distance between those two is the runtime's
// own reservations. Floor was obtained by running the thing under a
// limit until it stopped dying, which is the question actually being
// asked.
func (p Profile) Needs(rateStore uint64) uint64 {
	return p.Floor + rateStore + collectorOverhead
}

// Fits reports whether a profile can run under a ceiling, and says why
// not in a sentence an operator can act on.
//
// An unknown ceiling fits. That is deliberate and it is the same
// judgement as everywhere else in this system: refusing to proceed
// because a file could not be read turns an unreadable /proc into an
// outage, and this check exists to prevent an outage rather than to
// cause one.
func (p Profile) Fits(ceiling memlimit.Limit, rateStore uint64, bounded bool) (ok bool, why string) {
	if !ceiling.Known() {
		return true, ""
	}
	needs := p.Needs(rateStore)
	if needs <= ceiling.Bytes {
		return true, ""
	}

	unbounded := ""
	if !bounded {
		unbounded = " Ayrıca istek sınırı kapalı olduğu için hız deposunun " +
			"tavanı hesaplanamıyor; bu rakam onsuz."
	}
	return false, fmt.Sprintf(
		"%s profili en kötü durumda ~%d MB istiyor, bu kurulumda ise ~%d MB var (%s). "+
			"Daha küçük bir profil seçin ya da belleği artırın.%s",
		p.Label, needs/mb, ceiling.Bytes/mb, sourceInTurkish(ceiling.From), unbounded)
}

// sourceInTurkish names where a ceiling came from, for a message an
// operator reads.
//
// The provenance is shown rather than hidden because the two are worth
// different amounts: a container limit is exact and belongs to this
// process, while free memory on a shared machine is an estimate the
// database next door can invalidate. An operator who sees "boş bellek"
// knows to look at what else is running; one who sees only a number does
// not.
func sourceInTurkish(s memlimit.Source) string {
	switch s {
	case memlimit.SourceCgroupV2, memlimit.SourceCgroupV1:
		return "konteyner bellek sınırı"
	case memlimit.SourceAvailable:
		return "makinenin şu an boş belleği"
	default:
		return "bilinmiyor"
	}
}
