// Package ratestore tracks per-IP request activity using a cheap
// sliding-window counter rather than a full request log, so memory use
// stays flat regardless of traffic volume.
package ratestore

import (
	"net/netip"
	"time"
)

// WindowStats is the sliding-window state for one IP, returned after
// recording a request and used as scoring input.
type WindowStats struct {
	// PrevWindowCount is the number of requests seen in the window
	// immediately before the current one.
	PrevWindowCount int
	// CurrWindowCount is the number of requests seen in the current
	// window so far (includes the request that was just recorded).
	CurrWindowCount int
	// EstimatedRate is the approximate requests/second implied by the
	// weighted sliding window (Cloudflare-style sliding window counter):
	// prevCount*weight(elapsed) + currCount, divided by the window size.
	EstimatedRate float64
}

// Snapshot is a point-in-time view of one IP's tracked state, used when
// flushing periodic summaries to storage.
type Snapshot struct {
	IP  netip.Addr
	JA4 string
	WindowStats
	LastSeen time.Time
}

// RateStore tracks per-IP request activity and is safe for concurrent use.
//
// The in-memory implementation (MemoryRateStore) is the only one needed
// for the MVP — a single collector process, no Redis — but call sites
// depend on this interface rather than the concrete type so a distributed
// implementation can be swapped in later without touching callers.
type RateStore interface {
	// RecordRequest registers one request from ip (with its associated
	// JA4 fingerprint, which may be empty for non-TLS or unparseable
	// traffic) at time now, and returns the updated window statistics.
	RecordRequest(ip netip.Addr, ja4 string, now time.Time) WindowStats

	// Snapshot returns the current state of every IP last seen at or
	// after since, computing EstimatedRate as of now. Intended for
	// periodic persistence, not per-request use.
	Snapshot(since, now time.Time) []Snapshot
}
