package relupdate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cruciblelab/crucible-analytic/internal/heartbeat"
)

// Restarting the services, without the upgrader being able to.
//
// # The problem
//
// Replacing the binaries does not replace the running processes. Linux
// keeps the inode a process opened, so the four services go on serving
// the old version until something restarts them. Until now that
// something was a person, told by the panel to run systemctl by hand.
//
// The obvious fix is to let the upgrader run systemctl. That is also
// the wrong fix: a process that can restart services is a process that
// can stop them, and the upgrader is the component that fetches things
// over the network from an address in a config file. Handing it the
// power to stop the customer's website is handing it to whatever
// reaches it.
//
// # The doorbell
//
// So the upgrader gets no such power. It creates a file. A systemd
// .path unit notices, and a oneshot .service - running as root, with
// its command written into the unit file by whoever installed it -
// restarts the four units.
//
// The file's *contents are never read*. It is a doorbell, not an
// instruction: no unit names, no paths, no version, nothing the script
// acts on. So the worst an attacker who owned the upgrader could do
// through this channel is cause a restart, which is a thing systemd
// already lets anybody with the machine do.
//
// That is the whole security argument, and it is why the request
// carries nothing. A file naming what to restart would be a file naming
// what to run.
//
// # And what proves it worked
//
// Not "the process is up". A collector that starts, listens and cannot
// reach the database passes every check of that shape, and passes it
// for as long as nobody asks it to do anything.
//
// The heartbeat is the honest signal: each service writes a row when it
// starts and every minute after, and writing it requires the database.
// So a service that has written a row *newer than the restart* has
// started, connected and done its job once. Measured: the reporter
// writes immediately on start rather than after its first tick, so a
// healthy service produces that row within a second or two - which is
// what makes an automatic rollback fast enough to be worth having.

// DoorbellName is the file the upgrader creates to ask for a restart.
const DoorbellName = "restart-please"

// DefaultDoorbellDir is where it goes. A tmpfs path, because the
// request is meaningless after a reboot: a machine that has just booted
// is already running the new binaries.
const DefaultDoorbellDir = "/run/crucible-analytic"

// HealthServices are the services whose heartbeat has to come back,
// named the way the heartbeat names them.
//
// # These are database roles, not systemd units
//
// A service identifies itself in service_heartbeat by the role it
// connects as - internal/heartbeat asks the connection who it is - so
// the beacon is "beacon_writer" here and "crucible-beacon" to systemd.
// Two namespaces for four things, and writing the wrong one produces a
// check that waits thirty seconds for a row that will never appear and
// then rolls back a release that was fine.
//
// The systemd names are deliberately absent from this file. They live
// in the restarter's own unit, where root put them, and nothing on this
// side sends them: see Ring. Two lists that must agree would be one
// list too many, and the one that could be edited from here is the one
// that must not exist.
var HealthServices = []string{"collector", "beacon_writer", "analytics_reader", "panel_user"}

// HealthWindow is how long a service has to prove it came back.
//
// Thirty seconds against a signal that a healthy service produces in
// one or two: the heartbeat reporter writes on start, before its first
// tick. The margin is for a machine under load and a database that has
// just had four clients reconnect at once, not for a service that is
// slow to be healthy - there is no such thing here.
const HealthWindow = 30 * time.Second

// Doorbell asks for a restart and reports whether the services came
// back.
type Doorbell struct {
	// Dir is where the request file goes. Empty means
	// DefaultDoorbellDir.
	Dir string
	// Pool reads the heartbeat rows.
	Pool *pgxpool.Pool
	// Window is how long to wait. Zero means HealthWindow.
	Window time.Duration
	// Now measures how long Healthy has been waiting. This machine's
	// clock, and only ever used for a duration - the moment a restart
	// was asked for comes from Since, off the database. A test sets it
	// to a skewed clock to prove the two are not confused again.
	Now func() time.Time
	// Poll is how often the heartbeat is re-read. Zero means a second.
	Poll time.Duration
}

// Configured reports whether a restarter could be listening.
//
// The directory rather than the file: the file is created by this code
// and consumed by the unit, so its absence proves nothing. The
// directory is created by the unit's installation, so its absence means
// nobody installed one - and a deployment with no restarter is the
// ordinary case, not a fault.
func (d Doorbell) Configured() bool {
	info, err := os.Stat(d.dir())
	return err == nil && info.IsDir()
}

// Ring asks for the restart.
//
// Creating the file, not writing to it: the content is never read, and
// a file whose content nothing reads should not have any. Truncated
// each time so a .path unit watching for modification fires again.
func (d Doorbell) Ring() error {
	if !d.Configured() {
		return fmt.Errorf("relupdate: no restarter is installed (%s does not exist); "+
			"the binaries are in place and the services are still running the previous "+
			"ones until somebody restarts them", d.dir())
	}
	path := filepath.Join(d.dir(), DoorbellName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("relupdate: asking for a restart: %w", err)
	}
	return f.Close()
}

// Since is the moment the restart is being asked for, read from the
// database's own clock.
//
// # Why not time.Now
//
// beat_at is written by the database: the heartbeat's INSERT says
// now(), not a timestamp the service chose. So comparing it against the
// upgrader's clock is a comparison between two clocks, and what that
// comparison decides is whether to undo a release.
//
// A database a few seconds ahead makes every row that was already there
// look newer than the restart, so a release whose services never came
// back is accepted and the escape never fires. A few seconds behind and
// the opposite: healthy services report, their rows look old, and a
// working release is rolled back. Neither failure looks like a clock
// problem from the outside - one looks like the rollback being broken,
// the other like the release being broken.
//
// The two processes are usually on one machine, where the clocks agree
// by construction. They are not required to be: the DSN can point
// anywhere, and this is the check that stops being correct when it
// does.
//
// One clock, and it is the one that writes the rows being read.
func (d Doorbell) Since(ctx context.Context) (time.Time, error) {
	var at time.Time
	if err := d.Pool.QueryRow(ctx, `SELECT now()`).Scan(&at); err != nil {
		return time.Time{}, fmt.Errorf("relupdate: reading the database's clock, which is "+
			"what the heartbeat is timed by: %w", err)
	}
	return at, nil
}

// Healthy waits for every service to write a heartbeat newer than
// since, and reports which ones did not.
//
// since must come from Since, not from this machine; see there.
//
// Returns the names that failed rather than a bare error, because the
// sentence an operator needs is "the collector did not come back" and
// not "the restart failed".
func (d Doorbell) Healthy(ctx context.Context, since time.Time) ([]string, error) {
	deadline := d.now().Add(d.window())
	poll := d.poll()

	var missing []string
	for {
		rows, err := heartbeat.Read(ctx, d.Pool)
		if err != nil {
			return nil, fmt.Errorf("relupdate: reading the heartbeat: %w", err)
		}
		seen := map[string]time.Time{}
		for _, r := range rows {
			seen[r.Service] = r.BeatAt
		}

		missing = missing[:0]
		for _, name := range HealthServices {
			at, ok := seen[name]
			if !ok || !at.After(since) {
				missing = append(missing, name)
			}
		}
		if len(missing) == 0 {
			return nil, nil
		}
		if !d.now().Before(deadline) {
			// A copy, because missing is reused above and the caller
			// keeps this.
			out := append([]string(nil), missing...)
			return out, nil
		}

		select {
		case <-ctx.Done():
			return append([]string(nil), missing...), ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (d Doorbell) dir() string {
	if d.Dir != "" {
		return d.Dir
	}
	return DefaultDoorbellDir
}

func (d Doorbell) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Doorbell) window() time.Duration {
	if d.Window > 0 {
		return d.Window
	}
	return HealthWindow
}

func (d Doorbell) poll() time.Duration {
	if d.Poll > 0 {
		return d.Poll
	}
	return time.Second
}
