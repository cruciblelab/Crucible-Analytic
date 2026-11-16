package ratestore

import "time"

// Counter is one sliding window for a caller that keeps its own table of
// them: the beacon's per-address event rate (W2b), which needs two windows
// per address and no fingerprint.
//
// The arithmetic is RecordRequest's - rotatedCounts and windowStatsAt -
// so the collector's rate and the beacon's are one rule about what "n in
// the last window" means, with two window sizes.
//
// The zero value is ready: a window start long past rotates to empty.
type Counter struct {
	prev, curr int
	start      time.Time
}

// Add records one event at now in a window of size and returns how many
// the sliding window holds, this one included: the previous window
// weighted by how much of it still overlaps, plus the current one.
func (c *Counter) Add(now time.Time, size time.Duration) float64 {
	c.prev, c.curr, c.start = rotatedCounts(c.prev, c.curr, c.start, now, size)
	c.curr++
	return windowStatsAt(c.prev, c.curr, c.start, now, size).EstimatedRate * size.Seconds()
}

// Idle reports whether the window would have emptied by now: two whole
// windows since the current one began, so a table can forget it.
func (c Counter) Idle(now time.Time, size time.Duration) bool {
	return now.Sub(c.start) >= 2*size
}
