// Package privacy holds the decisions about what personal data reaches
// the disk, and the code that carries them out.
//
// It is one small package rather than a helper in each writer because
// both writers have to agree exactly. The collector writes
// traffic_snapshots.ip and the beacon writes beacon_events.ip, and the
// join between those two columns is this project's distinguishing claim
// (see the README). If the two masked an address even slightly
// differently - one zeroing the last octet, the other rounding to /25 -
// the join would return nothing, and it would return nothing quietly:
// no error, no log line, just a crossover view that had always shown
// numbers and now shows zero.
package privacy

import (
	"crypto/hmac"
	"crypto/sha256"
	"net/netip"
)

// IPMode is how much of an address is stored.
type IPMode string

const (
	// IPMasked is the default: the address is reduced to its network -
	// IPv4 /24, IPv6 /64 - and that is all that is stored. No key is
	// involved and none is needed; a /24 identifies a network, not a
	// subscriber.
	IPMasked IPMode = "masked"
	// IPFull keeps full precision without keeping the address.
	//
	// The stored row carries the *masked* address, exactly as in masked
	// mode, plus a keyed token derived from the whole one. So the raw
	// address still never reaches the disk - what full mode adds is the
	// ability to tell two visitors inside one /24 apart, which is what
	// the crossover join and the per-address views actually need.
	//
	// This is the mode that requires a key, and the only one. Switching
	// into it is a deliberate act: it needs the developer password like
	// any legally weighted setting, and it needs the key to have been
	// put in the config file beforehand, by somebody with a shell.
	IPFull IPMode = "full"
)

// DefaultIPMode is masked, on legal advice.
//
// The default matters more than the option does. Whoever installs this
// will not read every setting, and the value they never look at is the
// one that ends up in production - so the value they never look at has
// to be the one that stores less.
const DefaultIPMode = IPMasked

// The prefix lengths kept in masked mode.
//
// IPv4 to /24 is the conventional choice and the one Google Analytics
// popularised: it removes the host and keeps the network, which is
// enough to tell an office from a mobile carrier and not enough to
// single out a subscriber.
//
// IPv6 to /64 rather than something longer because /64 is the smallest
// unit an ISP hands out in practice - usually one per household or per
// device. Anything narrower would keep an identifier for one machine,
// which is what the masking is for. It also matches what the visitor id
// already does with IPv6, so the two agree about what "the same network"
// means.
const (
	maskedIPv4Bits = 24
	maskedIPv6Bits = 64
)

// ParseIPMode turns a stored setting value into a mode.
//
// Anything unrecognised becomes the default rather than an error. This
// is read on the hot path from a settings table that a service does not
// control, and the failure that must not happen is a typo turning
// masking off: an unreadable value has to fall to the safer side, not to
// whatever was easiest to code.
func ParseIPMode(value string) IPMode {
	switch IPMode(value) {
	case IPFull:
		return IPFull
	case IPMasked:
		return IPMasked
	default:
		return DefaultIPMode
	}
}

// String makes IPMode printable for logs and config dumps.
func (m IPMode) String() string { return string(m) }

// Masks reports whether the stored address is reduced to its network.
//
// Always, in both modes. It stayed a method rather than becoming a
// constant because MaskIP's callers read better for it, and because a
// future mode that stores something else will have to answer this
// question again rather than inheriting an assumption.
func (m IPMode) Masks() bool { return true }

// Tokenises reports whether this mode stores a keyed token alongside the
// masked address, and therefore whether it needs a key at all.
//
// Only full mode does. Masked mode is the default and works on any
// deployment with nothing configured, which is what lets the safe option
// be the effortless one.
func (m IPMode) Tokenises() bool { return m == IPFull }

// MaskIP applies the mode to an address.
//
// # Where this must be called, and where it must not
//
// At the moment a row is built, and as the *last* step. The whole
// address has work to do first: it derives the cookieless visitor id,
// and it resolves country and ASN. Masking before either would degrade
// both - visitor counts would collapse people who share a /24, and
// geography would resolve the network's registration rather than the
// visitor's - and neither degradation announces itself. The numbers
// would simply be different, with nothing to say why.
//
// After that work, the full address has no further use, and the value
// that reaches the disk is this one. There is no separate masking pass
// over stored rows because there is no window in which an unmasked row
// exists to be passed over.
//
// An invalid address is returned unchanged: it cannot be stored anyway,
// and inventing a masked value for it would be inventing data.
func MaskIP(ip netip.Addr, mode IPMode) netip.Addr {
	if !mode.Masks() || !ip.IsValid() {
		return ip
	}

	// IPv4-in-IPv6 (::ffff:1.2.3.4) is unwrapped first. Without this it
	// would take the IPv6 path and keep its whole 32-bit address inside
	// a /64 that already covers it - masked in name only.
	if ip.Is4In6() {
		ip = ip.Unmap()
	}

	bits := maskedIPv6Bits
	if ip.Is4() {
		bits = maskedIPv4Bits
	}

	prefix, err := ip.Prefix(bits)
	if err != nil {
		// Unreachable for a valid address with a bit count this package
		// controls. Returning the address unchanged would be the wrong
		// way to fail here - it would store the whole thing - so fall
		// back to the zero address, which is refused downstream rather
		// than stored.
		return netip.Addr{}
	}
	return prefix.Addr()
}

// --- The keyed token, and what it is honestly worth ---
//
// # No mode stores a raw address
//
// That is the rule the two modes are built around. Masked mode stores
// the network and nothing else. Full mode stores the same network plus a
// keyed token of the whole address - so it recovers the precision the
// crossover join wants without the address itself ever reaching disk.
//
// # What the token protects against, stated exactly
//
// It protects the database. A stolen backup, an imaged disk, a SQL
// injection, a compromised read-only API - none of them yield an
// address, because none of them include the key.
//
// It does *not* put the addresses beyond the reach of whoever holds the
// key. Worth stating plainly rather than letting it be assumed: the row
// already carries the /24, so anyone with the key has 256 candidates to
// try, not four billion. The token is a lock on the data, not a one-way
// door for the party holding both halves.
//
// So the honest claim is "an address cannot be recovered from the data
// alone", never "nobody can ever recover it". Anybody relying on the
// stronger sentence - in a privacy notice, or in advice from counsel -
// has to be told which of the two they actually have.
//
// # Why the join still works, and gets sharper
//
// Tokenising preserves equality, which is all the crossover join needs.
// Two processes tokenising the same address with the same key produce
// the same value, so beacon_events and traffic_snapshots line up - and
// in full mode they line up at whole-address precision rather than /24,
// which is exactly what that mode is for.

// HashLen is the stored pseudonym's length in bytes.
//
// 16 rather than the full 32: enough that two different networks
// colliding is not a practical concern at any volume this project will
// see, and half the storage on a column that appears in every row of the
// largest tables.
const HashLen = 16

// MinHashKeyLen bounds the configured key.
//
// Thirty-two bytes because a short key is the one that makes brute force
// easy in the direction that matters: not guessing the address, but
// guessing the key from a known address-and-hash pair, which anybody who
// can visit the site can produce for themselves.
const MinHashKeyLen = 32

// TokenIP turns a whole address into the keyed token stored in full
// mode, and returns nil in every other mode's circumstances.
//
// The *whole* address goes in, not the masked one. That is the point of
// full mode: the token distinguishes two visitors inside one /24, which
// the masked address in the same row cannot. The row therefore carries
// a coarse address anyone may read and a precise token nobody may
// reverse without the key - and no raw address at all.
//
// An absent or short key returns nil rather than tokenising anyway. A
// caller that stored a weakly keyed value would be storing something
// that looks like a token and reverses in microseconds, which is worse
// than storing nothing, because it would be believed.
func TokenIP(ip netip.Addr, key []byte) []byte {
	if len(key) < MinHashKeyLen || !ip.IsValid() {
		return nil
	}
	whole := ip
	if whole.Is4In6() {
		whole = whole.Unmap()
	}

	// The 16-byte form for both families, so an IPv4 address and its
	// IPv4-in-IPv6 spelling cannot tokenise differently - the two
	// writers would otherwise fail to join, for a reason nothing would
	// report.
	full := whole.As16()

	mac := hmac.New(sha256.New, key)
	mac.Write(full[:])
	return mac.Sum(nil)[:HashLen]
}
