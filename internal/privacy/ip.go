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
	default:
		return DefaultIPMode
	}
}

// String makes IPMode printable for logs and config dumps.
func (m IPMode) String() string { return string(m) }

// Masks reports whether this mode reduces the stored address.
func (m IPMode) Masks() bool { return m != IPFull }

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
