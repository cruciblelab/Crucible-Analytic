package beacon

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/netip"
	"sync"
	"time"
)

// DefaultSaltPeriod is how long one visitor-ID salt stays in use.
//
// A day, because that is the natural reporting unit: "daily unique
// visitors" is a number the salt period makes exactly correct rather
// than approximately correct, and any longer period would make the
// identifier progressively more like a durable tracking ID.
const DefaultSaltPeriod = 24 * time.Hour

// visitorIDBytes is how much of the HMAC output ends up in the stored
// ID. 16 bytes (32 hex characters) leaves collisions unreachable at any
// visitor count a single site will ever have, while keeping the column
// half the width of a full SHA-256.
const visitorIDBytes = 16

// VisitorIDs derives cookieless, non-durable visitor identifiers.
//
// The problem it solves is the one internal/api cannot: an IP is not a
// visitor. Turkish mobile carriers in particular run CGNAT, collapsing
// very many real people behind one address, while dynamic reassignment
// splits one person across several addresses over time. Counting
// DISTINCT ip therefore both under- and over-counts, in ways that
// cannot be corrected at the IP layer at all.
//
// The construction is the one Plausible popularized:
//
//	visitor_id = HMAC(daily_salt, site_id || ip || user_agent)
//
// with the salt generated randomly at startup, held only in memory, and
// replaced every SaltPeriod. Two people behind the same CGNAT address
// running different browsers get different IDs; the same person's
// requests within a day collapse to one ID even across pages. Because
// the salt is never written anywhere and is discarded on rotation, an
// old ID cannot be re-derived from an IP afterwards even by whoever
// holds the database - which is what keeps this out of "durable
// identifier" territory and is why the beacon needs no cookie and
// therefore no cookie banner.
//
// The cost, stated plainly: restarting the process generates a new salt
// mid-period, so one visitor is counted twice across that restart. The
// alternative - persisting the salt - would make the IDs recoverable
// from a database backup, which is the property this design is spending
// that accuracy to avoid.
//
// Safe for concurrent use.
type VisitorIDs struct {
	// SaltPeriod is how long a salt lives; zero means
	// DefaultSaltPeriod.
	SaltPeriod time.Duration
	// Now supplies the current time; nil means time.Now. Injectable so
	// tests can cross a rotation boundary without waiting a day.
	Now func() time.Time

	mu        sync.Mutex
	salt      []byte
	rotatedAt time.Time
}

// NewVisitorIDs returns a VisitorIDs with its first salt already drawn,
// so the very first request does not pay for the initial randomness.
func NewVisitorIDs() (*VisitorIDs, error) {
	v := &VisitorIDs{}
	if err := v.rotate(time.Now()); err != nil {
		return nil, err
	}
	return v, nil
}

func (v *VisitorIDs) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func (v *VisitorIDs) period() time.Duration {
	if v.SaltPeriod > 0 {
		return v.SaltPeriod
	}
	return DefaultSaltPeriod
}

// rotate draws a fresh salt. The caller must not hold v.mu.
func (v *VisitorIDs) rotate(now time.Time) error {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("beacon: draw visitor salt: %w", err)
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.salt, v.rotatedAt = salt, now
	return nil
}

// currentSalt returns the salt to use now, rotating first if the
// current one has expired. Rotation happens lazily on use rather than
// on a background ticker: a beacon that is receiving no traffic has no
// reason to be waking up, and lazily rotating means a process idle
// across several periods still starts the next one with a fresh salt
// rather than replaying the stale one.
func (v *VisitorIDs) currentSalt(now time.Time) ([]byte, error) {
	v.mu.Lock()
	salt, rotatedAt := v.salt, v.rotatedAt
	v.mu.Unlock()

	if salt != nil && now.Sub(rotatedAt) < v.period() {
		return salt, nil
	}
	if err := v.rotate(now); err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.salt, nil
}

// ID derives the visitor identifier for one request.
//
// An error means the system's randomness source failed while rotating,
// which is not something the caller can paper over: the alternative -
// falling back to a fixed or empty salt - would silently turn every ID
// into a plain hash of (site, ip, user agent), i.e. a durable
// cross-day identifier, which is precisely what this type exists to
// prevent. Callers should drop the event instead.
func (v *VisitorIDs) ID(siteID string, ip netip.Addr, userAgent string) (string, error) {
	salt, err := v.currentSalt(v.now())
	if err != nil {
		return "", err
	}

	mac := hmac.New(sha256.New, salt)
	// Length-framed rather than concatenated or separator-joined: with
	// plain concatenation, site "ab" + IP "1.2.3.4" and site "a" +
	// IP "b1.2.3.4" would hash identically. No separator byte can be
	// ruled out of a User-Agent header, so framing is the only way to
	// make the encoding unambiguous rather than merely unlikely to
	// collide.
	for _, field := range []string{siteID, visitorNetwork(ip).String(), userAgent} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		mac.Write(length[:])
		mac.Write([]byte(field))
	}

	return hex.EncodeToString(mac.Sum(nil)[:visitorIDBytes]), nil
}

// visitorNetwork reduces an address to the unit that identifies a
// subscriber rather than an interface.
//
// IPv4 is used whole. IPv6 is truncated to its /64 prefix, because
// RFC 8981 privacy extensions - on by default in every major OS -
// rotate the low 64 bits regularly, often daily. Hashing the full
// address would therefore split a single IPv6 visitor into a new
// "unique visitor" every time their operating system rotated its
// temporary address, inflating counts on exactly the modern, mobile
// clients a site most cares about. The /64 is the prefix a subscriber
// is delegated, so it stays put while the interface identifier churns.
func visitorNetwork(ip netip.Addr) netip.Addr {
	ip = ip.Unmap()
	if !ip.Is6() {
		return ip
	}
	prefix, err := ip.Prefix(64)
	if err != nil {
		return ip
	}
	return prefix.Addr()
}
