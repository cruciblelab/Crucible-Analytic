package relupdate

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/releasesign"
)

// The loop that asks, on a schedule nobody has to think about.

// DefaultCheckInterval is how often the upgrader asks the release
// source which version is current.
//
// Six hours, and the number is a division rather than a preference. The
// upgrader's own poll is thirty seconds because a queued request should
// be picked up while somebody is still looking at the page; checking for
// new *releases* at that rate would be 2 880 requests a day to somebody
// else's server for an answer that changes a few times a month.
//
// Six hours is four requests a day and means a release is visible on the
// page within a working day of being published, which is the resolution
// the question actually has: nobody publishes a version and expects a
// customer to have it within the minute.
const DefaultCheckInterval = 6 * time.Hour

// CheckRetryInterval is how soon a failed check is retried.
//
// Sooner than the success interval, because the two failures this is
// for - a host that was briefly down, a machine that had no network
// when it started - both clear in minutes. Waiting six hours to notice
// they cleared would make an outage of one minute cost a day of not
// knowing.
const CheckRetryInterval = 15 * time.Minute

// Checker asks the source and records the answer.
type Checker struct {
	Pool   *pgxpool.Pool
	Source Source
	Logger *slog.Logger
	// Now is time.Now, so a test can place a check on the clock.
	Now func() time.Time
	// Interval and Retry default to the constants above.
	Interval time.Duration
	Retry    time.Duration
}

// Due reports whether a check should run now.
//
// Read from the recorded row rather than from a timer held in memory,
// which is what makes a restart cheap: a service that restarts every
// few minutes because somebody is deploying would, with an in-memory
// timer, check on every start. This one asks the database when it last
// looked, so a hundred restarts produce one check.
func (c Checker) Due(last Available) bool {
	now := c.now()
	if last.CheckedAt.IsZero() {
		return true
	}
	wait := c.interval()
	if last.Error != "" {
		wait = c.retry()
	}
	return now.Sub(last.CheckedAt) >= wait
}

// RunOnce checks if it is due, and records what happened.
//
// It reports an error only when the caller could act on it. A source
// that is not configured, or one whose publisher has no manifest, is a
// deployment this feature does not apply to rather than a fault - and
// an upgrader that logged an error every six hours about a feature
// nobody set up would teach its operator to ignore its log.
func (c Checker) RunOnce(ctx context.Context) error {
	if c.Source.BaseURL == "" || !c.Source.PublicKey.IsSet() {
		return nil
	}

	last, err := ReadAvailable(ctx, c.Pool)
	if err != nil {
		return err
	}
	if !c.Due(last) {
		return nil
	}

	manifest, checkErr := c.Source.CheckLatest(ctx)
	if checkErr != nil {
		// Recorded even when the failure is "there is no manifest": the
		// page has to be able to say "we asked and this source does not
		// publish one" rather than staying blank forever, which is what
		// a deployment with no manifest and no record would look like.
		if recErr := RecordCheckFailure(ctx, c.Pool, checkErr.Error(), c.now()); recErr != nil {
			return recErr
		}
		switch {
		case errors.Is(checkErr, ErrNoManifest):
			// Not an error to the caller. Nothing is wrong here except
			// that the publisher has not started publishing one.
			c.logger().Debug("relupdate: the release source publishes no manifest",
				"base_url", c.Source.BaseURL)
			return nil
		case errors.Is(checkErr, releasesign.ErrBadSignature):
			// This one is loud wherever it appears. A manifest that does
			// not verify is either a misconfigured key or somebody
			// serving a document they did not sign, and the second is
			// the case this entire design exists for.
			c.logger().Error("relupdate: the release manifest did not verify",
				"base_url", c.Source.BaseURL, "err", checkErr)
		default:
			c.logger().Warn("relupdate: could not check for a new version", "err", checkErr)
		}
		return checkErr
	}

	if err := RecordAvailable(ctx, c.Pool, manifest, c.now()); err != nil {
		return err
	}
	c.logger().Info("relupdate: checked for a new version", "latest", manifest.Version)
	return nil
}

func (c Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c Checker) interval() time.Duration {
	if c.Interval > 0 {
		return c.Interval
	}
	return DefaultCheckInterval
}

func (c Checker) retry() time.Duration {
	if c.Retry > 0 {
		return c.Retry
	}
	return CheckRetryInterval
}

func (c Checker) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}
