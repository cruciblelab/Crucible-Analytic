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
	// IPFull stores the whole address.
	IPFull IPMode = "full"
	// IPMasked stores IPv4 to /24 and IPv6 to /64.
	IPMasked IPMode = "masked"
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
	case IPHashed:
		return IPHashed
	default:
		return DefaultIPMode
	}
}

// String makes IPMode printable for logs and config dumps.
func (m IPMode) String() string { return string(m) }

// Masks reports whether this mode reduces the stored address.
func (m IPMode) Masks() bool { return m != IPFull }

// Hashes reports whether this mode stores a pseudonym instead of an
// address. In hashed mode the address column is left empty entirely.
func (m IPMode) Hashes() bool { return m == IPHashed }

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

// --- Hashed mode ---
//
// A third mode, added on legal advice: mask the address, then replace it
// with a keyed hash, so what reaches the disk is a pseudonym rather than
// an address at all.
//
// # What this actually protects against, stated exactly
//
// It protects the database. A stolen backup, an imaged disk, a SQL
// injection, a compromised read-only API - none of them yield an
// address, because none of them include the key.
//
// It does *not* make the addresses unknowable to whoever holds the key.
// This is worth stating plainly rather than letting it be assumed: an
// IPv4 /24 network has about 16.7 million possible values, and trying
// every one of them against a known key takes a fraction of a second on
// an ordinary laptop. The hash is not a one-way door for anybody who has
// both halves.
//
// So the honest claim is: "an address cannot be recovered from the data
// alone", not "nobody can ever recover it". Anyone relying on the
// stronger claim - in a privacy notice, or in advice from counsel -
// should be told which of the two they actually have. The key lives in
// the same config file as the database password, so the party who can
// reverse it is exactly the party who could already read everything.
//
// # Why the join still works
//
// Hashing preserves equality, which is all the crossover join needs.
// Two processes hashing the same masked address with the same key
// produce the same pseudonym, so beacon_events and traffic_snapshots
// still line up - at the same /24 resolution masked mode gives, and with
// no address on either side.

// IPHashed masks the address and then replaces it with a keyed hash.
const IPHashed IPMode = "hashed"

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

// HashIP turns an address into the pseudonym stored in hashed mode.
//
// The address is masked first, so two visitors on one /24 hash alike -
// the resolution is exactly what masked mode gives, and the mode adds
// only the property that the value is no longer an address.
//
// An empty key returns nil rather than hashing with nothing. A caller
// that stored the result would be storing a value derived from a
// publicly known function of the address, which is worse than useless:
// it would look like a pseudonym and reverse in microseconds.
func HashIP(ip netip.Addr, key []byte) []byte {
	if len(key) < MinHashKeyLen || !ip.IsValid() {
		return nil
	}
	masked := MaskIP(ip, IPMasked)
	if !masked.IsValid() {
		return nil
	}

	// The 16-byte form for both families, so an IPv4 address and its
	// IPv4-in-IPv6 spelling cannot hash differently - the two writers
	// would otherwise fail to join, for a reason nothing would report.
	full := masked.As16()

	mac := hmac.New(sha256.New, key)
	mac.Write(full[:])
	return mac.Sum(nil)[:HashLen]
}
