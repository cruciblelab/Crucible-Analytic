package logging

import (
	"context"
	"log/slog"
	"time"

	"github.com/cruciblelab/crucible-analytic/internal/settings"
)

// LiveLifecycle is the retention policy a service should apply: the
// panel's value where one is stored, the config file's otherwise, field
// by field.
//
// The panel wins because it is where the decision is meant to be made:
// the two retention figures are behind the developer password, each
// with the legal reason written beside it, and a config file that
// silently outranked that would make the page's "saved" a lie. A key
// with no row is not a decision, so the file's number - including a
// negative one, which turns that stage off - stands.
//
// A stored value outside the bounds is treated as no value, the same as
// every other live setting: a row written by hand must not be able to
// hand a service zero, which would mean "never delete".
//
// Here rather than in each service's main, because three services call
// it and the keys have to be named through internal/settings for
// internal/panel's TestSettings_LiveKeysMatchWhatServicesRead to see
// that somebody reads them. That test is how the three settings were
// found unread; this is the one place that answers it.
func LiveLifecycle(live *settings.Source, file Lifecycle) Lifecycle {
	if live == nil {
		return file
	}
	return Lifecycle{
		ArchiveAfterDays: live.Int(settings.KeyLogArchiveAfterDays, "",
			file.ArchiveAfterDays, MinLifecycleDays, MaxLifecycleDays),
		RetentionDays: live.Int(settings.KeyLogRetentionDays, "",
			file.RetentionDays, MinLifecycleDays, MaxLifecycleDays),
		ImportantRetentionDays: live.Int(settings.KeyLogImportantRetentionDays, "",
			file.ImportantRetentionDays, MinLifecycleDays, MaxLifecycleDays),
	}
}

// KeepLive is what a service with live settings calls each time it
// applies them: the policy from LiveLifecycle, kept by Controls.Keep,
// and one line about what the pass did.
//
// The line is written only when a pass ran and changed something, or
// failed. A pass that found nothing to do is the ordinary hourly case and
// a line for it would be noise somebody learns to skip. INFO for the
// change rather than WARN: deleting what the policy says to delete is the
// feature working.
//
// # Before the settings have ever been read
//
// The policy is not known, and the file's numbers are not a stand-in for
// it: the panel may have lengthened a retention, and the file's shorter
// figure would delete what the owner said to keep. So until the table
// has been read once, the pass compresses and deletes nothing - archiving
// leaves every line readable, deletion cannot be undone. A service that
// cannot reach its database for days still keeps its disk to a tenth;
// what it does not do is make a legal decision on a guess.
func KeepLive(c *Controls, live *settings.Source, logger *slog.Logger, now time.Time) {
	if live == nil || !live.Loaded() {
		keepAndSay(c, Unknown(c.FileLifecycle()), logger, now)
		return
	}
	keepAndSay(c, LiveLifecycle(live, c.FileLifecycle()), logger, now)
}

// Unknown is the policy for a service that does not yet know the one in
// force: the file's archiving, and no deletion. Zero turns a retention
// off; see Maintain.
func Unknown(file Lifecycle) Lifecycle {
	file.RetentionDays, file.ImportantRetentionDays = 0, 0
	return file
}

// keepAndSay is Keep and the one line about it, for KeepLive and
// KeepFile alike.
func keepAndSay(c *Controls, policy Lifecycle, logger *slog.Logger, now time.Time) {
	report, ran, err := c.Keep(policy, now)
	switch {
	case err != nil:
		logger.Warn("logging: maintenance failed", "err", err,
			"archive_days", policy.ArchiveAfterDays, "retention_days", policy.RetentionDays,
			"important_days", policy.ImportantRetentionDays)
	case ran && (report.Archived > 0 || report.Deleted > 0):
		logger.Info("logging: maintenance", "archived", report.Archived, "deleted", report.Deleted,
			"reclaimed_bytes", report.BytesReclaimed,
			"archive_days", policy.ArchiveAfterDays, "retention_days", policy.RetentionDays,
			"important_days", policy.ImportantRetentionDays)
	}
}

// KeepFile keeps the tree under the config file's policy until ctx ends,
// for a service that has no settings to read.
//
// The analytics API is that service: its role is not granted
// panel_settings. Before W4a it, like every other service, maintained its
// tree once at startup and never again - so an API that ran for a year
// kept a year of access logs, whatever its own file said. The policy is
// the file's, and the clock is MaintainEvery.
//
// The first pass is at once: for these services the file is the whole
// policy, so there is nothing to wait for.
func KeepFile(ctx context.Context, c *Controls, logger *slog.Logger) {
	KeepFileOnce(c, logger)
	ticker := time.NewTicker(MaintainEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			keepAndSay(c, c.FileLifecycle(), logger, now)
		}
	}
}

// KeepFileOnce is one pass under the config file's policy, for a process
// that does its work and exits - the upgrader under its timer.
func KeepFileOnce(c *Controls, logger *slog.Logger) {
	keepAndSay(c, c.FileLifecycle(), logger, time.Now())
}
