package logging

import (
	"sync"
	"time"
)

// Keeping a running service's tree inside its retention policy.
//
// # What was here before
//
// One maintenance pass, at startup, with the numbers from the service's
// config file. Its comment said "the panel's repair operation covers the
// long-running case explicitly". There was no such operation: nothing
// else in the program called Maintain. A collector that ran for three
// months kept three months of access logs - the category that fills a
// disk, on the process whose full disk stops the traffic path.
//
// And the panel offered the three numbers as settings, behind the
// developer password, with a legal reason written beside each one. No
// service read them. The page said "Ayar kaydedildi" and the files
// stayed. Measured on the real beacon binary: a retention of 2 days
// written to panel_settings, a 3-day-old access log still on the disk
// after the setting had been read, and still there after a restart.
//
// # The shape
//
// Keep is called with the policy in force every time a service applies
// its live settings - once a minute by default. It runs a pass when the
// policy differs from the last pass's, so a change made in the panel
// takes effect on the next refresh, and otherwise once MaintainEvery has
// gone by, so a process that never restarts still ages its files out.
//
// The pass itself is Maintain, unchanged, which never touches the day
// being written to. That is what makes calling it from a running
// process safe; the startup pass relied on the same property.

// MaintainEvery is how often a running service re-applies an unchanged
// policy.
//
// An hour, because the unit of the policy is a day: a file becomes
// eligible at most once a day and an hourly pass deletes it within an
// hour of that. Shorter buys nothing but directory listings.
const MaintainEvery = time.Hour

// The bounds a day count may take, in the panel's registry and in every
// service that reads one. One pair rather than a copy in each place: a
// value the panel accepts and a service then throws away as out of
// range is a setting that says "saved" and does nothing, which is the
// defect this file exists about.
const (
	MinLifecycleDays = 1
	MaxLifecycleDays = 3650
)

// keeper is the state Keep carries between passes.
type keeper struct {
	mu   sync.Mutex
	tree *Tree
	// file is the policy the config file named, with defaults filled.
	file Lifecycle
	// applied and passed describe the last pass, whichever caller ran
	// it. hasPassed is false until one has.
	applied   Lifecycle
	passed    time.Time
	hasPassed bool
}

// FileLifecycle is the policy the service's config file names, with the
// defaults filled in. A live setting overrides it field by field; see
// LiveLifecycle.
func (c *Controls) FileLifecycle() Lifecycle {
	if c == nil || c.keep == nil {
		return DefaultLifecycle()
	}
	return c.keep.file
}

// Keep applies policy to the service's tree if it is due, and reports
// whether a pass ran.
//
// Due means the policy differs from the one the last pass applied, or
// MaintainEvery has gone by since it. A service with no log directory has
// no tree, and Keep never runs anything for it.
//
// Safe to call from the settings refresher while the service logs: the
// pass skips the day being written to, and passes are serialised here.
func (c *Controls) Keep(policy Lifecycle, now time.Time) (MaintenanceReport, bool, error) {
	if c == nil || c.keep == nil || c.keep.tree == nil {
		return MaintenanceReport{}, false, nil
	}
	k := c.keep
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.hasPassed && policy == k.applied && now.Sub(k.passed) < MaintainEvery {
		return MaintenanceReport{}, false, nil
	}
	report, err := k.tree.Maintain(policy)
	// Recorded even when the pass failed part way. A pass that cannot
	// delete one file would otherwise be retried on every refresh - once
	// a minute, each one logging the same failure - and the next attempt
	// an hour later is soon enough for a problem somebody has to fix by
	// hand anyway.
	k.applied, k.passed, k.hasPassed = policy, now, true
	return report, true, err
}
