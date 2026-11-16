// Package verdict carries the collector's opinion of a request to the
// beacon, in a header the beacon can check was written by the collector.
//
// # Why there is a header at all (W2)
//
// The beacon sees an event: a path, a title, a User-Agent. It cannot see
// what the collector sees in full-proxy mode - the TLS fingerprint of the
// connection and how fast that address has been asking. Measured on the
// live rig (2026-11-03): 303 page views sent with curl from one address
// were counted as people. The collector had already scored that address
// 74 (a known-bot fingerprint and a rate), and nothing told the beacon.
//
// # Why it is signed
//
// The header travels collector -> the site's web server -> beacon, so the
// beacon cannot tell the collector's header from one a visitor wrote: in
// the documented layout where the beacon has its own address, the
// visitor reaches the web server directly and the server forwards
// whatever it was sent. Trusting the peer would trust the visitor.
//
// So the value is a MAC over the score, the time and the visitor's
// address, keyed by a key both services already share
// (privacy.ip_hash_key), derived for this one purpose so a value written
// here can never be confused with an address token. The address in the
// MAC means a header copied from one visitor's request does not hold on
// another's; the time bounds how long one holds at all.
//
// # What the outcomes mean
//
// Only Verified carries a score. Everything else that was present is a
// header the collector did not write for this request, and the beacon
// treats it as such - see the beacon's use. Absent means "nobody said":
// not "a person". The beacon's own rate ceiling still applies to it.
package verdict

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/privacy"
	"github.com/cruciblelab/crucible-analytic/internal/scoring"
)

// Header is the name the collector writes and the beacon reads. The
// collector deletes any copy the visitor sent before writing its own.
const Header = "X-Crucible-Verdict"

// version leads every value, so a change of format is a value the old
// reader refuses as malformed rather than misreads.
const version = "v1"

// purpose labels the derived key. A MAC under privacy.ip_hash_key itself
// would share a key with the address tokens stored on disk; deriving one
// per purpose keeps the two from ever being interchangeable.
const purpose = "crucible-analytic beacon verdict v1"

// MaxSkew is how far the header's time may be from the beacon's clock.
//
// The header is written and read within one forwarded request, so its
// age is milliseconds. Five minutes is not that age: it is room for two
// machines' clocks to disagree when the collector and the beacon run on
// different hosts, which is a deployment this product supports and one
// nobody would think to check the clocks of. The address in the MAC,
// not the time, is what stops a copied header.
const MaxSkew = 5 * time.Minute

// Key derives the verdict key from the shared address key, or returns nil
// when that key is not one privacy.CanTokenise accepts. A nil key means
// "no verdicts": the collector writes none, the beacon checks none.
func Key(ipHashKey []byte) []byte {
	if !privacy.CanTokenise(ipHashKey) {
		return nil
	}
	m := hmac.New(sha256.New, ipHashKey)
	m.Write([]byte(purpose))
	return m.Sum(nil)
}

// Sign writes the header value for one request: the score the collector
// gave addr at the moment at.
func Sign(key []byte, score int, at time.Time, addr netip.Addr) string {
	ip := addr.Unmap().String()
	unix := strconv.FormatInt(at.Unix(), 10)
	s := strconv.Itoa(score)
	return version + " " + s + " " + unix + " " + ip + " " + mac(key, s, unix, ip)
}

func mac(key []byte, score, unix, ip string) string {
	m := hmac.New(sha256.New, key)
	// NUL-separated: no field may contain one, so no two different
	// field lists can produce the same input.
	m.Write([]byte(version + "\x00" + score + "\x00" + unix + "\x00" + ip))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Outcome is what Check concluded about a header.
type Outcome int

const (
	// Absent: no header. Nobody said anything about this request.
	Absent Outcome = iota
	// Verified: the collector wrote this header for this address within
	// MaxSkew. The score is its opinion.
	Verified
	// Malformed: present, but not a value the collector writes.
	Malformed
	// BadSignature: well formed, but the MAC does not hold under the
	// shared key. Written by someone without the key - or by a collector
	// configured with a different one.
	BadSignature
	// Stale: the MAC holds, but the time is further than MaxSkew from now.
	Stale
	// OtherAddress: the MAC holds, for a different address than the one
	// this request came from. Either a header copied between visitors or
	// a beacon resolving visitors' addresses differently from the
	// collector (trusted_proxies) - the beacon's Health counter names
	// the second, because it is by far the likelier.
	OtherAddress
)

// Check reads a header value for a request from addr at now.
//
// The order is the order of cost to forge: a value that does not parse
// is Malformed whatever else is wrong with it; one whose MAC fails is
// BadSignature before its time or address is looked at, so those two
// outcomes are only ever reported about values the collector really
// wrote.
func Check(key []byte, header string, addr netip.Addr, now time.Time) (int, Outcome) {
	if header == "" {
		return 0, Absent
	}
	f := strings.Split(header, " ")
	if len(f) != 5 || f[0] != version {
		return 0, Malformed
	}
	score, err := strconv.Atoi(f[1])
	if err != nil || score < 0 || score > scoring.MaxScore || strconv.Itoa(score) != f[1] {
		return 0, Malformed
	}
	unix, err := strconv.ParseInt(f[2], 10, 64)
	if err != nil || strconv.FormatInt(unix, 10) != f[2] {
		return 0, Malformed
	}
	signed, err := netip.ParseAddr(f[3])
	if err != nil || signed.String() != f[3] {
		return 0, Malformed
	}
	if !hmac.Equal([]byte(mac(key, f[1], f[2], f[3])), []byte(f[4])) {
		return 0, BadSignature
	}
	at := time.Unix(unix, 0)
	if d := now.Sub(at); d > MaxSkew || d < -MaxSkew {
		return 0, Stale
	}
	if signed != addr.Unmap() {
		return 0, OtherAddress
	}
	return score, Verified
}
