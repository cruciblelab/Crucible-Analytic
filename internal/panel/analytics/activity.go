package analytics

import (
	"context"
	"net/url"
	"time"
)

// BeaconActivity is when one site last sent anything, and how much in
// the hour before AsOf (B3g).
//
// LastEvent is nil for a site that has never sent an event - an answer,
// not a failure, and the one the page has to tell apart from "sent
// nothing lately".
type BeaconActivity struct {
	LastEvent *time.Time `json:"last_event"`
	LastHour  int64      `json:"last_hour"`
	// AsOf is the database's clock when it answered. "How long ago" is
	// computed against it, never against the panel's own clock: the
	// events were stamped by the database, and two machines' clocks are
	// two answers (the B3e lesson).
	AsOf time.Time `json:"as_of"`
}

// Ago is how long before AsOf the last event arrived; false when there
// was none.
func (a BeaconActivity) Ago() (time.Duration, bool) {
	if a.LastEvent == nil {
		return 0, false
	}
	d := a.AsOf.Sub(*a.LastEvent)
	if d < 0 {
		// An event a little ahead of the clock that read it: the beacon
		// stamps with its own clock before the row lands. "Just now" is
		// the truthful reading of a negative gap, not a future.
		d = 0
	}
	return d, true
}

// BeaconActivity asks the read API when one site last sent anything.
//
// It takes no range - the API ignores from and to on this route, and
// get sends them because every other call does (siteList does the
// same).
func (c *Client) BeaconActivity(ctx context.Context, site string) (BeaconActivity, error) {
	if !c.Configured() {
		return BeaconActivity{}, ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, PageTimeout)
	defer cancel()
	var out BeaconActivity
	now := time.Now()
	err := c.get(ctx, "/api/v1/sites/"+url.PathEscape(site)+"/beacon/activity",
		now.Add(-time.Hour), now, nil, &out)
	return out, err
}
